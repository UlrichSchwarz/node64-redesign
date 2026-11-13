package ipam64

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Network ist ein Netz mit Belegung: ein DHCP-Netz aus DNS64, ein eigenes Netz oder beides (eigene Angaben gehen vor).
type Network struct {
	Prefix     netip.Prefix `json:"-"`
	Subnet     string       `json:"subnet"`
	Name       string       `json:"name"`
	VLAN       int          `json:"vlan"`
	Iface      string       `json:"interface"`
	Router     string       `json:"router"`
	RangeStart string       `json:"range_start"`
	RangeEnd   string       `json:"range_end"`
	Comment    string       `json:"comment"`
	DHCP       bool         `json:"dhcp"`     // DHCP-Netz aus DNS64
	Manual     bool         `json:"manual"`   // eigene Angaben in IPAM64
	Attached   bool         `json:"attached"` // haengt direkt an der Node (Netz-Scan moeglich)
	Size       int          `json:"size"`     // nutzbare Adressen
	Used       int          `json:"used"`
	Free       int          `json:"free"`
	PoolFree   int          `json:"pool_free"` // davon frei im DHCP-Bereich
	Conflicts  int          `json:"conflicts"`
	Pct        float64      `json:"used_pct"`

	start, end netip.Addr
}

// Label ist der Name bzw. ersatzweise das Netz.
func (n Network) Label() string {
	if n.Name != "" {
		return n.Name
	}
	return n.Subnet
}

// inPool meldet, ob ip im DHCP-Bereich liegt.
func (n Network) inPool(ip netip.Addr) bool {
	return n.start.IsValid() && n.end.IsValid() && !ip.Less(n.start) && !n.end.Less(ip)
}

// Entry ist eine belegte Adresse.
type Entry struct {
	IP       string    `json:"ip"`
	Name     string    `json:"name"`
	Note     string    `json:"note,omitempty"`
	NoteKey  string    `json:"-"` // statt Note: i18n-Schluessel (diese Node, Router, Konflikt)
	NameKey  string    `json:"-"` // ohne Namen: i18n-Schluessel statt des Namens
	MAC      string    `json:"mac"`
	MACs     []string  `json:"macs,omitempty"` // Konflikt: alle beteiligten MAC-Adressen
	Kind     string    `json:"kind"`
	Sources  []string  `json:"sources"`
	Seen     time.Time `json:"last_seen,omitzero"`
	State    string    `json:"state"` // online | offline | unknown | conflict
	ManualID int64     `json:"manual_id,omitempty"`

	ip netip.Addr
}

// Snapshot ist die Belegung aller Netze zu einem Zeitpunkt.
type Snapshot struct {
	Networks []Network
	entries  map[netip.Prefix][]Entry
}

// Entries liefert die belegten Adressen eines Netzes, nach Adresse sortiert.
func (s *Snapshot) Entries(p netip.Prefix) []Entry { return s.entries[p] }

// Network sucht ein Netz ueber seine Schreibweise ("192.168.1.0/24").
func (s *Snapshot) Network(subnet string) *Network {
	for i := range s.Networks {
		if s.Networks[i].Subnet == subnet {
			return &s.Networks[i]
		}
	}
	return nil
}

// vlanOf liest das VLAN aus dem Namen der Schnittstelle (eth0.20, vlan20, br-vlan20); 0 = keins.
func vlanOf(iface string) int {
	s := iface
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		s = s[i+1:]
	} else if i := strings.LastIndex(strings.ToLower(s), "vlan"); i >= 0 {
		s = s[i+4:]
	} else {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 4094 {
		return n
	}
	return 0
}

func addrText(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

// networks fuehrt die DHCP-Netze aus DNS64 und die eigenen Netze zusammen (ohne Belegung), sortiert nach Adresse.
func (m *Module) networks(ctx context.Context) []Network {
	var out []Network
	idx := map[netip.Prefix]int{}
	if m.hints != nil {
		for _, h := range m.hints(ctx) {
			p := h.Subnet.Masked()
			if _, dup := idx[p]; dup || !p.Addr().Is4() || p.Bits() < minPrefix || p.Bits() > maxPrefix {
				continue
			}
			idx[p] = len(out)
			out = append(out, Network{Prefix: p, Subnet: p.String(), Name: h.Name, Iface: h.Iface, Router: addrText(h.Router),
				RangeStart: addrText(h.Start), RangeEnd: addrText(h.End), DHCP: true})
		}
	}
	rows, err := m.networkRows(ctx)
	if err != nil {
		m.log.Warn("ipam64: networks", "err", err)
	}
	for _, r := range rows {
		p, err := netip.ParsePrefix(r.Subnet)
		if err != nil {
			continue
		}
		i, ok := idx[p]
		if !ok {
			i, idx[p] = len(out), len(out)
			out = append(out, Network{Prefix: p, Subnet: r.Subnet})
		}
		n := &out[i]
		n.Manual, n.VLAN, n.Comment = true, r.VLAN, r.Comment
		if r.Name != "" {
			n.Name = r.Name
		}
		if r.Iface != "" {
			n.Iface = r.Iface
		}
		if r.Router != "" {
			n.Router = r.Router
		}
		if !n.DHCP { // den Bereich eines DHCP-Netzes kennt DNS64 besser
			n.RangeStart, n.RangeEnd = r.RangeStart, r.RangeEnd
		}
	}
	for i := range out {
		n := &out[i]
		if n.VLAN == 0 {
			n.VLAN = vlanOf(n.Iface)
		}
		n.start, _ = netip.ParseAddr(n.RangeStart)
		n.end, _ = netip.ParseAddr(n.RangeEnd)
		n.Size = 1<<(32-n.Prefix.Bits()) - 2
		n.Free = n.Size
		n.Attached = m.attached(n.Prefix)
	}
	slices.SortFunc(out, func(a, b Network) int { return a.Prefix.Addr().Compare(b.Prefix.Addr()) })
	return out
}

// hostOf liefert den Rechnernamen bzw. die IP aus einem Ziel (IP, Name, "host:port", URL).
func hostOf(target string) string {
	t := strings.TrimSpace(target)
	if t == "" {
		return ""
	}
	if strings.Contains(t, "://") {
		if u, err := url.Parse(t); err == nil {
			return strings.ToLower(u.Hostname())
		}
		return ""
	}
	if i := strings.IndexAny(t, "/?"); i >= 0 {
		t = t[:i]
	}
	if _, err := netip.ParseAddr(t); err == nil {
		return t
	}
	if h, _, err := net.SplitHostPort(t); err == nil {
		t = h
	}
	return strings.ToLower(strings.TrimSuffix(strings.Trim(t, "[]"), "."))
}

// acc sammelt die Hinweise zu einer Adresse.
type acc struct {
	e        Entry
	assigned []string        // der Adresse zugewiesene MAC-Adressen (DHCP, von Hand)
	recent   []string        // in den letzten conflictFor gesehene MAC-Adressen
	lastMAC  string          // zuletzt gesehene MAC
	names    map[string]bool // alle bekannten Namen (klein)
}

func (a *acc) source(key string) {
	if !slices.Contains(a.e.Sources, key) {
		a.e.Sources = append(a.e.Sources, key)
	}
}

func (a *acc) name(name, note string) {
	if name = strings.TrimSpace(name); name == "" {
		return
	}
	a.names[strings.ToLower(name)] = true
	if a.e.Name == "" {
		a.e.Name, a.e.Note = name, strings.TrimSpace(note)
	} else if a.e.Note == "" && a.e.NoteKey == "" {
		a.e.Note = strings.TrimSpace(note)
	}
}

func (a *acc) seen(t time.Time) {
	if t.After(a.e.Seen) {
		a.e.Seen = t
	}
}

func (a *acc) assign(mac string) {
	if mac = strings.ToLower(strings.TrimSpace(mac)); mac != "" && !slices.Contains(a.assigned, mac) {
		a.assigned = append(a.assigned, mac)
	}
}

// Snapshot fuehrt alle Quellen zusammen.
func (m *Module) Snapshot(ctx context.Context) (*Snapshot, error) {
	now := m.now()
	nets := m.networks(ctx)
	netOf := func(ip netip.Addr) *Network {
		for i := range nets {
			if nets[i].Prefix.Contains(ip) {
				return &nets[i]
			}
		}
		return nil
	}
	all := map[netip.Addr]*acc{}
	get := func(ip netip.Addr) *acc { // nil: in keinem Netz bzw. Netz- oder Broadcast-Adresse
		ip = ip.Unmap()
		n := netOf(ip)
		if n == nil || ip == n.Prefix.Addr() || ip == lastAddr(n.Prefix) {
			return nil
		}
		a := all[ip]
		if a == nil {
			a = &acc{e: Entry{IP: ip.String(), ip: ip}, names: map[string]bool{}}
			all[ip] = a
		}
		return a
	}
	manual, err := m.Addresses(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range manual { // von Hand: Name und Bemerkung gehen vor
		ip, err := netip.ParseAddr(r.IP)
		if err != nil {
			continue
		}
		if a := get(ip); a != nil {
			a.source(SrcManual)
			a.name(r.Name, r.Comment)
			a.assign(r.MAC)
			a.e.ManualID = r.ID
		}
	}
	for _, l := range m.locals() { // die Node selbst
		if a := get(l.Addr()); a != nil {
			a.source(SrcNode)
			a.seen(now)
			if a.e.Note == "" {
				a.e.NoteKey = "ipam64.note.self"
			}
		}
	}
	type later struct {
		key string
		s   Sighting
	}
	var byName []later
	for _, src := range m.sources {
		for _, s := range src.f(ctx) {
			h := hostOf(s.Addr)
			ip, err := netip.ParseAddr(h)
			if err != nil {
				if h != "" {
					byName = append(byName, later{src.key, s})
				}
				continue
			}
			a := get(ip)
			if a == nil {
				continue
			}
			a.source(src.key)
			a.name(s.Name, s.Note)
			a.seen(s.Seen)
			a.assign(s.MAC)
			if s.Kind == KindFixed || (s.Kind == KindDHCP && a.e.Kind == "") {
				a.e.Kind = s.Kind
			}
		}
	}
	seen, err := m.seenRows(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range seen { // aufsteigend nach Zeit: die letzte MAC gewinnt
		a := get(s.IP)
		if a == nil {
			continue
		}
		a.source(SrcScan)
		a.seen(s.Last)
		a.lastMAC = s.MAC
		if now.Sub(s.Last) <= conflictFor && !slices.Contains(a.recent, s.MAC) {
			a.recent = append(a.recent, s.MAC)
		}
	}
	names := map[string]*acc{} // Name -> Adresse, auch der erste Teil eines vollen Namens (nas.home.lan -> nas)
	for _, a := range all {
		for n := range a.names {
			names[n] = a
			if short, _, ok := strings.Cut(n, "."); ok && names[short] == nil {
				names[short] = a
			}
		}
	}
	for _, l := range byName { // Ziele mit Namen (Healthchecks, PROXY64): nur an bekannte Adressen haengen
		if a := names[hostOf(l.s.Addr)]; a != nil {
			a.source(l.key)
			a.seen(l.s.Seen)
			a.name(l.s.Name, l.s.Note)
		}
	}
	snap := &Snapshot{Networks: nets, entries: map[netip.Prefix][]Entry{}}
	for ip, a := range all {
		n := netOf(ip)
		e := a.e
		e.MAC = a.lastMAC
		if len(a.assigned) > 0 && !slices.Contains(a.assigned, a.lastMAC) { // zugewiesen geht vor gesehen
			e.MAC = a.assigned[0]
		}
		conflict := len(a.recent) > 1
		for _, mac := range a.recent { // erreichbar mit einer anderen MAC als zugewiesen
			conflict = conflict || (len(a.assigned) > 0 && !slices.Contains(a.assigned, mac))
		}
		switch {
		case conflict:
			e.Kind, e.State, e.NoteKey = KindConflict, "conflict", "ipam64.note.conflict"
			e.MACs = append(slices.Clone(a.assigned), a.recent...)
			slices.Sort(e.MACs)
			e.MACs = slices.Compact(e.MACs)
			n.Conflicts++
		case e.Seen.IsZero():
			e.State = "unknown"
		case now.Sub(e.Seen) <= onlineFor:
			e.State = "online"
		default:
			e.State = "offline"
		}
		if e.Kind == "" {
			e.Kind = KindStatic
		}
		if e.Note == "" && e.NoteKey == "" && e.IP == n.Router {
			e.NoteKey = "ipam64.note.router"
		}
		if e.Name == "" && e.NoteKey != "" && !conflict { // ohne Namen: der Hinweis ist der Name
			e.NameKey, e.NoteKey = e.NoteKey, ""
		}
		n.Used++
		snap.entries[n.Prefix] = append(snap.entries[n.Prefix], e)
	}
	for i := range nets {
		n := &nets[i]
		list := snap.entries[n.Prefix]
		slices.SortFunc(list, func(a, b Entry) int { return a.ip.Compare(b.ip) })
		n.Free = max(0, n.Size-n.Used)
		if n.Size > 0 {
			n.Pct = float64(n.Used) * 100 / float64(n.Size)
		}
		if n.start.IsValid() && n.end.IsValid() {
			used := 0
			for _, e := range list {
				if n.inPool(e.ip) {
					used++
				}
			}
			n.PoolFree = max(0, poolSize(n.start, n.end)-used)
		}
	}
	return snap, nil
}

func u32(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func poolSize(a, b netip.Addr) int { return int(u32(b)-u32(a)) + 1 }
