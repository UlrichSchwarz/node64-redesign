package ipam64

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Fremde DHCP-Server: Router und Firewalls mit eigenem DHCP-Server (pfSense, OPNsense, OpenWrt ...) melden Netze,
// feste Adressen und Vergaben ueber PUT /api/ipam64/sources/{name}. Jede Meldung ersetzt den Stand der Quelle
// vollstaendig; was die Quelle nicht mehr nennt, ist danach weg. Abgelaufene Vergaben zeigt IPAM64 nicht mehr.

const (
	maxSources      = 16
	maxSrcNetworks  = 64
	maxSrcLeases    = 8192 // feste Adressen und Vergaben zusammen
	maxSrcBody      = 4 << 20
	srcStaleAfter   = time.Hour // so lange ohne Meldung: Hinweis auf der Seite
	maxSkippedShown = 5

	// SrcExtPrefix steht vor der Kennung einer fremden Quelle in "Bekannt aus" (z. B. "dhcp:pfsense").
	SrcExtPrefix = "dhcp:"
)

var (
	ErrSourceName  = errors.New("ipam64: invalid source name")
	ErrSourceLimit = errors.New("ipam64: too many sources")
	ErrSourceSize  = errors.New("ipam64: too many entries")
	ErrSourceBody  = errors.New("ipam64: invalid json")

	sourceRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
)

// FlexTime liest einen Zeitpunkt als Unix-Sekunden (Zahl) oder RFC 3339 (Text); 0, "" und null = unbekannt.
type FlexTime struct{ time.Time }

func (t *FlexTime) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == `""` || s == "0" {
		t.Time = time.Time{}
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			t.Time = time.Unix(n, 0)
			return nil
		}
		p, err := time.Parse(time.RFC3339, v)
		t.Time = p
		return err
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 || n > 1e11 {
		return fmt.Errorf("invalid time %s", s)
	}
	t.Time = time.Unix(int64(n), 0)
	return nil
}

// SourceUpdate ist die Meldung eines fremden DHCP-Servers (vollstaendiger Stand).
type SourceUpdate struct {
	Label        string          `json:"label"` // Anzeige, leer = Kennung
	Networks     []SourceNetwork `json:"networks"`
	Reservations []SourceLease   `json:"reservations"` // feste Adressen
	Leases       []SourceLease   `json:"leases"`       // aktuelle Vergaben
}

// SourceNetwork ist ein Netz, in dem die Quelle Adressen vergibt.
type SourceNetwork struct {
	Subnet     string `json:"subnet"`
	Name       string `json:"name"`
	Iface      string `json:"interface"`
	Router     string `json:"router"`
	RangeStart string `json:"range_start"`
	RangeEnd   string `json:"range_end"`
	Server     string `json:"server"` // Adresse des DHCP-Servers in diesem Netz (leer = unbekannt)
}

// SourceLease ist eine feste Adresse bzw. eine Vergabe.
type SourceLease struct {
	IP       string   `json:"ip"`
	MAC      string   `json:"mac"`
	Name     string   `json:"name"`      // Name der festen Adresse bzw. Hostname des Geraets
	Comment  string   `json:"comment"`   // Zusatz unter dem Namen
	Expires  FlexTime `json:"expires"`   // Ablauf der Vergabe (leer = unbekannt, bleibt bis zur naechsten Meldung)
	LastSeen FlexTime `json:"last_seen"` // letzte Verlaengerung (leer = unbekannt)
}

// SourceResult ist die Antwort auf eine Meldung.
type SourceResult struct {
	Source       string   `json:"source"`
	Networks     int      `json:"networks"`
	Reservations int      `json:"reservations"`
	Leases       int      `json:"leases"`
	Skipped      int      `json:"skipped"`                 // uebersprungene Eintraege (ungueltig, kein IPv4 ...)
	SkippedNotes []string `json:"skipped_notes,omitempty"` // die ersten Gruende
}

func (r *SourceResult) skip(format string, args ...any) {
	if r.Skipped++; len(r.SkippedNotes) < maxSkippedShown {
		r.SkippedNotes = append(r.SkippedNotes, fmt.Sprintf(format, args...))
	}
}

// SourceInfo ist eine fremde Quelle mit Zaehlern (Seite und GET /api/ipam64/sources).
type SourceInfo struct {
	Name         string    `json:"name"`
	Label        string    `json:"label"`
	UpdatedAt    time.Time `json:"updated_at"`
	Stale        bool      `json:"stale"` // laenger als eine Stunde keine Meldung
	Networks     int       `json:"networks"`
	Reservations int       `json:"reservations"`
	Leases       int       `json:"leases"` // nicht abgelaufene Vergaben
}

// DecodeSourceUpdate liest eine Meldung streng (unbekannte Felder sind ein Fehler, damit Tippfehler auffallen).
func DecodeSourceUpdate(body []byte) (SourceUpdate, error) {
	var in SourceUpdate
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, fmt.Errorf("%w: %v", ErrSourceBody, err)
	}
	return in, nil
}

// normalizeSourceNet prueft ein gemeldetes Netz: IPv4 von /8 bis /30, Router und Adressbereich im Netz.
func normalizeSourceNet(n SourceNetwork) (SourceNetwork, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(n.Subnet))
	if err != nil || !p.Addr().Is4() {
		return n, ErrPrefix
	}
	if p.Bits() < 8 || p.Bits() > 30 {
		return n, ErrSize
	}
	p = p.Masked()
	out := SourceNetwork{Subnet: p.String(), Name: cut(n.Name, maxName), Iface: cut(n.Iface, 32)}
	in := func(v string) (netip.Addr, bool) { // leer ist erlaubt
		if v = strings.TrimSpace(v); v == "" {
			return netip.Addr{}, true
		}
		a, err := netip.ParseAddr(v)
		return a, err == nil && p.Contains(a)
	}
	r, ok := in(n.Router)
	if !ok {
		return n, ErrIP
	}
	a, ok1 := in(n.RangeStart)
	b, ok2 := in(n.RangeEnd)
	if !ok1 || !ok2 || a.IsValid() != b.IsValid() || (a.IsValid() && b.Less(a)) {
		return n, ErrRange
	}
	srv, ok := in(n.Server)
	if !ok {
		return n, ErrIP
	}
	out.Router, out.RangeStart, out.RangeEnd, out.Server = addrText(r), addrText(a), addrText(b), addrText(srv)
	return out, nil
}

// cut kuerzt Text aus einer Meldung auf max Zeichen und entfernt Steuerzeichen (Meldungen kommen von Skripten).
func cut(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	if r := []rune(s); len(r) > max {
		s = string(r[:max])
	}
	return s
}

// UpdateSource uebernimmt die Meldung einer Quelle: Ungueltige Eintraege werden uebersprungen und gezaehlt, der Rest
// ersetzt den bisherigen Stand der Quelle.
func (m *Module) UpdateSource(ctx context.Context, name string, in SourceUpdate) (SourceResult, error) {
	res := SourceResult{Source: name}
	if !sourceRe.MatchString(name) {
		return res, ErrSourceName
	}
	if len(in.Networks) > maxSrcNetworks || len(in.Reservations)+len(in.Leases) > maxSrcLeases {
		return res, ErrSourceSize
	}
	var known int
	var exists int
	if err := m.core.Reader().QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(name = ?), 0) FROM ipam_sources`, name).Scan(&known, &exists); err != nil {
		return res, err
	}
	if exists == 0 && known >= maxSources {
		return res, ErrSourceLimit
	}
	var nets []SourceNetwork
	seenNet := map[string]bool{}
	for i, n := range in.Networks {
		row, err := normalizeSourceNet(n)
		if err != nil {
			res.skip("networks[%d] %s: %s", i, cut(n.Subnet, 40), strings.TrimPrefix(err.Error(), "ipam64: "))
			continue
		}
		if seenNet[row.Subnet] {
			res.skip("networks[%d] %s: duplicate", i, row.Subnet)
			continue
		}
		seenNet[row.Subnet] = true
		nets = append(nets, row)
	}
	type leaseRow struct {
		ip, kind, mac, name, note string
		expires, seen             int64
	}
	var leases []leaseRow
	seenLease := map[string]bool{}
	add := func(list []SourceLease, kind, field string) int {
		n := 0
		for i, l := range list {
			ip, err := netip.ParseAddr(strings.TrimSpace(l.IP))
			if err != nil || !ip.Unmap().Is4() {
				res.skip("%s[%d] %s: no ipv4 address", field, i, cut(l.IP, 40))
				continue
			}
			mac, err := normMAC(l.MAC)
			if err != nil {
				res.skip("%s[%d] %s: invalid mac", field, i, ip.Unmap())
				continue
			}
			key := kind + "/" + ip.Unmap().String()
			if seenLease[key] {
				res.skip("%s[%d] %s: duplicate", field, i, ip.Unmap())
				continue
			}
			seenLease[key] = true
			row := leaseRow{ip: ip.Unmap().String(), kind: kind, mac: mac, name: cut(l.Name, maxName), note: cut(l.Comment, maxComment)}
			if !l.Expires.IsZero() {
				row.expires = l.Expires.Unix()
			}
			if !l.LastSeen.IsZero() {
				row.seen = l.LastSeen.Unix()
			}
			leases = append(leases, row)
			n++
		}
		return n
	}
	res.Reservations = add(in.Reservations, KindFixed, "reservations")
	res.Leases = add(in.Leases, KindDHCP, "leases")
	res.Networks = len(nets)

	tx, err := m.core.Writer().BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	label := cut(in.Label, maxName)
	if _, err := tx.ExecContext(ctx, `INSERT INTO ipam_sources (name, label, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET label = excluded.label, updated_at = excluded.updated_at`, name, label, m.now().Unix()); err != nil {
		return res, err
	}
	for _, q := range []string{`DELETE FROM ipam_source_networks WHERE source = ?`, `DELETE FROM ipam_source_leases WHERE source = ?`} {
		if _, err := tx.ExecContext(ctx, q, name); err != nil {
			return res, err
		}
	}
	for _, n := range nets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ipam_source_networks (source, subnet, name, iface, router, range_start, range_end, server) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			name, n.Subnet, n.Name, n.Iface, n.Router, n.RangeStart, n.RangeEnd, n.Server); err != nil {
			return res, err
		}
	}
	for _, l := range leases {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ipam_source_leases (source, ip, kind, mac, name, note, expires, seen) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			name, l.ip, l.kind, l.mac, l.name, l.note, l.expires, l.seen); err != nil {
			return res, err
		}
	}
	if err := tx.Commit(); err != nil {
		return res, err
	}
	m.invalidate()
	return res, nil
}

// DeleteSource entfernt eine fremde Quelle mit allem, was sie gemeldet hat.
func (m *Module) DeleteSource(ctx context.Context, name string) error {
	tx, err := m.core.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	r, err := tx.ExecContext(ctx, `DELETE FROM ipam_sources WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	for _, q := range []string{`DELETE FROM ipam_source_networks WHERE source = ?`, `DELETE FROM ipam_source_leases WHERE source = ?`} {
		if _, err := tx.ExecContext(ctx, q, name); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	m.invalidate()
	return nil
}

// Sources liefert die fremden Quellen mit Zaehlern, nach Kennung sortiert.
func (m *Module) Sources(ctx context.Context) ([]SourceInfo, error) {
	now := m.now()
	rows, err := m.core.Reader().QueryContext(ctx, `SELECT s.name, s.label, s.updated_at,
		(SELECT COUNT(*) FROM ipam_source_networks n WHERE n.source = s.name),
		(SELECT COUNT(*) FROM ipam_source_leases l WHERE l.source = s.name AND l.kind = 'fixed'),
		(SELECT COUNT(*) FROM ipam_source_leases l WHERE l.source = s.name AND l.kind = 'dhcp' AND (l.expires = 0 OR l.expires > ?))
		FROM ipam_sources s ORDER BY s.name`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SourceInfo{}
	for rows.Next() {
		var s SourceInfo
		var ts int64
		if err := rows.Scan(&s.Name, &s.Label, &ts, &s.Networks, &s.Reservations, &s.Leases); err != nil {
			return nil, err
		}
		if s.Label == "" {
			s.Label = s.Name
		}
		s.UpdatedAt = time.Unix(ts, 0).UTC()
		s.Stale = now.Sub(s.UpdatedAt) > srcStaleAfter
		out = append(out, s)
	}
	return out, rows.Err()
}

// extSource ist der Stand einer fremden Quelle fuer die Zusammenfuehrung.
type extSource struct {
	name, label string
	updated     time.Time // letzte Meldung
	stale       bool      // laenger als srcStaleAfter keine Meldung: der Server gilt nicht mehr als aktiv
	hints       []NetHint
	sightings   []Sighting
}

// extSources liest alle fremden Quellen (ohne abgelaufene Vergaben), nach Kennung sortiert.
func (m *Module) extSources(ctx context.Context) ([]extSource, error) {
	db := m.core.Reader()
	now := m.now()
	rows, err := db.QueryContext(ctx, `SELECT name, label, updated_at FROM ipam_sources ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var out []extSource
	idx := map[string]int{}
	for rows.Next() {
		var s extSource
		var ts int64
		if err := rows.Scan(&s.name, &s.label, &ts); err != nil {
			rows.Close()
			return nil, err
		}
		s.updated = time.Unix(ts, 0)
		s.stale = now.Sub(s.updated) > srcStaleAfter
		if s.label == "" {
			s.label = s.name
		}
		idx[s.name] = len(out)
		out = append(out, s)
	}
	if err := closeRows(rows); err != nil || len(out) == 0 {
		return out, err
	}
	rows, err = db.QueryContext(ctx, `SELECT source, subnet, name, iface, router, range_start, range_end, server FROM ipam_source_networks ORDER BY source, subnet`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var src, subnet, router, a, b, server string
		var h NetHint
		if err := rows.Scan(&src, &subnet, &h.Name, &h.Iface, &router, &a, &b, &server); err != nil {
			rows.Close()
			return nil, err
		}
		i, ok := idx[src]
		if h.Subnet, err = netip.ParsePrefix(subnet); err != nil || !ok {
			continue
		}
		h.Router, _ = netip.ParseAddr(router)
		h.Start, _ = netip.ParseAddr(a)
		h.End, _ = netip.ParseAddr(b)
		h.Ext, h.ExtLabel = out[i].name, out[i].label
		out[i].hints = append(out[i].hints, h)
		if h.Router.IsValid() { // das Gateway ist belegt, auch wenn es keine Lease hat ("naechste freie Adresse")
			out[i].sightings = append(out[i].sightings, Sighting{Addr: h.Router.String()})
		}
		if srv, err := netip.ParseAddr(server); err == nil { // der DHCP-Server selbst: belegt und, solange er meldet, als aktiv gekennzeichnet
			sg := Sighting{Addr: srv.String(), Seen: out[i].updated} // die Meldung kommt vom Server selbst: zuletzt gesehen = letzte Meldung
			if !out[i].stale {
				h.Server, sg.DHCPServer = srv, out[i].label
				out[i].hints[len(out[i].hints)-1] = h
			}
			out[i].sightings = append(out[i].sightings, sg)
		}
	}
	if err := closeRows(rows); err != nil {
		return nil, err
	}
	rows, err = db.QueryContext(ctx, `SELECT source, ip, kind, mac, name, note, seen FROM ipam_source_leases
		WHERE kind = 'fixed' OR expires = 0 OR expires > ? ORDER BY source, kind DESC, ip`, now.Unix())
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var src string
		var seen int64
		var s Sighting
		if err := rows.Scan(&src, &s.Addr, &s.Kind, &s.MAC, &s.Name, &s.Note, &seen); err != nil {
			rows.Close()
			return nil, err
		}
		if seen > 0 {
			s.Seen = time.Unix(seen, 0)
		}
		if i, ok := idx[src]; ok {
			out[i].sightings = append(out[i].sightings, s)
		}
	}
	return out, closeRows(rows)
}

func closeRows(rows *sql.Rows) error {
	err := rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	return err
}
