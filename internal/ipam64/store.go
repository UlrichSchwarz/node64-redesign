package ipam64

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"net"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/ipv64net/node64/internal/store"
)

//go:embed migrations
var migrationFS embed.FS

// Migrations liefert die eingebetteten Migrationen des Moduls (Scope Name).
func Migrations() []store.Migration {
	ms, err := store.ReadMigrations(migrationFS, "migrations")
	if err != nil {
		panic(err)
	}
	return ms
}

// Grenzen der Eingaben.
const (
	minPrefix  = 20 // groesstes Netz: /20 (4094 Adressen)
	maxPrefix  = 30
	maxName    = 64
	maxComment = 200
)

// Fehler der Eingaben (Texte ueber errKey).
var (
	ErrSubnet   = errors.New("ipam64: invalid subnet")
	ErrPrefix   = errors.New("ipam64: prefix out of range")
	ErrOverlap  = errors.New("ipam64: subnet overlaps another network")
	ErrName     = errors.New("ipam64: invalid name")
	ErrIface    = errors.New("ipam64: invalid interface")
	ErrVLAN     = errors.New("ipam64: invalid vlan")
	ErrRouter   = errors.New("ipam64: router not in subnet")
	ErrRange    = errors.New("ipam64: invalid dhcp range")
	ErrIP       = errors.New("ipam64: invalid address")
	ErrNoNet    = errors.New("ipam64: address in no network")
	ErrMAC      = errors.New("ipam64: invalid mac")
	ErrTaken    = errors.New("ipam64: address already entered")
	ErrNotFound = errors.New("ipam64: not found")
	ErrFromDHCP = errors.New("ipam64: network comes from dhcp")
)

var ifaceRe = regexp.MustCompile(`^[A-Za-z0-9._@:-]{1,32}$`)

// NetworkRow ist ein von Hand angelegtes Netz bzw. die Ergaenzung (Name, VLAN) zu einem DHCP-Netz aus DNS64.
type NetworkRow struct {
	ID         int64     `json:"id"`
	Subnet     string    `json:"subnet"`
	Name       string    `json:"name"`
	VLAN       int       `json:"vlan"`
	Iface      string    `json:"interface"`
	Router     string    `json:"router"`
	RangeStart string    `json:"range_start"`
	RangeEnd   string    `json:"range_end"`
	Comment    string    `json:"comment"`
	CreatedAt  time.Time `json:"created_at"`
}

// AddressRow ist eine von Hand eingetragene (reservierte) Adresse.
type AddressRow struct {
	ID        int64     `json:"id"`
	IP        string    `json:"ip"`
	MAC       string    `json:"mac"`
	Name      string    `json:"name"`
	Comment   string    `json:"comment"`
	CreatedAt time.Time `json:"created_at"`
}

// seenRow ist eine Sichtung aus der Nachbartabelle.
type seenRow struct {
	IP          netip.Addr
	MAC         string
	First, Last time.Time
}

func clean(s string, max int) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) > max || strings.ContainsAny(s, "\r\n\t<>") {
		return "", false
	}
	return s, true
}

// normMAC liefert aa:bb:cc:dd:ee:ff ("" bleibt leer).
func normMAC(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	hw, err := net.ParseMAC(s)
	if err != nil || len(hw) != 6 {
		return "", ErrMAC
	}
	return hw.String(), nil
}

// parseSubnet prueft ein IPv4-Netz und liefert es mit Netzadresse.
func parseSubnet(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(s))
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, ErrSubnet
	}
	if p.Bits() < minPrefix || p.Bits() > maxPrefix {
		return netip.Prefix{}, ErrPrefix
	}
	return p.Masked(), nil
}

// normalizeNetwork prueft die Eingaben eines Netzes.
func normalizeNetwork(n *NetworkRow) error {
	p, err := parseSubnet(n.Subnet)
	if err != nil {
		return err
	}
	n.Subnet = p.String()
	var ok bool
	if n.Name, ok = clean(n.Name, maxName); !ok {
		return ErrName
	}
	if n.Comment, ok = clean(n.Comment, maxComment); !ok {
		return ErrName
	}
	if n.Iface = strings.TrimSpace(n.Iface); n.Iface != "" && !ifaceRe.MatchString(n.Iface) {
		return ErrIface
	}
	if n.VLAN < 0 || n.VLAN > 4094 {
		return ErrVLAN
	}
	in := func(v *string) (netip.Addr, bool) {
		if *v = strings.TrimSpace(*v); *v == "" {
			return netip.Addr{}, true
		}
		a, err := netip.ParseAddr(*v)
		if err != nil || !p.Contains(a) {
			return netip.Addr{}, false
		}
		*v = a.String()
		return a, true
	}
	if _, ok := in(&n.Router); !ok {
		return ErrRouter
	}
	a, ok1 := in(&n.RangeStart)
	b, ok2 := in(&n.RangeEnd)
	if !ok1 || !ok2 || a.IsValid() != b.IsValid() || (a.IsValid() && b.Less(a)) {
		return ErrRange
	}
	return nil
}

func (m *Module) networkRows(ctx context.Context) ([]NetworkRow, error) {
	rows, err := m.core.Reader().QueryContext(ctx, `SELECT id, subnet, name, vlan, iface, router, range_start, range_end, comment, created_at FROM ipam_networks ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NetworkRow
	for rows.Next() {
		var n NetworkRow
		var ts int64
		if err := rows.Scan(&n.ID, &n.Subnet, &n.Name, &n.VLAN, &n.Iface, &n.Router, &n.RangeStart, &n.RangeEnd, &n.Comment, &ts); err != nil {
			return nil, err
		}
		n.CreatedAt = time.Unix(ts, 0).UTC()
		out = append(out, n)
	}
	return out, rows.Err()
}

// SaveNetwork legt ein Netz an bzw. aendert es (Schluessel ist das Netz selbst). Netze duerfen sich nicht
// ueberschneiden, auch nicht mit den DHCP-Netzen aus DNS64.
func (m *Module) SaveNetwork(ctx context.Context, n NetworkRow) (NetworkRow, error) {
	if err := normalizeNetwork(&n); err != nil {
		return n, err
	}
	p := netip.MustParsePrefix(n.Subnet)
	for _, o := range m.networks(ctx) {
		if o.Prefix != p && o.Prefix.Overlaps(p) {
			return n, ErrOverlap
		}
	}
	_, err := m.core.Writer().ExecContext(ctx, `INSERT INTO ipam_networks (subnet, name, vlan, iface, router, range_start, range_end, comment, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (subnet) DO UPDATE SET name = excluded.name, vlan = excluded.vlan, iface = excluded.iface, router = excluded.router,
			range_start = excluded.range_start, range_end = excluded.range_end, comment = excluded.comment`,
		n.Subnet, n.Name, n.VLAN, n.Iface, n.Router, n.RangeStart, n.RangeEnd, n.Comment, time.Now().Unix())
	return n, err
}

// DeleteNetwork loescht ein eigenes Netz samt den von Hand eingetragenen Adressen darin. Ein DHCP-Netz aus DNS64
// bleibt sichtbar (nur die Ergaenzung faellt weg).
func (m *Module) DeleteNetwork(ctx context.Context, subnet string) error {
	p, err := parseSubnet(subnet)
	if err != nil {
		return err
	}
	res, err := m.core.Writer().ExecContext(ctx, `DELETE FROM ipam_networks WHERE subnet = ?`, p.String())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	for _, o := range m.networks(ctx) { // als DHCP-Netz noch da: Adressen behalten
		if o.Prefix == p {
			return nil
		}
	}
	list, err := m.Addresses(ctx)
	if err != nil {
		return err
	}
	for _, a := range list {
		if ip, err := netip.ParseAddr(a.IP); err == nil && p.Contains(ip) {
			if _, err := m.core.Writer().ExecContext(ctx, `DELETE FROM ipam_addresses WHERE id = ?`, a.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

// Addresses liefert die von Hand eingetragenen Adressen.
func (m *Module) Addresses(ctx context.Context) ([]AddressRow, error) {
	rows, err := m.core.Reader().QueryContext(ctx, `SELECT id, ip, mac, name, comment, created_at FROM ipam_addresses ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AddressRow
	for rows.Next() {
		var a AddressRow
		var ts int64
		if err := rows.Scan(&a.ID, &a.IP, &a.MAC, &a.Name, &a.Comment, &ts); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(ts, 0).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// SaveAddress traegt eine Adresse ein (ID 0) bzw. aendert sie. Die Adresse muss in einem bekannten Netz liegen.
func (m *Module) SaveAddress(ctx context.Context, a AddressRow) (AddressRow, error) {
	ip, err := netip.ParseAddr(strings.TrimSpace(a.IP))
	if err != nil || !ip.Is4() {
		return a, ErrIP
	}
	a.IP = ip.String()
	found := false
	for _, n := range m.networks(ctx) {
		if n.Prefix.Contains(ip) {
			found = ip != n.Prefix.Addr() && ip != lastAddr(n.Prefix)
		}
	}
	if !found {
		return a, ErrNoNet
	}
	if a.MAC, err = normMAC(a.MAC); err != nil {
		return a, err
	}
	var ok bool
	if a.Name, ok = clean(a.Name, maxName); !ok || a.Name == "" {
		return a, ErrName
	}
	if a.Comment, ok = clean(a.Comment, maxComment); !ok {
		return a, ErrName
	}
	var other int64
	err = m.core.Reader().QueryRowContext(ctx, `SELECT id FROM ipam_addresses WHERE ip = ?`, a.IP).Scan(&other)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return a, err
	}
	if err == nil && other != a.ID {
		return a, ErrTaken
	}
	if a.ID == 0 {
		res, err := m.core.Writer().ExecContext(ctx, `INSERT INTO ipam_addresses (ip, mac, name, comment, created_at) VALUES (?, ?, ?, ?, ?)`,
			a.IP, a.MAC, a.Name, a.Comment, time.Now().Unix())
		if err != nil {
			return a, err
		}
		a.ID, _ = res.LastInsertId()
		return a, nil
	}
	res, err := m.core.Writer().ExecContext(ctx, `UPDATE ipam_addresses SET ip = ?, mac = ?, name = ?, comment = ? WHERE id = ?`, a.IP, a.MAC, a.Name, a.Comment, a.ID)
	if err != nil {
		return a, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return a, ErrNotFound
	}
	return a, nil
}

// DeleteAddress loescht eine von Hand eingetragene Adresse.
func (m *Module) DeleteAddress(ctx context.Context, id int64) error {
	res, err := m.core.Writer().ExecContext(ctx, `DELETE FROM ipam_addresses WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// saveSeen merkt Sichtungen (IP -> MAC) zum Zeitpunkt now.
func (m *Module) saveSeen(ctx context.Context, seen map[netip.Addr]string, now time.Time) error {
	if len(seen) == 0 {
		return nil
	}
	tx, err := m.core.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for ip, mac := range seen {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ipam_seen (ip, mac, first_seen, last_seen) VALUES (?, ?, ?, ?)
			ON CONFLICT (ip, mac) DO UPDATE SET last_seen = MAX(last_seen, excluded.last_seen)`, ip.String(), mac, now.Unix(), now.Unix()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (m *Module) seenRows(ctx context.Context) ([]seenRow, error) {
	rows, err := m.core.Reader().QueryContext(ctx, `SELECT ip, mac, first_seen, last_seen FROM ipam_seen ORDER BY last_seen`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []seenRow
	for rows.Next() {
		var ip string
		var s seenRow
		var a, b int64
		if err := rows.Scan(&ip, &s.MAC, &a, &b); err != nil {
			return nil, err
		}
		if s.IP, err = netip.ParseAddr(ip); err != nil {
			continue
		}
		s.First, s.Last = time.Unix(a, 0), time.Unix(b, 0)
		out = append(out, s)
	}
	return out, rows.Err()
}

// pruneSeen loescht Sichtungen, die aelter als seenKeep sind.
func (m *Module) pruneSeen(ctx context.Context, now time.Time) {
	if _, err := m.core.Writer().ExecContext(ctx, `DELETE FROM ipam_seen WHERE last_seen < ?`, now.Add(-seenKeep).Unix()); err != nil {
		m.log.Warn("ipam64: prune sightings", "err", err)
	}
}

// lastAddr ist die letzte Adresse eines Netzes (Broadcast).
func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	host := uint32(1)<<(32-p.Bits()) - 1
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3]) | host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}
