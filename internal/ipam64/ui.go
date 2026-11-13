package ipam64

import (
	"embed"
	"encoding/csv"
	"errors"
	"io/fs"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ipv64net/node64/internal/module"
)

//go:embed templates
var templateFS embed.FS

// Templates liefert die Seiten des Moduls (module.Templater).
func (m *Module) Templates() fs.FS {
	sub, _ := fs.Sub(templateFS, "templates")
	return sub
}

func (m *Module) uiRoutes(mux *http.ServeMux, g module.Guards) {
	page := func(h http.HandlerFunc) http.Handler { return g.Enabled(g.Auth(h)) }
	mux.Handle("GET /ipam64", page(m.pageOverview))
	mux.Handle("GET /ipam64/export.csv", page(m.pageExport))
	mux.Handle("POST /ipam64/network", page(m.pageNetworkSave))
	mux.Handle("POST /ipam64/network/delete", page(m.pageNetworkDelete))
	mux.Handle("POST /ipam64/address", page(m.pageAddressSave))
	mux.Handle("POST /ipam64/address/delete", page(m.pageAddressDelete))
	mux.Handle("POST /ipam64/scan", page(m.pageScan))
	mux.Handle("POST /ipam64/autoscan", page(m.pageAutoScan))
}

const mapRow = 32 // Kaestchen je Zeile der Adresskarte

// Cell ist ein Kaestchen der Adresskarte.
type Cell struct {
	IP    string
	Class string // dhcp | fixed | static | conflict | pool | free | na (Netz- bzw. Broadcast-Adresse)
	Name  string
}

type blockOpt struct {
	N     int
	Label string
}

type pageData struct {
	Networks  []Network
	Cur       *Network
	Entries   []Entry // gefiltert
	Total     int     // Eintraege des Netzes
	Counts    map[string]int
	Filter    string
	Query     string
	Rows      [][]Cell
	Blocks    []blockOpt // Netze groesser /24: Abschnitte der Karte
	Block     int
	MapFirst  string
	MapLast   string
	FreePlain int // frei ausserhalb des DHCP-Bereichs

	VLANs, Used, Size, Free, Conflicts int
	FreePct                            float64

	Form     string // net | addr | ""
	NetForm  NetworkRow
	NetEdit  bool // vorhandenes Netz (Netz nicht aenderbar)
	AddrForm AddressRow
	Err      string

	Subnets  []string // Vorschlaege: Netze an den Schnittstellen der Node
	AutoScan int
	AutoOpts []int
	LastScan time.Time
}

// URL baut den Link der Seite mit dem gewaehlten Netz und geaenderten Parametern ("k", "v", ...; "" entfernt).
func (d *pageData) URL(kv ...string) string {
	q := url.Values{}
	if d.Cur != nil {
		q.Set("net", d.Cur.Subnet)
	}
	if d.Filter != "" {
		q.Set("kind", d.Filter)
	}
	if d.Query != "" {
		q.Set("q", d.Query)
	}
	if d.Block > 0 {
		q.Set("block", strconv.Itoa(d.Block))
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			q.Del(kv[i])
		} else {
			q.Set(kv[i], kv[i+1])
		}
	}
	if len(q) == 0 {
		return "/ipam64"
	}
	return "/ipam64?" + q.Encode()
}

func addrAt(p netip.Prefix, i int) netip.Addr {
	v := u32(p.Masked().Addr()) + uint32(i)
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// cells baut die Adresskarte eines Abschnitts (hoechstens 256 Adressen).
func cells(n *Network, list []Entry, block int) [][]Cell {
	by := map[netip.Addr]*Entry{}
	for i := range list {
		by[list[i].ip] = &list[i]
	}
	total := 1 << (32 - n.Prefix.Bits())
	last := lastAddr(n.Prefix)
	var rows [][]Cell
	for i := block * 256; i < min(total, block*256+256); i++ {
		ip := addrAt(n.Prefix, i)
		c := Cell{IP: ip.String(), Class: "free"}
		switch e := by[ip]; {
		case ip == n.Prefix.Addr() || ip == last:
			c.Class = "na"
		case e != nil:
			c.Class, c.Name = e.Kind, e.Name
		case n.inPool(ip):
			c.Class = "pool"
		}
		if i%mapRow == 0 {
			rows = append(rows, nil)
		}
		rows[len(rows)-1] = append(rows[len(rows)-1], c)
	}
	return rows
}

// nextFree schlaegt die erste freie Adresse ausserhalb des DHCP-Bereichs vor.
func nextFree(n *Network, list []Entry) string {
	used := map[netip.Addr]bool{}
	for _, e := range list {
		used[e.ip] = true
	}
	last := lastAddr(n.Prefix)
	for ip := n.Prefix.Addr().Next(); ip != last && n.Prefix.Contains(ip); ip = ip.Next() {
		if !used[ip] && !n.inPool(ip) {
			return ip.String()
		}
	}
	return ""
}

func matches(e Entry, q string) bool {
	if q == "" {
		return true
	}
	for _, v := range []string{e.IP, e.Name, e.Note, e.MAC} {
		if strings.Contains(strings.ToLower(v), q) {
			return true
		}
	}
	return false
}

// overview baut die Seite aus der Abfrage (net, kind, q, block, form, edit).
func (m *Module) overview(r *http.Request) (*pageData, error) {
	ctx := r.Context()
	snap, err := m.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	d := &pageData{Networks: snap.Networks, Counts: map[string]int{}, AutoScan: m.AutoScan(ctx), AutoOpts: AutoScans, LastScan: m.LastScan(ctx),
		Query: strings.TrimSpace(q.Get("q")), Form: q.Get("form")}
	vlans := map[int]bool{}
	for _, n := range snap.Networks {
		d.Used, d.Size, d.Free, d.Conflicts = d.Used+n.Used, d.Size+n.Size, d.Free+n.Free, d.Conflicts+n.Conflicts
		if n.VLAN > 0 {
			vlans[n.VLAN] = true
		}
	}
	d.VLANs = len(vlans)
	if d.Size > 0 {
		d.FreePct = float64(d.Free) * 100 / float64(d.Size)
	}
	for _, l := range m.locals() { // Vorschlaege fuer "Netz anlegen"
		p := l.Masked()
		if p.Bits() >= minPrefix && p.Bits() <= maxPrefix && l.Addr().IsPrivate() && snap.Network(p.String()) == nil {
			d.Subnets = append(d.Subnets, p.String())
		}
	}
	if d.Cur = snap.Network(q.Get("net")); d.Cur == nil && len(snap.Networks) > 0 {
		d.Cur = &snap.Networks[0]
	}
	if d.Form != "net" && d.Form != "addr" {
		d.Form = ""
	}
	if d.Cur == nil {
		if d.Form == "addr" {
			d.Form = ""
		}
		return d, nil
	}
	all := snap.Entries(d.Cur.Prefix)
	d.Total = len(all)
	for _, e := range all {
		d.Counts[e.Kind]++
	}
	d.FreePlain = max(0, d.Cur.Free-d.Cur.PoolFree)
	switch f := q.Get("kind"); f {
	case KindDHCP, KindFixed, KindStatic, KindConflict:
		d.Filter = f
	}
	lq := strings.ToLower(d.Query)
	for _, e := range all {
		if (d.Filter == "" || e.Kind == d.Filter) && matches(e, lq) {
			d.Entries = append(d.Entries, e)
		}
	}
	blocks := max(1, (1<<(32-d.Cur.Prefix.Bits()))/256)
	if b, _ := strconv.Atoi(q.Get("block")); b > 0 && b < blocks {
		d.Block = b
	}
	if blocks > 1 {
		for i := range blocks {
			a := addrAt(d.Cur.Prefix, i*256).As4()
			d.Blocks = append(d.Blocks, blockOpt{i, strconv.Itoa(int(a[0])) + "." + strconv.Itoa(int(a[1])) + "." + strconv.Itoa(int(a[2])) + ".x"})
		}
	}
	d.Rows = cells(d.Cur, all, d.Block)
	if len(d.Rows) > 0 {
		d.MapFirst, d.MapLast = d.Rows[0][0].IP, d.Rows[len(d.Rows)-1][len(d.Rows[len(d.Rows)-1])-1].IP
	}
	switch d.Form {
	case "net":
		if q.Get("new") == "" { // gewaehltes Netz bearbeiten
			c := d.Cur
			d.NetEdit = true
			d.NetForm = NetworkRow{Subnet: c.Subnet, Name: c.Name, VLAN: c.VLAN, Iface: c.Iface, Router: c.Router, RangeStart: c.RangeStart, RangeEnd: c.RangeEnd, Comment: c.Comment}
		} else if len(d.Subnets) > 0 {
			d.NetForm.Subnet = d.Subnets[0]
		}
	case "addr":
		d.AddrForm.IP = nextFree(d.Cur, all)
		if ip, err := netip.ParseAddr(q.Get("ip")); err == nil && d.Cur.Prefix.Contains(ip) {
			d.AddrForm.IP = ip.String()
		}
		id, _ := strconv.ParseInt(q.Get("edit"), 10, 64)
		for _, e := range all {
			if id > 0 && e.ManualID == id {
				if rows, err := m.Addresses(ctx); err == nil {
					for _, a := range rows {
						if a.ID == id {
							d.AddrForm = a
						}
					}
				}
			} else if id == 0 && e.IP == q.Get("ip") && e.ManualID == 0 { // bekannte Adresse uebernehmen
				d.AddrForm.Name, d.AddrForm.MAC = e.Name, e.MAC
			}
		}
	}
	return d, nil
}

func (m *Module) render(w http.ResponseWriter, r *http.Request, status int, patch func(*pageData)) {
	d, err := m.overview(r)
	if err != nil {
		m.log.Error("ipam64: overview", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if patch != nil {
		patch(d)
	}
	m.g.Render(w, r, status, "ipam64/overview", "ipam64.title", d)
}

func (m *Module) pageOverview(w http.ResponseWriter, r *http.Request) {
	m.render(w, r, http.StatusOK, nil)
}

// errKey liefert den i18n-Schluessel zu einem Fehler der Eingaben ("" = unbekannt).
func errKey(err error) string {
	for e, k := range map[error]string{ErrSubnet: "subnet", ErrPrefix: "prefix", ErrOverlap: "overlap", ErrName: "name", ErrIface: "iface", ErrVLAN: "vlan",
		ErrRouter: "router", ErrRange: "range", ErrIP: "ip", ErrNoNet: "no_net", ErrMAC: "mac", ErrTaken: "taken", ErrNotFound: "not_found",
		ErrNotAttached: "not_attached"} {
		if errors.Is(err, e) {
			return "ipam64.err." + k
		}
	}
	return ""
}

func (m *Module) errText(r *http.Request, err error) string {
	if k := errKey(err); k != "" {
		return m.g.T(r, k)
	}
	m.log.Error("ipam64: request failed", "err", err)
	return m.g.T(r, "ipam64.err.internal")
}

// back leitet auf die Seite des Netzes zurueck.
func back(w http.ResponseWriter, r *http.Request, subnet string) {
	u := "/ipam64"
	if subnet != "" {
		u += "?" + url.Values{"net": {subnet}}.Encode()
	}
	http.Redirect(w, r, u, http.StatusSeeOther)
}

func (m *Module) pageNetworkSave(w http.ResponseWriter, r *http.Request) {
	vlan, verr := strconv.Atoi(strings.TrimSpace(r.PostFormValue("vlan")))
	if strings.TrimSpace(r.PostFormValue("vlan")) == "" {
		vlan, verr = 0, nil
	}
	in := NetworkRow{Subnet: r.PostFormValue("subnet"), Name: r.PostFormValue("name"), VLAN: vlan, Iface: r.PostFormValue("iface"), Router: r.PostFormValue("router"),
		RangeStart: r.PostFormValue("range_start"), RangeEnd: r.PostFormValue("range_end"), Comment: r.PostFormValue("comment")}
	var err error
	if verr != nil {
		err = ErrVLAN
	} else {
		in, err = m.SaveNetwork(r.Context(), in)
	}
	if err != nil {
		text := m.errText(r, err)
		m.render(w, r, http.StatusUnprocessableEntity, func(d *pageData) {
			d.Form, d.NetForm, d.Err, d.NetEdit = "net", in, text, r.PostFormValue("edit") == "1"
		})
		return
	}
	m.g.Audit(r, "ipam64.network.save", in.Subnet, in.Name)
	m.g.Flash(w, r, "success", m.g.T(r, "ipam64.flash.network_saved"))
	back(w, r, in.Subnet)
}

func (m *Module) pageNetworkDelete(w http.ResponseWriter, r *http.Request) {
	subnet := r.PostFormValue("subnet")
	if err := m.DeleteNetwork(r.Context(), subnet); err != nil {
		m.g.Flash(w, r, "danger", m.errText(r, err))
		back(w, r, subnet)
		return
	}
	m.g.Audit(r, "ipam64.network.delete", subnet, "")
	m.g.Flash(w, r, "success", m.g.T(r, "ipam64.flash.network_deleted"))
	back(w, r, "")
}

func (m *Module) pageAddressSave(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	in := AddressRow{ID: id, IP: r.PostFormValue("ip"), MAC: r.PostFormValue("mac"), Name: r.PostFormValue("name"), Comment: r.PostFormValue("comment")}
	out, err := m.SaveAddress(r.Context(), in)
	if err != nil {
		text := m.errText(r, err)
		m.render(w, r, http.StatusUnprocessableEntity, func(d *pageData) { d.Form, d.AddrForm, d.Err = "addr", in, text })
		return
	}
	m.g.Audit(r, "ipam64.address.save", out.IP, out.Name)
	m.g.Flash(w, r, "success", m.g.T(r, "ipam64.flash.address_saved"))
	back(w, r, r.PostFormValue("net"))
}

func (m *Module) pageAddressDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	if err := m.DeleteAddress(r.Context(), id); err != nil {
		m.g.Flash(w, r, "danger", m.errText(r, err))
	} else {
		m.g.Audit(r, "ipam64.address.delete", r.PostFormValue("ip"), "")
		m.g.Flash(w, r, "success", m.g.T(r, "ipam64.flash.address_deleted"))
	}
	back(w, r, r.PostFormValue("net"))
}

func (m *Module) pageScan(w http.ResponseWriter, r *http.Request) {
	subnet := r.PostFormValue("subnet")
	n, err := m.Scan(r.Context(), subnet)
	if err != nil {
		m.g.Flash(w, r, "warning", m.errText(r, err))
	} else {
		m.g.Audit(r, "ipam64.scan", subnet, strconv.Itoa(n))
		m.g.Flash(w, r, "success", m.g.T(r, "ipam64.flash.scanned", "n", strconv.Itoa(n)))
	}
	back(w, r, subnet)
}

func (m *Module) pageAutoScan(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.PostFormValue("minutes"))
	if err := m.SetAutoScan(r.Context(), n); err != nil {
		m.g.Flash(w, r, "danger", m.errText(r, err))
	} else {
		m.g.Audit(r, "ipam64.autoscan", Name, strconv.Itoa(n))
		m.g.Flash(w, r, "success", m.g.T(r, "ipam64.flash.autoscan_saved"))
	}
	back(w, r, r.PostFormValue("subnet"))
}

// csvSafe entschaerft Werte, die eine Tabellenkalkulation als Formel lesen wuerde.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// pageExport liefert die Belegung als CSV: das gewaehlte Netz (net=) bzw. alle Netze.
func (m *Module) pageExport(w http.ResponseWriter, r *http.Request) {
	snap, err := m.Snapshot(r.Context())
	if err != nil {
		m.log.Error("ipam64: export", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="ipam64.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"network", "ip", "name", "note", "mac", "kind", "sources", "last_seen", "state"})
	only := r.URL.Query().Get("net")
	for _, n := range snap.Networks {
		if only != "" && n.Subnet != only {
			continue
		}
		for _, e := range snap.Entries(n.Prefix) {
			seen := ""
			if !e.Seen.IsZero() {
				seen = e.Seen.UTC().Format(time.RFC3339)
			}
			name, note := e.Name, e.Note
			if name == "" && e.NameKey != "" {
				name = m.g.T(r, e.NameKey)
			}
			if note == "" && e.NoteKey != "" {
				note = m.g.T(r, e.NoteKey)
			}
			_ = cw.Write([]string{n.Subnet, e.IP, csvSafe(name), csvSafe(note), e.MAC, e.Kind, strings.Join(e.Sources, " "), seen, e.State})
		}
	}
	cw.Flush()
}
