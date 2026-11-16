package ipam64

import (
	"context"
	"database/sql"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// Ereignisse: Was sich zwischen zwei Meldungen eines fremden DHCP-Servers geaendert hat. Eine Verlaengerung ist kein
// Ereignis. Die erste Meldung einer Quelle schreibt nur "Quelle meldet erstmals" (sonst ein Ereignis je Geraet).

const (
	EvSourceNew     = "source_new"     // Quelle meldet zum ersten Mal
	EvLeaseNew      = "lease_new"      // neue Vergabe
	EvLeaseChange   = "lease_change"   // dieselbe Adresse an ein anderes Geraet (detail = vorige MAC)
	EvLeaseExpired  = "lease_expired"  // Vergabe abgelaufen (Zeit = Ablauf)
	EvLeaseReleased = "lease_released" // Vergabe vor Ablauf verschwunden (zurueckgegeben oder geloescht)
	EvFixedNew      = "fixed_new"      // feste Zuweisung angelegt
	EvFixedChange   = "fixed_change"   // feste Zuweisung auf eine andere MAC
	EvFixedGone     = "fixed_gone"     // feste Zuweisung entfernt
	EvNetNew        = "net_new"        // Netz neu gemeldet
	EvNetGone       = "net_gone"       // Netz nicht mehr gemeldet

	eventsKeep = 90 * 24 * time.Hour
	eventsMax  = 5000
)

// EventKinds: Reihenfolge der Auswahl auf der Seite.
var EventKinds = []string{EvLeaseNew, EvLeaseChange, EvLeaseExpired, EvLeaseReleased, EvFixedNew, EvFixedChange, EvFixedGone, EvNetNew, EvNetGone, EvSourceNew}

// Event ist ein Ereignis.
type Event struct {
	ID     int64     `json:"id"`
	Time   time.Time `json:"time"`
	Source string    `json:"source"`
	Label  string    `json:"source_label"`
	Kind   string    `json:"kind"`
	IP     string    `json:"ip,omitempty"` // Adresse bzw. bei net_* das Netz
	MAC    string    `json:"mac,omitempty"`
	Name   string    `json:"name,omitempty"`
	Detail string    `json:"detail,omitempty"` // lease_change, fixed_change: vorige MAC
}

// leaseState ist eine Vergabe bzw. feste Zuweisung fuer den Vergleich.
type leaseState struct {
	mac, name string
	expires   int64
}

// diffEvents vergleicht den alten und neuen Stand einer Quelle. prev = Zeitpunkt der vorigen Meldung, now = jetzt.
// Abgelaufene Vergaben zaehlen auf beiden Seiten nicht als vorhanden.
func diffEvents(prev, now int64, oldLease, newLease, oldFixed, newFixed map[string]leaseState, oldNets, newNets []string) []Event {
	var out []Event
	add := func(ts int64, kind, ip string, l leaseState, detail string) {
		out = append(out, Event{Time: time.Unix(ts, 0), Kind: kind, IP: ip, MAC: l.mac, Name: l.name, Detail: detail})
	}
	active := func(l leaseState, at int64) bool { return l.expires == 0 || l.expires > at }
	for ip, n := range newLease {
		if !active(n, now) {
			continue
		}
		o, had := oldLease[ip]
		switch {
		case !had || !active(o, prev):
			add(now, EvLeaseNew, ip, n, "")
		case o.mac != n.mac && n.mac != "":
			add(now, EvLeaseChange, ip, n, o.mac)
		}
	}
	for ip, o := range oldLease {
		if !active(o, prev) {
			continue
		}
		if n, has := newLease[ip]; has && active(n, now) {
			continue
		}
		if o.expires != 0 && o.expires <= now {
			add(o.expires, EvLeaseExpired, ip, o, "")
		} else {
			add(now, EvLeaseReleased, ip, o, "")
		}
	}
	for ip, n := range newFixed {
		o, had := oldFixed[ip]
		switch {
		case !had:
			add(now, EvFixedNew, ip, n, "")
		case o.mac != n.mac:
			add(now, EvFixedChange, ip, n, o.mac)
		}
	}
	for ip, o := range oldFixed {
		if _, has := newFixed[ip]; !has {
			add(now, EvFixedGone, ip, o, "")
		}
	}
	for _, n := range newNets {
		if !slices.Contains(oldNets, n) {
			add(now, EvNetNew, n, leaseState{}, "")
		}
	}
	for _, n := range oldNets {
		if !slices.Contains(newNets, n) {
			add(now, EvNetGone, n, leaseState{}, "")
		}
	}
	slices.SortStableFunc(out, func(a, b Event) int { // nach Zeit, dann Adresse: gleiche Meldung, gleiche Reihenfolge
		if c := a.Time.Compare(b.Time); c != 0 {
			return c
		}
		x, _ := netip.ParseAddr(a.IP)
		y, _ := netip.ParseAddr(b.IP)
		if c := x.Compare(y); c != 0 {
			return c
		}
		return strings.Compare(a.Kind, b.Kind)
	})
	return out
}

// sourceState liest den gespeicherten Stand einer Quelle fuer den Vergleich (in der Transaktion der Meldung).
func sourceState(ctx context.Context, tx *sql.Tx, name string) (lease, fixed map[string]leaseState, nets []string, err error) {
	lease, fixed = map[string]leaseState{}, map[string]leaseState{}
	rows, err := tx.QueryContext(ctx, `SELECT ip, kind, mac, name, expires FROM ipam_source_leases WHERE source = ?`, name)
	if err != nil {
		return nil, nil, nil, err
	}
	for rows.Next() {
		var ip, kind string
		var l leaseState
		if err := rows.Scan(&ip, &kind, &l.mac, &l.name, &l.expires); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		if kind == KindFixed {
			fixed[ip] = l
		} else {
			lease[ip] = l
		}
	}
	if err := closeRows(rows); err != nil {
		return nil, nil, nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT subnet FROM ipam_source_networks WHERE source = ? ORDER BY subnet`, name)
	if err != nil {
		return nil, nil, nil, err
	}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, nil, nil, err
		}
		nets = append(nets, s)
	}
	return lease, fixed, nets, closeRows(rows)
}

// writeEvents schreibt Ereignisse einer Quelle und haelt die Tabelle klein (Alter und Zahl).
func writeEvents(ctx context.Context, tx *sql.Tx, source, label string, list []Event, now time.Time) error {
	for _, e := range list {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ipam_events (ts, source, label, kind, ip, mac, name, detail) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			e.Time.Unix(), source, label, e.Kind, e.IP, e.MAC, e.Name, e.Detail); err != nil {
			return err
		}
	}
	if len(list) == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ipam_events WHERE ts < ?`, now.Add(-eventsKeep).Unix()); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM ipam_events WHERE id <= (SELECT id FROM ipam_events ORDER BY id DESC LIMIT 1 OFFSET ?)`, eventsMax)
	return err
}

// EventFilter waehlt Ereignisse aus (leere Felder = alle).
type EventFilter struct {
	Kind   string
	Source string
	Prefix netip.Prefix // Adressen in diesem Praefix bzw. das Netz selbst
	IP     string       // genau diese Adresse
	Query  string       // Teil von Adresse, Name oder MAC
}

// Events liefert die Ereignisse, neueste zuerst.
func (m *Module) Events(ctx context.Context, f EventFilter) ([]Event, error) {
	rows, err := m.core.Reader().QueryContext(ctx, `SELECT id, ts, source, label, kind, ip, mac, name, detail FROM ipam_events ORDER BY ts DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	q := strings.ToLower(strings.TrimSpace(f.Query))
	out := []Event{}
	for rows.Next() {
		var e Event
		var ts int64
		if err := rows.Scan(&e.ID, &ts, &e.Source, &e.Label, &e.Kind, &e.IP, &e.MAC, &e.Name, &e.Detail); err != nil {
			return nil, err
		}
		e.Time = time.Unix(ts, 0).UTC()
		if e.Label == "" {
			e.Label = e.Source
		}
		if (f.Kind != "" && e.Kind != f.Kind) || (f.Source != "" && e.Source != f.Source) || (f.IP != "" && e.IP != f.IP) {
			continue
		}
		if f.Prefix.IsValid() {
			if ip, err := netip.ParseAddr(e.IP); err == nil {
				if !f.Prefix.Contains(ip) {
					continue
				}
			} else if p, err := netip.ParsePrefix(e.IP); err != nil || !f.Prefix.Overlaps(p) {
				continue
			}
		}
		if q != "" && !strings.Contains(strings.ToLower(e.IP+" "+e.Name+" "+e.MAC+" "+e.Detail), q) {
			continue
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
