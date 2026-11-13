// Package ipam64 ist das optionale Node64-Modul IPAM64: die IP-Belegung der Netze im Heimnetz auf einen Blick.
//
// Das Modul pflegt kaum eigene Daten, es fuehrt zusammen, was die Node schon weiss: DHCP-Netze, Vergaben und feste
// Adressen sowie lokale DNS-Eintraege aus DNS64, Ziele der Healthchecks, Hosts aus Monitor64, Ziele von PROXY64 und
// die Nachbartabelle (ARP) des Hosts. Eigene Daten sind nur zusaetzliche Netze (bzw. Name und VLAN zu einem
// DHCP-Netz), von Hand eingetragene Adressen und die Sichtungen aus der Nachbartabelle (30 Tage).
//
// Standard aus: ausgeschaltet gibt es keinen Menuepunkt, keine Hintergrundarbeit und keinen Netz-Scan. Nur IPv4.
package ipam64

import (
	"context"
	_ "embed"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/ipv64net/node64/internal/module"
	"github.com/ipv64net/node64/internal/store"
)

// changelog wird unter Einstellungen > Module angezeigt (Version siehe Info).
//
//go:embed CHANGELOG.md
var changelog string

//go:embed CHANGELOG.en.md
var changelogEN string

// Name ist der technische Schluessel des Moduls.
const Name = "ipam64"

const (
	version = "0.1.0"

	setAutoScan = "ipam64.autoscan" // automatischer Netz-Scan alle n Minuten (0 = aus)
	setLastScan = "ipam64.lastscan" // letzter Netz-Scan (Unix-Sekunden)

	pollEvery   = 2 * time.Minute     // Nachbartabelle lesen
	onlineFor   = 10 * time.Minute    // so lange nach der letzten Sichtung gilt ein Geraet als online
	conflictFor = 15 * time.Minute    // zwei MAC-Adressen in dieser Zeit an einer IP: Konflikt
	seenKeep    = 30 * 24 * time.Hour // Sichtungen aufbewahren
	stopWait    = 10 * time.Second
)

// AutoScans sind die waehlbaren Abstaende des automatischen Netz-Scans in Minuten (0 = aus).
var AutoScans = []int{0, 5, 15, 60}

// Quellen eines Eintrags ("Bekannt aus").
const (
	SrcManual       = "ipam"
	SrcNode         = "node"
	SrcDHCP         = "dhcp"
	SrcDNS          = "dns"
	SrcHealthchecks = "healthchecks"
	SrcMonitor64    = "monitor64"
	SrcProxy        = "proxy"
	SrcScan         = "scan"
)

// Arten eines Eintrags.
const (
	KindDHCP     = "dhcp"     // vom DHCP-Server vergeben
	KindFixed    = "fixed"    // feste Adresse im DHCP-Server
	KindStatic   = "static"   // alles andere: am Geraet eingestellt bzw. von Hand eingetragen
	KindConflict = "conflict" // zwei MAC-Adressen an einer IP
)

// Sighting ist ein Hinweis einer Quelle auf eine belegte Adresse.
type Sighting struct {
	Addr string    // IP, Name, "host:port" oder URL; Namen werden ueber bekannte Namen einer IP zugeordnet
	MAC  string    // der Adresse zugewiesene MAC (DHCP), leer = unbekannt
	Name string    // Name des Geraets bzw. des Eintrags in der Quelle
	Note string    // kurzer Zusatz unter dem Namen
	Kind string    // KindDHCP, KindFixed oder leer
	Seen time.Time // zuletzt sicher erreichbar (Null = unbekannt)
}

// NetHint ist ein Netz aus einer Quelle (DHCP-Netze aus DNS64).
type NetHint struct {
	Name, Iface        string
	Subnet             netip.Prefix
	Router, Start, End netip.Addr
}

// Neighbor ist ein Eintrag der Nachbartabelle: MAC und wie lange die letzte Bestaetigung her ist.
type Neighbor struct {
	MAC string
	Age time.Duration
}

type source struct {
	key string
	f   func(ctx context.Context) []Sighting
}

// Module implementiert module.Module.
type Module struct {
	core *store.Store
	log  *slog.Logger
	g    module.Guards

	sources []source                                  // Reihenfolge = Rang der Namen
	hints   func(ctx context.Context) []NetHint       // DHCP-Netze (nil = keine)
	neigh   func() (map[netip.Addr]Neighbor, error)   // Nachbartabelle des Hosts
	locals  func() []netip.Prefix                     // Adressen der Node mit Netz
	sweep   func(ctx context.Context, p netip.Prefix) // spricht alle Adressen eines Netzes an (Tests ersetzen es)
	now     func() time.Time
	wait    time.Duration // nach dem Ansprechen auf Antworten warten

	scanMu sync.Mutex // ein Netz-Scan zur Zeit
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// New erzeugt das Modul.
func New(core *store.Store, log *slog.Logger) *Module {
	return &Module{core: core, log: log.With("module", Name), neigh: neighbors, locals: localPrefixes, sweep: sweepUDP, now: time.Now, wait: scanWait}
}

// SetSource haengt eine Quelle an (vor dem Start; die Reihenfolge bestimmt, wessen Name gewinnt). f liefert nil,
// solange das Modul der Quelle nicht laeuft.
func (m *Module) SetSource(key string, f func(ctx context.Context) []Sighting) {
	m.sources = append(m.sources, source{key, f})
}

// SetNetworks liefert die DHCP-Netze aus DNS64 (erscheinen ohne Zutun als Netze).
func (m *Module) SetNetworks(f func(ctx context.Context) []NetHint) { m.hints = f }

func (m *Module) Info() module.Info {
	return module.Info{Name: Name, TitleKey: "nav.ipam64", Icon: "bi-grid-3x3-gap", Path: "/ipam64", Available: true, Order: 45,
		Version: version, Changelog: changelog, ChangelogEN: changelogEN}
}

func (m *Module) Migrations() []store.Migration { return Migrations() }

func (m *Module) Routes(mux *http.ServeMux, g module.Guards) {
	m.g = g
	m.uiRoutes(mux, g)
	m.apiRoutes(mux, g)
}

// Start liest ab jetzt regelmaessig die Nachbartabelle (und scannt, wenn eingestellt).
func (m *Module) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		return nil
	}
	life, cancel := context.WithCancel(context.Background())
	m.cancel, m.done = cancel, make(chan struct{})
	go m.loop(life, m.done)
	return nil
}

// Stop beendet die Hintergrundarbeit.
func (m *Module) Stop(ctx context.Context) error {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.cancel, m.done = nil, nil
	m.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-time.After(stopWait):
	case <-ctx.Done():
	}
	return nil
}

func (m *Module) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(pollEvery)
	defer t.Stop()
	var pruned time.Time
	for {
		now := m.now()
		if now.Sub(pruned) >= 24*time.Hour {
			m.pruneSeen(ctx, now)
			pruned = now
		}
		if n := m.AutoScan(ctx); n > 0 && now.Sub(m.LastScan(ctx)) >= time.Duration(n)*time.Minute {
			if _, err := m.ScanAll(ctx); err != nil && ctx.Err() == nil {
				m.log.Warn("ipam64: scan", "err", err)
			}
		} else if err := m.poll(ctx); err != nil && ctx.Err() == nil {
			m.log.Debug("ipam64: neighbour table", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// AutoScan ist der Abstand des automatischen Netz-Scans in Minuten (0 = aus).
func (m *Module) AutoScan(ctx context.Context) int {
	n, _ := strconv.Atoi(m.core.SettingOr(ctx, setAutoScan, "0"))
	for _, v := range AutoScans {
		if v == n {
			return n
		}
	}
	return 0
}

// SetAutoScan stellt den automatischen Netz-Scan ein (nur Werte aus AutoScans).
func (m *Module) SetAutoScan(ctx context.Context, minutes int) error {
	for _, v := range AutoScans {
		if v == minutes {
			return m.core.SetSetting(ctx, setAutoScan, strconv.Itoa(minutes))
		}
	}
	return ErrNotFound
}

// LastScan ist der Zeitpunkt des letzten Netz-Scans (Null = nie).
func (m *Module) LastScan(ctx context.Context) time.Time {
	n, _ := strconv.ParseInt(m.core.SettingOr(ctx, setLastScan, "0"), 10, 64)
	if n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// localPrefixes liefert die IPv4-Adressen der Node mit ihrem Netz.
func localPrefixes() []netip.Prefix {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []netip.Prefix
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(n.IP)
		ones, _ := n.Mask.Size()
		if ip = ip.Unmap(); !ok || !ip.Is4() || ip.IsLoopback() {
			continue
		}
		out = append(out, netip.PrefixFrom(ip, ones))
	}
	return out
}
