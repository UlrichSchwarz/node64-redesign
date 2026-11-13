package ipam64

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ipv64net/node64/internal/config"
	"github.com/ipv64net/node64/internal/i18n"
	"github.com/ipv64net/node64/internal/logging"
	"github.com/ipv64net/node64/internal/module"
	"github.com/ipv64net/node64/internal/store"
	nodesync "github.com/ipv64net/node64/internal/sync"
	"github.com/ipv64net/node64/internal/web"
)

var csrfRe = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

// fakeNet ersetzt Nachbartabelle, Adressen der Node und das Ansprechen der Adressen.
type fakeNet struct {
	tab    map[netip.Addr]Neighbor
	swept  []netip.Prefix
	locals []netip.Prefix
}

func modTest(t *testing.T) (*Module, *fakeNet, string) {
	t.Helper()
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	core, err := store.Open(context.Background(), filepath.Join(dir, "node64.db"), filepath.Join(dir, "backups"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { core.Close() })
	cm, _ := store.CoreMigrations()
	if err := core.Migrate(context.Background(), "core", cm); err != nil {
		t.Fatal(err)
	}
	if err := core.Migrate(context.Background(), Name, Migrations()); err != nil {
		t.Fatal(err)
	}
	m := New(core, log)
	f := &fakeNet{tab: map[netip.Addr]Neighbor{}, locals: []netip.Prefix{netip.MustParsePrefix("192.168.1.2/24")}}
	m.neigh = func() (map[netip.Addr]Neighbor, error) { return f.tab, nil }
	m.locals = func() []netip.Prefix { return f.locals }
	m.sweep = func(_ context.Context, p netip.Prefix) { f.swept = append(f.swept, p) }
	m.wait = 0
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	return m, f, dir
}

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

func entry(t *testing.T, s *Snapshot, subnet, addr string) *Entry {
	t.Helper()
	n := s.Network(subnet)
	if n == nil {
		t.Fatalf("network %s missing", subnet)
	}
	for _, e := range s.Entries(n.Prefix) {
		if e.IP == addr {
			return &e
		}
	}
	return nil
}

// Quellen zusammenfuehren: Netze aus DHCP und eigene, Arten, Namen, Quellen, Zustand und Konflikte.
func TestSnapshot(t *testing.T) {
	m, f, _ := modTest(t)
	ctx := context.Background()
	now := time.Now()
	m.SetNetworks(func(context.Context) []NetHint {
		return []NetHint{{Name: "Heimnetz", Iface: "eth0", Subnet: netip.MustParsePrefix("192.168.1.0/24"), Router: ip("192.168.1.1"), Start: ip("192.168.1.100"), End: ip("192.168.1.199")}}
	})
	m.SetSource(SrcDHCP, func(context.Context) []Sighting {
		return []Sighting{
			{Addr: "192.168.1.10", MAC: "02:64:03:00:00:0A", Name: "nas", Kind: KindFixed},
			{Addr: "192.168.1.10", MAC: "02:64:03:00:00:0a", Name: "nas-lease", Kind: KindDHCP, Seen: now.Add(-time.Hour)},
			{Addr: "192.168.1.101", MAC: "02:64:03:00:00:65", Name: "macbook", Kind: KindDHCP, Seen: now.Add(-2 * time.Hour)},
			{Addr: "10.9.9.9", Name: "fremd", Kind: KindDHCP},
		}
	})
	m.SetSource(SrcDNS, func(context.Context) []Sighting {
		return []Sighting{{Addr: "192.168.1.3", Name: "pihole.home.lan"}, {Addr: "192.168.1.10", Name: "nas.home.lan"}}
	})
	m.SetSource(SrcMonitor64, func(context.Context) []Sighting {
		return []Sighting{{Addr: "192.168.1.10", Name: "NAS", Note: "Synology", Seen: now.Add(-time.Minute)}}
	})
	m.SetSource(SrcHealthchecks, func(context.Context) []Sighting {
		return []Sighting{{Addr: "https://pihole.home.lan:8443/admin", Name: "Pi-hole"}, {Addr: "nas:5001", Name: "NAS-Web"}, {Addr: "example.org", Name: "extern"}}
	})
	if _, err := m.SaveNetwork(ctx, NetworkRow{Subnet: "192.168.20.7/24", Name: "IoT", Iface: "eth0.20"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SaveNetwork(ctx, NetworkRow{Subnet: "192.168.1.0/24", VLAN: 1}); err != nil { // DHCP-Netz ergaenzen
		t.Fatal(err)
	}
	if _, err := m.SaveNetwork(ctx, NetworkRow{Subnet: "192.168.1.128/25"}); err != ErrOverlap {
		t.Fatalf("overlap: %v", err)
	}
	if _, err := m.SaveNetwork(ctx, NetworkRow{Subnet: "10.0.0.0/8"}); err != ErrPrefix {
		t.Fatalf("prefix: %v", err)
	}
	if _, err := m.SaveAddress(ctx, AddressRow{IP: "192.168.20.5", Name: "kamera", MAC: "02-64-03-00-00-2D"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SaveAddress(ctx, AddressRow{IP: "192.168.20.5", Name: "doppelt"}); err != ErrTaken {
		t.Fatalf("taken: %v", err)
	}
	if _, err := m.SaveAddress(ctx, AddressRow{IP: "172.16.0.1", Name: "x"}); err != ErrNoNet {
		t.Fatalf("no net: %v", err)
	}
	if _, err := m.SaveAddress(ctx, AddressRow{IP: "192.168.20.255", Name: "x"}); err != ErrNoNet {
		t.Fatalf("broadcast: %v", err)
	}
	// Nachbartabelle: Router frisch, Kamera mit fremder MAC (Konflikt), ein Geraet ausserhalb der Netze
	f.tab = map[netip.Addr]Neighbor{ip("192.168.1.1"): {MAC: "02:64:03:00:00:01", Age: 5 * time.Second},
		ip("192.168.20.5"): {MAC: "02:64:03:00:00:99", Age: time.Minute}, ip("172.30.0.1"): {MAC: "02:00:00:00:00:01"}}
	if err := m.poll(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := m.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Networks) != 2 || s.Networks[0].Name != "Heimnetz" || !s.Networks[0].DHCP || !s.Networks[0].Manual || s.Networks[0].VLAN != 1 || !s.Networks[0].Attached ||
		s.Networks[1].Subnet != "192.168.20.0/24" || s.Networks[1].VLAN != 20 || s.Networks[1].Attached {
		t.Fatalf("networks: %+v", s.Networks)
	}
	home := s.Networks[0]
	if home.Size != 254 || home.Used != 5 || home.Free != 249 || home.PoolFree != 99 || home.Conflicts != 0 {
		t.Fatalf("home: %+v", home)
	}
	nas := entry(t, s, "192.168.1.0/24", "192.168.1.10")
	if nas == nil || nas.Kind != KindFixed || nas.Name != "nas" || nas.Note != "Synology" || nas.MAC != "02:64:03:00:00:0a" || nas.State != "online" ||
		strings.Join(nas.Sources, ",") != "dhcp,dns,monitor64,healthchecks" {
		t.Fatalf("nas: %+v", nas)
	}
	if e := entry(t, s, "192.168.1.0/24", "192.168.1.101"); e == nil || e.Kind != KindDHCP || e.State != "offline" {
		t.Fatalf("lease: %+v", e)
	}
	if e := entry(t, s, "192.168.1.0/24", "192.168.1.3"); e == nil || e.Kind != KindStatic || e.State != "unknown" || strings.Join(e.Sources, ",") != "dns,healthchecks" {
		t.Fatalf("dns: %+v", e)
	}
	if e := entry(t, s, "192.168.1.0/24", "192.168.1.1"); e == nil || e.NameKey != "ipam64.note.router" || e.MAC != "02:64:03:00:00:01" || e.State != "online" || e.Sources[0] != SrcScan {
		t.Fatalf("router: %+v", e)
	}
	if e := entry(t, s, "192.168.1.0/24", "192.168.1.2"); e == nil || e.NameKey != "ipam64.note.self" || e.State != "online" {
		t.Fatalf("self: %+v", e)
	}
	cam := entry(t, s, "192.168.20.0/24", "192.168.20.5")
	if cam == nil || cam.Kind != KindConflict || cam.State != "conflict" || cam.MAC != "02:64:03:00:00:2d" || len(cam.MACs) != 2 || cam.ManualID == 0 || s.Networks[1].Conflicts != 1 {
		t.Fatalf("camera: %+v", cam)
	}
	// Adresskarte: Netzadresse, Router, Node, feste Adresse, frei im Bereich, DHCP, Broadcast
	rows := cells(&home, s.Entries(home.Prefix), 0)
	if len(rows) != 8 || len(rows[0]) != 32 || rows[0][0].Class != "na" || rows[0][1].Class != KindStatic || rows[0][10].Class != KindFixed || rows[0][4].Class != "free" ||
		rows[3][4].Class != "pool" || rows[3][5].Class != KindDHCP || rows[7][31].Class != "na" {
		t.Fatalf("cells: %+v", rows[0])
	}
	if got := nextFree(&home, s.Entries(home.Prefix)); got != "192.168.1.4" {
		t.Fatalf("next free: %s", got)
	}
	// eigenes Netz loeschen nimmt seine Adressen mit; beim DHCP-Netz faellt nur die Ergaenzung weg
	if err := m.DeleteNetwork(ctx, "192.168.20.0/24"); err != nil {
		t.Fatal(err)
	}
	if list, _ := m.Addresses(ctx); len(list) != 0 {
		t.Fatalf("addresses kept: %+v", list)
	}
	if err := m.DeleteNetwork(ctx, "192.168.1.0/24"); err != nil {
		t.Fatal(err)
	}
	if s, _ = m.Snapshot(ctx); len(s.Networks) != 1 || s.Networks[0].Manual || s.Networks[0].VLAN != 0 {
		t.Fatalf("after delete: %+v", s.Networks)
	}
}

// Netz-Scan: nur direkt angeschlossene Netze, zaehlt frische Antworten, merkt den Zeitpunkt.
func TestScan(t *testing.T) {
	m, f, _ := modTest(t)
	ctx := context.Background()
	for _, n := range []string{"192.168.1.0/24", "192.168.30.0/24"} {
		if _, err := m.SaveNetwork(ctx, NetworkRow{Subnet: n}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Scan(ctx, "192.168.30.0/24"); err != ErrNotAttached {
		t.Fatalf("not attached: %v", err)
	}
	if _, err := m.Scan(ctx, "192.168.99.0/24"); err != ErrNotFound {
		t.Fatalf("unknown: %v", err)
	}
	f.tab = map[netip.Addr]Neighbor{ip("192.168.1.7"): {MAC: "02:00:00:00:00:07", Age: time.Second}, ip("192.168.1.8"): {MAC: "02:00:00:00:00:08", Age: time.Hour}}
	n, err := m.Scan(ctx, "192.168.1.0/24")
	if err != nil || n != 1 || len(f.swept) != 1 || f.swept[0].String() != "192.168.1.0/24" || m.LastScan(ctx).IsZero() {
		t.Fatalf("scan: %d %v %v", n, err, f.swept)
	}
	s, _ := m.Snapshot(ctx)
	if e := entry(t, s, "192.168.1.0/24", "192.168.1.8"); e == nil || e.State != "offline" || e.Kind != KindStatic {
		t.Fatalf("stale: %+v", e)
	}
	if total, err := m.ScanAll(ctx); err != nil || total != 1 || len(f.swept) != 2 {
		t.Fatalf("scan all: %d %v", total, err)
	}
	if err := m.SetAutoScan(ctx, 15); err != nil || m.AutoScan(ctx) != 15 || m.SetAutoScan(ctx, 7) == nil {
		t.Fatalf("autoscan: %v %d", err, m.AutoScan(ctx))
	}
}

func TestHelpers(t *testing.T) {
	for in, want := range map[string]string{"192.168.1.5": "192.168.1.5", "NAS.home.lan.": "nas.home.lan", "nas:5001": "nas", "https://Pi.hole:8443/admin": "pi.hole",
		"192.168.1.5:22": "192.168.1.5", "host/path": "host", "": "", "tcp://10.0.0.1:53": "10.0.0.1"} {
		if got := hostOf(in); got != want {
			t.Errorf("hostOf(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]int{"eth0": 0, "eth0.20": 20, "vlan30": 30, "br-vlan7": 7, "eth0.9999": 0, "enp3s0": 0} {
		if got := vlanOf(in); got != want {
			t.Errorf("vlanOf(%q) = %d, want %d", in, got, want)
		}
	}
	if got := lastAddr(netip.MustParsePrefix("192.168.4.0/22")).String(); got != "192.168.7.255" {
		t.Errorf("lastAddr = %s", got)
	}
	if csvSafe("=1+1") != "'=1+1" || csvSafe("nas") != "nas" {
		t.Error("csvSafe")
	}
	if _, err := normMAC("zz"); err != ErrMAC {
		t.Errorf("normMAC: %v", err)
	}
}

// neighMsg baut eine Netlink-Nachricht RTM_NEWNEIGH (IPv4) mit Zustand, MAC und Alter der Bestaetigung.
func neighMsg(addr netip.Addr, state uint16, mac []byte, ticks uint32) []byte {
	attr := func(typ uint16, v []byte) []byte {
		b := make([]byte, 4, 4+(len(v)+3)&^3)
		binary.NativeEndian.PutUint16(b[0:2], uint16(4+len(v)))
		binary.NativeEndian.PutUint16(b[2:4], typ)
		b = append(b, v...)
		for len(b)%4 != 0 {
			b = append(b, 0)
		}
		return b
	}
	body := make([]byte, ndMsgLen)
	body[0] = 2
	binary.NativeEndian.PutUint16(body[8:10], state)
	a4 := addr.As4()
	body = append(body, attr(ndaDst, a4[:])...)
	if mac != nil {
		body = append(body, attr(ndaLLAddr, mac)...)
	}
	ci := make([]byte, 16)
	binary.NativeEndian.PutUint32(ci[0:4], ticks)
	body = append(body, attr(ndaCacheInfo, ci)...)
	msg := make([]byte, nlmsgHdrLen)
	binary.NativeEndian.PutUint32(msg[0:4], uint32(nlmsgHdrLen+len(body)))
	binary.NativeEndian.PutUint16(msg[4:6], rtmNewNeigh)
	return append(msg, body...)
}

func TestParseNeigh(t *testing.T) {
	mac := []byte{2, 0x64, 3, 0, 0, 1}
	var b []byte
	b = append(b, neighMsg(ip("192.168.1.1"), nudReachable, mac, 250)...)
	b = append(b, neighMsg(ip("192.168.1.2"), nudStale, mac, 60000)...)
	b = append(b, neighMsg(ip("192.168.1.3"), 0x20, nil, 0)...)  // gescheitert
	b = append(b, neighMsg(ip("192.168.1.4"), 0x80, mac, 0)...)  // fest eingetragen
	b = append(b, neighMsg(ip("192.168.1.5"), 0x01, nil, 10)...) // unvollstaendig
	b = append(b, 1, 2, 3)                                       // abgeschnitten
	got := parseNeigh(b)
	if len(got) != 2 || got[ip("192.168.1.1")] != (Neighbor{"02:64:03:00:00:01", 2500 * time.Millisecond}) || got[ip("192.168.1.2")].Age != 10*time.Minute {
		t.Fatalf("parseNeigh: %+v", got)
	}
}

// Seiten und API ueber den Kern: aus = 404 und kein Menuepunkt, an = Seite, Formulare, CSV, API.
func TestPagesRender(t *testing.T) {
	m, f, dir := modTest(t)
	st := m.core
	logger := logging.New("error", "text", io.Discard)
	bundle, err := i18n.Load()
	if err != nil {
		t.Fatal(err)
	}
	m.SetSource(SrcDNS, func(context.Context) []Sighting { return []Sighting{{Addr: "192.168.1.3", Name: "pi<b>hole</b>"}} })
	reg := module.NewRegistry(st, logger.Slog())
	reg.Register(m)
	t.Cleanup(reg.StopAll)
	s, err := web.New(web.Deps{Cfg: &config.Config{DataDir: dir, HTTPPort: 8080, LogLevel: "error"}, Store: st, Log: logger, I18n: bundle, Modules: reg,
		Sync: nodesync.NoOp{}, Version: "test", StartedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(method, path, body, ctype string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	get := func(path string) (int, string) { return do("GET", path, "", "") }
	post := func(path string, form url.Values) (int, string) {
		return do("POST", path, form.Encode(), "application/x-www-form-urlencoded")
	}
	_, body := get("/setup")
	csrf := csrfRe.FindStringSubmatch(body)
	if csrf == nil {
		t.Fatal("no csrf")
	}
	if code, _ := post("/setup", url.Values{"_csrf": {csrf[1]}, "lang": {"de"}, "username": {"admin"}, "password": {"sehr geheimes passwort"},
		"password2": {"sehr geheimes passwort"}, "timezone": {"Europe/Berlin"}, "node_name": {"pi"}}); code != http.StatusSeeOther {
		t.Fatalf("setup: %d", code)
	}
	for _, p := range []string{"/ipam64", "/api/ipam64/networks", "/ipam64/export.csv"} {
		if code, _ := get(p); code != http.StatusNotFound {
			t.Fatalf("module off %s: %d", p, code)
		}
	}
	if _, body := get("/"); strings.Contains(body, `href="/ipam64"`) {
		t.Fatal("menu entry while off")
	}
	if err := reg.Enable(context.Background(), Name); err != nil {
		t.Fatal(err)
	}
	code, body := get("/ipam64")
	if code != 200 || !strings.Contains(body, `id="ipam-empty"`) || strings.Contains(body, `id="ipam-list"`) || strings.Contains(body, "ipam64.") {
		t.Fatalf("empty: %d\n%s", code, body)
	}
	if _, body := get("/"); !strings.Contains(body, `href="/ipam64"`) {
		t.Fatal("no menu entry")
	}
	csrf = csrfRe.FindStringSubmatch(body)
	if code, body = post("/ipam64/network", url.Values{"_csrf": {csrf[1]}, "subnet": {"192.168.1.0/33"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, `id="ipam-form-error"`) {
		t.Fatalf("bad network: %d", code)
	}
	if code, _ = post("/ipam64/network", url.Values{"_csrf": {csrf[1]}, "subnet": {"192.168.1.0/24"}, "name": {"Heimnetz"}, "vlan": {""}, "router": {"192.168.1.1"},
		"range_start": {"192.168.1.100"}, "range_end": {"192.168.1.199"}}); code != http.StatusSeeOther {
		t.Fatalf("network: %d", code)
	}
	if code, _ = post("/ipam64/address", url.Values{"_csrf": {csrf[1]}, "net": {"192.168.1.0/24"}, "ip": {"192.168.1.40"}, "name": {"switch-keller"}, "mac": {"02:64:03:00:00:28"}, "comment": {"SNMP"}}); code != http.StatusSeeOther {
		t.Fatalf("address: %d", code)
	}
	f.tab = map[netip.Addr]Neighbor{ip("192.168.1.1"): {MAC: "02:64:03:00:00:01", Age: time.Second}, ip("192.168.1.40"): {MAC: "02:64:03:00:00:ff", Age: time.Second}}
	if code, _ = post("/ipam64/scan", url.Values{"_csrf": {csrf[1]}, "subnet": {"192.168.1.0/24"}}); code != http.StatusSeeOther || len(f.swept) != 1 {
		t.Fatalf("scan: %d", code)
	}
	code, body = get("/ipam64?net=192.168.1.0/24")
	for _, want := range []string{`id="ip-192.168.1.40"`, "switch-keller", "SNMP", "pi&lt;b&gt;hole&lt;/b&gt;", `class="ipam-cell ipam-conflict"`, `class="ipam-cell ipam-pool"`, "Router (Gateway)",
		"diese Node", "Netz-Scan", "Lokaler DNS", `id="ipam-conflicts">1<`, "4 von 254 belegt", `value="192.168.1.0/24"`} {
		if code != 200 || !strings.Contains(body, want) {
			t.Fatalf("overview: %d, missing %q\n%s", code, want, body)
		}
	}
	if strings.Contains(body, "ipam64.") || strings.Contains(body, "<b>hole") {
		t.Fatalf("untranslated key or unescaped name\n%s", body)
	}
	if code, body = get("/ipam64?net=192.168.1.0/24&kind=conflict"); code != 200 || !strings.Contains(body, "switch-keller") || strings.Contains(body, `id="ip-192.168.1.3"`) {
		t.Fatalf("filter: %d", code)
	}
	if code, body = get("/ipam64?net=192.168.1.0/24&q=HOLE"); code != 200 || !strings.Contains(body, `id="ip-192.168.1.3"`) || strings.Contains(body, `id="ip-192.168.1.40"`) {
		t.Fatalf("search: %d", code)
	}
	if code, body = get("/ipam64?net=192.168.1.0/24&form=addr&ip=192.168.1.3"); code != 200 || !strings.Contains(body, `name="ip" value="192.168.1.3"`) || !strings.Contains(body, `value="pi&lt;b&gt;hole&lt;/b&gt;"`) {
		t.Fatalf("adopt form: %d\n%s", code, body)
	}
	if code, body = get("/ipam64?net=192.168.1.0/24&form=net"); code != 200 || !strings.Contains(body, `name="subnet" value="192.168.1.0/24" placeholder="192.168.1.0/24" required readonly`) {
		t.Fatalf("edit form: %d\n%s", code, body)
	}
	if code, body = get("/ipam64/export.csv?net=192.168.1.0/24"); code != 200 || !strings.HasPrefix(body, "network,ip,name,") || !strings.Contains(body, "192.168.1.0/24,192.168.1.40,switch-keller,") ||
		strings.Count(body, "\n") != 5 {
		t.Fatalf("csv: %d\n%s", code, body)
	}
	if code, body = get("/api/ipam64/networks"); code != 200 || !strings.Contains(body, `"subnet":"192.168.1.0/24"`) || !strings.Contains(body, `"conflicts":1`) {
		t.Fatalf("api networks: %d %s", code, body)
	}
	if code, body = get("/api/ipam64/addresses?network=192.168.1.0/24"); code != 200 || !strings.Contains(body, `"ip":"192.168.1.40"`) || !strings.Contains(body, `"kind":"conflict"`) {
		t.Fatalf("api addresses: %d %s", code, body)
	}
	if code, _ = get("/api/ipam64/addresses?network=10.0.0.0/24"); code != http.StatusNotFound {
		t.Fatalf("api unknown network: %d", code)
	}
	if code, body = get("/"); code != 200 || !strings.Contains(body, `href="/ipam64"`) {
		t.Fatalf("dashboard: %d", code)
	}
}
