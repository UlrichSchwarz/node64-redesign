package ipam64

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"time"
)

// Netz-Scan: Die Node schickt jedem moeglichen Geraet eines direkt angeschlossenen Netzes ein einzelnes kleines
// UDP-Paket (Port 9, "discard"). Dafuer fragt der Kernel die MAC per ARP an; wer antwortet, steht danach in der
// Nachbartabelle. Es werden keine Ports abgetastet und keine besonderen Rechte gebraucht.

const (
	scanPort  = 9
	scanPause = 2 * time.Millisecond    // zwischen zwei Adressen (ARP nicht fluten)
	scanWait  = 1500 * time.Millisecond // auf ARP-Antworten warten
	scanFresh = 30 * time.Second        // so frisch muss die Bestaetigung nach dem Scan sein
)

// ErrNotAttached: das Netz haengt nicht direkt an der Node (z. B. Docker-Bridge, geroutetes VLAN) – kein ARP moeglich.
var ErrNotAttached = errors.New("ipam64: network not directly attached")

// sweepUDP spricht alle Adressen des Netzes an (ohne Netz- und Broadcast-Adresse).
func sweepUDP(ctx context.Context, p netip.Prefix) {
	c, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return
	}
	defer c.Close()
	last := lastAddr(p)
	for ip := p.Masked().Addr().Next(); ip.IsValid() && ip != last && ctx.Err() == nil; ip = ip.Next() {
		_, _ = c.WriteTo([]byte{0}, net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, scanPort)))
		time.Sleep(scanPause)
	}
}

// attached meldet, ob die Node eine Adresse im Netz hat.
func (m *Module) attached(p netip.Prefix) bool {
	for _, l := range m.locals() {
		if p.Contains(l.Addr()) {
			return true
		}
	}
	return false
}

// poll uebernimmt die Nachbartabelle in die Sichtungen.
func (m *Module) poll(ctx context.Context) error {
	_, err := m.collect(ctx, nil, 0)
	return err
}

// collect schreibt die Sichtungen aus der Nachbartabelle (nur Adressen in bekannten Netzen) und zaehlt die Geraete
// in only, deren Bestaetigung hoechstens fresh alt ist.
func (m *Module) collect(ctx context.Context, only *netip.Prefix, fresh time.Duration) (int, error) {
	tab, err := m.neigh()
	if err != nil {
		return 0, err
	}
	nets := m.networks(ctx)
	now := m.now()
	found := 0
	byTime := map[int64]map[netip.Addr]string{} // Sichtungen mit gleichem Zeitpunkt zusammen schreiben
	for ip, n := range tab {
		in := false
		for _, nw := range nets {
			in = in || nw.Prefix.Contains(ip)
		}
		if !in || n.Age > seenKeep {
			continue
		}
		if only != nil && only.Contains(ip) && n.Age <= fresh {
			found++
		}
		ts := now.Add(-n.Age).Unix()
		if byTime[ts] == nil {
			byTime[ts] = map[netip.Addr]string{}
		}
		byTime[ts][ip] = n.MAC
	}
	for ts, set := range byTime {
		if err := m.saveSeen(ctx, set, time.Unix(ts, 0)); err != nil {
			return found, err
		}
	}
	return found, nil
}

// Scan sucht die Geraete eines direkt angeschlossenen Netzes und liefert, wie viele geantwortet haben.
func (m *Module) Scan(ctx context.Context, subnet string) (int, error) {
	p, err := parseSubnet(subnet)
	if err != nil {
		return 0, err
	}
	known := false
	for _, n := range m.networks(ctx) {
		known = known || n.Prefix == p
	}
	if !known {
		return 0, ErrNotFound
	}
	if !m.attached(p) {
		return 0, ErrNotAttached
	}
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	return m.scanLocked(ctx, p)
}

func (m *Module) scanLocked(ctx context.Context, p netip.Prefix) (int, error) {
	m.sweep(ctx, p)
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(m.wait):
	}
	n, err := m.collect(ctx, &p, scanFresh)
	if err == nil {
		_ = m.core.SetSetting(ctx, setLastScan, strconv.FormatInt(m.now().Unix(), 10))
	}
	return n, err
}

// ScanAll scannt alle direkt angeschlossenen Netze (automatischer Scan) und liefert die Zahl der Antworten.
func (m *Module) ScanAll(ctx context.Context) (int, error) {
	m.scanMu.Lock()
	defer m.scanMu.Unlock()
	total := 0
	var first error
	for _, n := range m.networks(ctx) {
		if !m.attached(n.Prefix) {
			continue
		}
		c, err := m.scanLocked(ctx, n.Prefix)
		if total += c; err != nil && first == nil {
			first = err
		}
	}
	if total == 0 && first == nil { // nichts angeschlossen: wenigstens die Nachbartabelle lesen
		first = m.poll(ctx)
	}
	return total, first
}
