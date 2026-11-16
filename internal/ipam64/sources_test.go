package ipam64

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ipv64net/node64/internal/config"
	"github.com/ipv64net/node64/internal/i18n"
	"github.com/ipv64net/node64/internal/logging"
	"github.com/ipv64net/node64/internal/module"
	nodesync "github.com/ipv64net/node64/internal/sync"
	"github.com/ipv64net/node64/internal/web"
)

const srcBody = `{
	"label": "pfSense",
	"networks": [
		{"subnet": "192.168.178.1/24", "name": "LAN", "interface": "igb1", "router": "192.168.178.1", "range_start": "192.168.178.100", "range_end": "192.168.178.199"},
		{"subnet": "192.168.20.0/24", "name": "IoT", "interface": "igb1.20"},
		{"subnet": "fd00::/64", "name": "kein IPv4"},
		{"subnet": "192.168.20.0/24", "name": "doppelt"},
		{"subnet": "192.168.30.0/24", "router": "10.0.0.1"}
	],
	"reservations": [
		{"ip": "192.168.178.10", "mac": "02:64:03:00:00:0A", "name": "nas", "comment": "Synology"},
		{"ip": "fd00::10", "mac": "02:64:03:00:00:0b", "name": "v6"},
		{"ip": "192.168.178.12", "mac": "kaputt", "name": "x"}
	],
	"leases": [
		{"ip": "192.168.178.101", "mac": "02:64:03:00:00:65", "name": "macbook", "expires": %d, "last_seen": %d},
		{"ip": "192.168.178.102", "mac": "02:64:03:00:00:66", "name": "abgelaufen", "expires": %d},
		{"ip": "192.168.178.10", "mac": "02:64:03:00:00:0a", "name": "nas-lease", "expires": "2099-01-01T00:00:00Z"},
		{"ip": "192.168.20.51", "mac": "", "name": "sensor"}
	]
}`

// Fremder DHCP-Server: Die Meldung ersetzt den Stand, Ungueltiges wird gezaehlt, Netze und Vergaben erscheinen,
// DNS64 bleibt aussen vor (kein DHCP-Netz in DNS64).
func TestSourceUpdate(t *testing.T) {
	m, f, _ := modTest(t)
	ctx := context.Background()
	now := time.Now()
	in, err := DecodeSourceUpdate([]byte(fmt.Sprintf(srcBody, now.Add(time.Hour).Unix(), now.Add(-time.Minute).Unix(), now.Add(-time.Minute).Unix())))
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.UpdateSource(ctx, "pfsense", in)
	if err != nil || res.Networks != 2 || res.Reservations != 1 || res.Leases != 4 || res.Skipped != 5 || len(res.SkippedNotes) != 5 {
		t.Fatalf("update: %+v %v", res, err)
	}
	list, err := m.Sources(ctx)
	if err != nil || len(list) != 1 || list[0].Label != "pfSense" || list[0].Networks != 2 || list[0].Reservations != 1 || list[0].Leases != 3 || list[0].Stale {
		t.Fatalf("sources: %+v %v", list, err)
	}
	s, err := m.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lan, iot := s.Net("192.168.178.0/24"), s.Net("192.168.20.0/24")
	if len(s.Nets) != 2 || lan == nil || iot == nil || lan.DHCP || lan.ExtDHCP != "pfSense" || lan.UID != "" || lan.Name != "LAN" || lan.PoolStart != "192.168.178.100" ||
		lan.Router != "192.168.178.1" || lan.Used != 3 || lan.PoolFree != 99 || iot.VLAN != 20 || iot.PoolStart != "" {
		t.Fatalf("nets: %+v", s.Nets)
	}
	if s.Labels["dhcp:pfsense"] != "pfSense" {
		t.Fatalf("labels: %v", s.Labels)
	}
	nas := entry(t, s, "192.168.178.10")
	if nas == nil || nas.Kind != KindFixed || nas.Name != "nas" || nas.Note != "Synology" || nas.MAC != "02:64:03:00:00:0a" || strings.Join(nas.Sources, ",") != "dhcp:pfsense" ||
		nas.ExtLabel("dhcp:pfsense") != "pfSense" || nas.ExtLabel(SrcDHCP) != "" {
		t.Fatalf("nas: %+v", nas)
	}
	if e := entry(t, s, "192.168.178.1"); e == nil || e.NameKey != "ipam64.note.router" || e.Kind != KindStatic { // Gateway gilt als belegt
		t.Fatalf("router: %+v", e)
	}
	if e := entry(t, s, "192.168.178.101"); e == nil || e.Kind != KindDHCP || e.State != "online" || e.Name != "macbook" {
		t.Fatalf("lease: %+v", e)
	}
	if e := entry(t, s, "192.168.178.102"); e != nil {
		t.Fatalf("expired lease shown: %+v", e)
	}
	if e := entry(t, s, "192.168.20.51"); e == nil || e.MAC != "" || e.State != "unknown" {
		t.Fatalf("lease without mac: %+v", e)
	}
	// ein Geraet mit Lease der pfSense ist kein unbekanntes Geraet, eines ohne schon
	f.locals = append(f.locals, netip.MustParsePrefix("192.168.178.2/24"))
	f.tab = map[netip.Addr]Neighbor{ip("192.168.178.101"): {MAC: "02:64:03:00:00:65", Age: time.Second}, ip("192.168.178.50"): {MAC: "02:64:03:00:00:32", Age: time.Second}}
	if err := m.poll(ctx); err != nil {
		t.Fatal(err)
	}
	m.invalidate()
	unknown, err := m.Unknown(ctx)
	if err != nil || len(unknown) != 1 || unknown[0].IP != "192.168.178.50" {
		t.Fatalf("unknown: %+v %v", unknown, err)
	}
	// IPAM64 uebernimmt das Praefix: Name und VLAN aus IPAM64, Adressbereich weiter von der pfSense
	p := mustPrefix(t, m, Prefix{Prefix: "192.168.178.0/24", Name: "Heimnetz", VLAN: 1, Status: StatusActive, Scan: ScanPassive})
	if s, _ = m.Snapshot(ctx); s.Net("192.168.178.0/24").UID != p.UID || s.Net("192.168.178.0/24").Name != "Heimnetz" || s.Net("192.168.178.0/24").ExtDHCP != "pfSense" ||
		s.Net("192.168.178.0/24").PoolEnd != "192.168.178.199" || s.Net("192.168.178.0/24").DHCP {
		t.Fatalf("adopted: %+v", s.Net("192.168.178.0/24"))
	}
	// "naechste freie Adresse" meidet den Adressbereich der pfSense
	free, err := m.Available(ctx, "192.168.178.0/24", "", 300)
	if err != nil || len(free) == 0 {
		t.Fatalf("available: %d %v", len(free), err)
	}
	for _, x := range free {
		if (!x.Less(ip("192.168.178.100")) && !ip("192.168.178.199").Less(x)) || x == ip("192.168.178.1") {
			t.Fatalf("free address in the pool or on the gateway: %s", x)
		}
	}
	// naechste Meldung ersetzt alles
	res, err = m.UpdateSource(ctx, "pfsense", SourceUpdate{Networks: []SourceNetwork{{Subnet: "192.168.20.0/24"}}})
	if err != nil || res.Networks != 1 || res.Leases != 0 {
		t.Fatalf("replace: %+v %v", res, err)
	}
	if s, _ = m.Snapshot(ctx); s.Net("192.168.178.0/24") == nil || s.Net("192.168.178.0/24").ExtDHCP != "" || s.Labels["dhcp:pfsense"] != "pfsense" ||
		entry(t, s, "192.168.178.10") != nil {
		t.Fatalf("after replace: %+v", s.Nets)
	}
	m.now = func() time.Time { return now.Add(2 * time.Hour) }
	if list, _ = m.Sources(ctx); !list[0].Stale {
		t.Fatal("not stale")
	}
	m.now = time.Now
	if err := m.DeleteSource(ctx, "pfsense"); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteSource(ctx, "pfsense"); err != ErrNotFound {
		t.Fatalf("delete twice: %v", err)
	}
	if s, _ = m.Snapshot(ctx); len(s.Nets) != 1 || len(s.Labels) != 0 {
		t.Fatalf("after delete: %+v", s.Nets)
	}
}

// Ein Netz, das DNS64 schon bedient, bleibt ein DHCP-Netz von DNS64; die Vergaben der fremden Quelle zaehlen trotzdem.
func TestSourceBesideDNS64(t *testing.T) {
	m, _, _ := modTest(t)
	ctx := context.Background()
	m.SetNetworks(func(context.Context) []NetHint {
		return []NetHint{{Name: "LAN", Iface: "eth0", Subnet: netip.MustParsePrefix("192.168.1.0/24"), Start: ip("192.168.1.100"), End: ip("192.168.1.150"), Domain: "home.lan"}}
	})
	m.SetSource(SrcDHCP, func(context.Context) []Sighting {
		return []Sighting{{Addr: "192.168.1.101", MAC: "02:00:00:00:00:01", Name: "von-dns64", Kind: KindDHCP}}
	})
	m.SetSource(SrcDNS, func(context.Context) []Sighting { return []Sighting{{Addr: "192.168.1.200", Name: "aus-dns.home.lan"}} })
	if _, err := m.UpdateSource(ctx, "router", SourceUpdate{Networks: []SourceNetwork{{Subnet: "192.168.1.0/24", Name: "anders", RangeStart: "192.168.1.200", RangeEnd: "192.168.1.250"}},
		Leases: []SourceLease{{IP: "192.168.1.101", MAC: "02:00:00:00:00:01", Name: "vom-router"}, {IP: "192.168.1.200", MAC: "02:00:00:00:00:02", Name: "vom-router-2"}}}); err != nil {
		t.Fatal(err)
	}
	s, err := m.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := s.Net("192.168.1.0/24")
	if n == nil || !n.DHCP || n.ExtDHCP != "" || n.PoolStart != "192.168.1.100" || n.Name != "LAN" {
		t.Fatalf("net: %+v", n)
	}
	if e := entry(t, s, "192.168.1.101"); e == nil || e.Name != "von-dns64" || strings.Join(e.Sources, ",") != "dhcp,dhcp:router" {
		t.Fatalf("rank: %+v", e)
	}
	if e := entry(t, s, "192.168.1.200"); e == nil || e.Name != "vom-router-2" || e.Kind != KindDHCP || strings.Join(e.Sources, ",") != "dhcp:router,dns" {
		t.Fatalf("rank before dns: %+v", e)
	}
}

func TestSourceLimits(t *testing.T) {
	m, _, _ := modTest(t)
	ctx := context.Background()
	for _, n := range []string{"", "PfSense", "a b", strings.Repeat("a", 33), "../x"} {
		if _, err := m.UpdateSource(ctx, n, SourceUpdate{}); err != ErrSourceName {
			t.Errorf("name %q: %v", n, err)
		}
	}
	if _, err := m.UpdateSource(ctx, "x", SourceUpdate{Leases: make([]SourceLease, maxSrcLeases+1)}); err != ErrSourceSize {
		t.Fatalf("size: %v", err)
	}
	for i := range maxSources {
		if _, err := m.UpdateSource(ctx, "fw"+string(rune('a'+i)), SourceUpdate{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.UpdateSource(ctx, "noch-eine", SourceUpdate{}); err != ErrSourceLimit {
		t.Fatalf("limit: %v", err)
	}
	if _, err := m.UpdateSource(ctx, "fwa", SourceUpdate{}); err != nil { // bekannte Quelle darf weiter melden
		t.Fatal(err)
	}
	if _, err := DecodeSourceUpdate([]byte(`{"leases":[],"tippfehler":1}`)); !errors.Is(err, ErrSourceBody) {
		t.Fatalf("unknown field: %v", err)
	}
	var ft struct {
		A, B, C, D, E FlexTime
	}
	if err := json.Unmarshal([]byte(`{"A":1794817089,"B":"2026-11-16T12:00:00Z","C":"1794817089","D":0,"E":null}`), &ft); err != nil ||
		ft.A.Unix() != 1794817089 || ft.B.UTC().Hour() != 12 || ft.C.Unix() != 1794817089 || !ft.D.IsZero() || !ft.E.IsZero() {
		t.Fatalf("flextime: %+v %v", ft, err)
	}
	if err := json.Unmarshal([]byte(`{"A":"gestern"}`), &ft); err == nil {
		t.Fatal("bad time accepted")
	}
}

// API mit API-Key, Anzeige auf den Seiten, Skript zum Herunterladen, Entfernen.
func TestSourcePages(t *testing.T) {
	m, _, dir := modTest(t)
	st := m.core
	logger := logging.New("error", "text", io.Discard)
	bundle, err := i18n.Load()
	if err != nil {
		t.Fatal(err)
	}
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
	do := func(c *http.Client, method, path, body string, hdr map[string]string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	get := func(path string) (int, string) { return do(client, "GET", path, "", nil) }
	_, body := get("/setup")
	csrf := csrfRe.FindStringSubmatch(body)
	if csrf == nil {
		t.Fatal("no csrf")
	}
	post := func(path string, form url.Values) (int, string) {
		form.Set("_csrf", csrf[1])
		return do(client, "POST", path, form.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	}
	if code, _ := post("/setup", url.Values{"lang": {"de"}, "username": {"admin"}, "password": {"sehr geheimes passwort"},
		"password2": {"sehr geheimes passwort"}, "timezone": {"Europe/Berlin"}, "node_name": {"pi"}}); code != http.StatusSeeOther {
		t.Fatalf("setup: %d", code)
	}
	if err := reg.Enable(context.Background(), Name); err != nil {
		t.Fatal(err)
	}
	_, body = get("/")
	csrf = csrfRe.FindStringSubmatch(body)
	key, _, err := st.NewAPIKey(context.Background(), 1, "pfsense")
	if err != nil {
		t.Fatal(err)
	}
	api := func(method, path, body, auth string) (int, string) {
		hdr := map[string]string{"Content-Type": "application/json"}
		if auth != "" {
			hdr["Authorization"] = "Bearer " + auth
		}
		return do(http.DefaultClient, method, path, body, hdr)
	}
	put := `{"label":"pf<i>Sense","networks":[{"subnet":"192.168.20.0/24","name":"IoT","interface":"igb1.20","range_start":"192.168.20.50","range_end":"192.168.20.150"}],
		"reservations":[{"ip":"192.168.20.5","mac":"02:64:14:00:00:05","name":"kamera"}],"leases":[{"ip":"192.168.20.51","mac":"02:64:14:00:00:33","name":"sensor","expires":4102444800}]}`
	if code, _ := api("PUT", "/api/ipam64/sources/pfsense", put, ""); code == 200 {
		t.Fatalf("put without key: %d", code)
	}
	if code, body := api("PUT", "/api/ipam64/sources/pfsense", put, key); code != 200 || !strings.Contains(body, `"networks":1`) || !strings.Contains(body, `"leases":1`) || !strings.Contains(body, `"skipped":0`) {
		t.Fatalf("put: %d %s", code, body)
	}
	if code, body := api("PUT", "/api/ipam64/sources/pfsense", `{"lease":[]}`, key); code != http.StatusBadRequest {
		t.Fatalf("put unknown field: %d %s", code, body)
	}
	if code, body := api("PUT", "/api/ipam64/sources/Pf", `{}`, key); code != http.StatusBadRequest {
		t.Fatalf("put bad name: %d %s", code, body)
	}
	big := `{"leases":[` + strings.TrimSuffix(strings.Repeat(`{"ip":"192.168.20.9","mac":"02:64:14:00:00:09","name":"`+strings.Repeat("x", 300)+`"},`, 4000), ",") + `]}`
	if code, body := api("PUT", "/api/ipam64/sources/gross", big, key); code != 200 || !strings.Contains(body, `"leases":1`) || !strings.Contains(body, `"skipped":3999`) {
		t.Fatalf("put above 1 MB: %d %.200s", code, body) // der Kern laesst fuer diesen Pfad mehr als 1 MB zu
	}
	if code, body := api("GET", "/api/ipam64/sources", "", key); code != 200 || !strings.Contains(body, `"name":"pfsense"`) || !strings.Contains(body, `"reservations":1`) {
		t.Fatalf("sources: %d %s", code, body)
	}
	if code, body := api("GET", "/api/ipam64/prefixes", "", key); code != 200 || !strings.Contains(body, `"dhcp_server":"pf`) || strings.Contains(body, "pf<i>") || !strings.Contains(body, `"dhcp":false`) {
		t.Fatalf("prefixes: %d %s", code, body)
	}
	code, body := get("/ipam64")
	for _, want := range []string{`id="ipam-sources" open`, "pf&lt;i&gt;Sense", `action="/ipam64/source/delete"`, `href="/ipam64/collector/pfsense.php"`, `id="ipam-suggest"`, "DHCP-Netz von pf&lt;i&gt;Sense",
		"192.168.20.50–192.168.20.150", "gemeldet"} {
		if code != 200 || !strings.Contains(body, want) {
			t.Fatalf("overview: %d, missing %q\n%s", code, want, body)
		}
	}
	if untranslated(body) || strings.Contains(body, "pf<i>") {
		t.Fatalf("untranslated key or unescaped label\n%s", body)
	}
	code, body = get("/ipam64/prefix?p=192.168.20.0/24")
	for _, want := range []string{`id="ipam-ext-dhcp"`, "kamera", "sensor", ">pf&lt;i&gt;Sense</span>", "ipam-fixed"} {
		if code != 200 || !strings.Contains(body, want) {
			t.Fatalf("prefix: %d, missing %q\n%s", code, want, body)
		}
	}
	if untranslated(body) || strings.Contains(body, "/dns64/dhcp") {
		t.Fatalf("prefix page: untranslated key or link to DNS64\n%s", body)
	}
	if code, body = get("/ipam64/address?ip=192.168.20.5"); code != 200 || !strings.Contains(body, `id="ipam-ext-dhcp"`) || !strings.Contains(body, "Lege die feste Zuweisung dort an") || untranslated(body) {
		t.Fatalf("address: %d\n%s", code, body)
	}
	if code, body = get("/ipam64/collector/pfsense.php"); code != 200 || !strings.HasPrefix(body, "#!/usr/local/bin/php") {
		t.Fatalf("collector: %d", code)
	}
	if code, _ = post("/ipam64/source/delete", url.Values{"name": {"gross"}}); code != http.StatusSeeOther {
		t.Fatalf("delete on page: %d", code)
	}
	if code, _ = api("DELETE", "/api/ipam64/sources/pfsense", "", key); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ = api("DELETE", "/api/ipam64/sources/pfsense", "", key); code != http.StatusNotFound {
		t.Fatalf("delete twice: %d", code)
	}
	if code, body = get("/ipam64"); code != 200 || strings.Contains(body, `id="ipam-sources" open`) || strings.Contains(body, "192.168.20.0/24") {
		t.Fatalf("after delete: %d", code)
	}
}
