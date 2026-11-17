package ops64

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ipv64net/node64/internal/config"
	"github.com/ipv64net/node64/internal/i18n"
	"github.com/ipv64net/node64/internal/logging"
	"github.com/ipv64net/node64/internal/module"
	"github.com/ipv64net/node64/internal/notify"
	"github.com/ipv64net/node64/internal/proxmox"
	"github.com/ipv64net/node64/internal/store"
	nodesync "github.com/ipv64net/node64/internal/sync"
	"github.com/ipv64net/node64/internal/web"
)

var csrfRe = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

func berlin(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip(err)
	}
	return loc
}

// modTest: Modul mit leerer Datenbank; die Uhr steht auf Mittwoch, 18.11.2026, 10:30 Uhr (Berlin).
func modTest(t *testing.T) (*Module, *time.Time, string) {
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
	now := time.Date(2026, 11, 18, 10, 30, 0, 0, berlin(t))
	m.now = func() time.Time { return now }
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	return m, &now, dir
}

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04", s, berlin(t))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// ingestLinux meldet den Host pve-01 mit der Ausgabe des Sammlers (testdata/linux.json).
func ingestLinux(t *testing.T, m *Module) IngestResult {
	t.Helper()
	raw, err := os.ReadFile("testdata/linux.json")
	if err != nil {
		t.Fatal(err)
	}
	u, err := sampleUpdate(raw)
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.UpdateSource(context.Background(), "pve-01", u)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func jobByName(p *Plan, name string) *JobPlan {
	for _, j := range p.Jobs {
		if j.Name == name {
			return j
		}
	}
	return nil
}

// TestCollectorOutput: der Sammler liefert fuer die nachgestellten Ausgaben in testdata/linux genau testdata/linux.json
// (uebersprungen ohne python3; die Datei selbst prueft TestIngestAndPlan immer).
func TestCollectorOutput(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not found")
	}
	cmd := exec.Command(py, "-B", "collectors/linux.py", "--dry-run", "--config", "/nonexistent")
	cmd.Env = append(os.Environ(), "NODE64_OPS_FIXTURE=testdata/linux")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile("testdata/linux.json")
	var a, b any
	if json.Unmarshal(out, &a) != nil || json.Unmarshal(want, &b) != nil || !reflect.DeepEqual(a, b) {
		t.Fatalf("collector output differs from testdata/linux.json:\n%s", out)
	}
	if bytes.Contains(out, []byte("geheim123")) || bytes.Contains(out, []byte("pw123")) || bytes.Contains(out, []byte("abc123secret")) {
		t.Fatal("secret not masked")
	}
}

// TestIngestAndPlan: Meldung annehmen, Laeufe zuordnen, Zustaende, erwartete Dauer und gleichzeitige Aufgaben.
func TestIngestAndPlan(t *testing.T) {
	m, now, _ := modTest(t)
	ctx := context.Background()
	if res := ingestLinux(t, m); res.Jobs != 12 || res.Runs != 5 || res.Skipped != 0 {
		t.Fatalf("ingest: %+v", res)
	}
	day := at(t, "2026-11-18 00:00")
	p, err := m.Plan(ctx, day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Sources) != 1 || p.Sources[0].Host() != "pve-01" || p.Sources[0].Jobs != 12 || p.Sources[0].Timezone != "Europe/Berlin" {
		t.Fatalf("sources: %+v", p.Sources)
	}
	// Backup-Job 01:00: der Lauf ohne Aufgabe (vzdump-Aufgabe von 01:00:02) gehoert zu ihm
	b := jobByName(p, "Backup VMs")
	if b == nil || len(b.Items) != 1 || b.Items[0].Status != StOK || !b.Items[0].Planned.Equal(at(t, "2026-11-18 01:00")) ||
		b.Items[0].End.Sub(b.Items[0].Start) != 72*time.Minute-2*time.Second {
		t.Fatalf("backup: %+v", b)
	}
	if b.Expected != 72*time.Minute-2*time.Second || b.ExpectedFrom != "runs" || b.Last == nil || b.Last.Status != RunOK || b.Overdue {
		t.Fatalf("backup expected: %v %s", b.Expected, b.ExpectedFrom)
	}
	if n := b.Next; !n.Equal(at(t, "2026-11-19 01:00")) {
		t.Fatalf("next: %v", n)
	}
	// abgeschaltet, System-Aufgabe, haeufig, ohne festen Zeitpunkt: nicht im Zeitplan
	for name, why := range map[string]string{"vzdump 101,102": "disabled", "apt-daily": "system", "curl": "frequent", "mount-check": "never", "old": "no schedule"} {
		if j := jobByName(p, name); j == nil || j.Planned() || len(j.Items) != 0 {
			t.Fatalf("%s (%s) is planned", name, why)
		}
	}
	if j := jobByName(p, "curl"); !j.Frequent || j.Shown() != true {
		t.Fatal("curl not frequent")
	}
	// Timer mit fehlgeschlagenem letztem Lauf
	if j := jobByName(p, "snapshot-prune"); j == nil || len(j.Items) != 1 || j.Items[0].Status != StFail || j.Items[0].Run.Detail != "Result=exit-code, exit 1" {
		t.Fatalf("timer: %+v", j)
	}
	// Cronjob: die Quelle meldet keine Laeufe, also weder erfolgreich noch ueberfaellig; Laufzeit unklar
	r := jobByName(p, "rsync-medien.sh")
	if r == nil || len(r.Items) != 1 || r.Items[0].Status != StUnknown || !r.Items[0].Unclear || strings.Contains(r.Command, "geheim") {
		t.Fatalf("cron: %+v", r)
	}
	// Backup von Hand laeuft: Lauf ohne Aufgabe
	if len(p.Extra) != 1 || p.Extra[0].Name != "vzdump 104" || p.Extra[0].Status != StRunning || !p.Extra[0].End.Equal(*now) {
		t.Fatalf("extra: %+v", p.Extra)
	}
	// gleichzeitig: rsync (01:30, angenommen 30 Minuten) faellt in das Backup (01:00-02:12)
	if len(p.Overlaps) != 1 || !p.Overlaps[0].Start.Equal(at(t, "2026-11-18 01:30")) || !p.Overlaps[0].End.Equal(at(t, "2026-11-18 02:00")) ||
		strings.Join(p.Overlaps[0].Names, "|") != "Backup VMs (pve-01)|rsync-medien.sh (pve-01)" {
		t.Fatalf("overlaps: %+v", p.Overlaps)
	}
	// Vortag: der Lauf vom 17.11. ist fehlgeschlagen
	y, err := m.Plan(ctx, day.AddDate(0, 0, -1), day)
	if err != nil {
		t.Fatal(err)
	}
	if b := jobByName(y, "Backup VMs"); len(b.Items) != 1 || b.Items[0].Status != StFail || b.Items[0].Run.Detail != "job errors" {
		t.Fatalf("yesterday: %+v", b.Items)
	}
	// morgen: geplant, mit der erwarteten Dauer
	tm, _ := m.Plan(ctx, day.AddDate(0, 0, 1), day.AddDate(0, 0, 2))
	if b := jobByName(tm, "Backup VMs"); len(b.Items) != 1 || b.Items[0].Status != StPlanned || b.Items[0].End.Sub(b.Items[0].Start) != b.Expected {
		t.Fatalf("tomorrow: %+v", b.Items)
	}

	// Einstellungen: von Hand gesetzte Dauer zaehlt, ausgeblendete Aufgabe verschwindet, System-Aufgabe laesst sich zeigen
	if err := m.SetPrefs(ctx, "pve-01", r.ID, "", 90*60); err != nil {
		t.Fatal(err)
	}
	if err := m.SetPrefs(ctx, "pve-01", b.ID, VisibleHide, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.SetPrefs(ctx, "pve-01", "timer-apt-daily.timer", VisibleShow, 0); err != nil {
		t.Fatal(err)
	}
	if m.SetPrefs(ctx, "pve-01", "nope", "", 0) != ErrNotFound || m.SetPrefs(ctx, "pve-01", r.ID, "x", 0) != ErrInput {
		t.Fatal("prefs: no error")
	}
	p, _ = m.Plan(ctx, day, day.AddDate(0, 0, 1))
	if r := jobByName(p, "rsync-medien.sh"); r.Expected != 90*time.Minute || r.ExpectedFrom != "set" || r.Items[0].Unclear {
		t.Fatalf("expected set: %+v", r)
	}
	if b := jobByName(p, "Backup VMs"); b.Planned() || len(p.Overlaps) != 0 {
		t.Fatalf("hidden job still planned (%d overlaps)", len(p.Overlaps))
	}
	if a := jobByName(p, "apt-daily"); !a.Planned() || len(a.Items) != 2 || a.Items[0].Status != StOK || a.Items[1].Status != StPlanned {
		t.Fatalf("apt-daily: %+v", a.Items)
	}

	// naechste Meldung: der laufende Lauf ist fertig, eine Aufgabe fehlt; Einstellungen und alte Laeufe bleiben
	raw, _ := os.ReadFile("testdata/linux.json")
	u, _ := sampleUpdate(raw)
	u.Runs[0].End, u.Runs[0].Status = u.Runs[0].Start+600, RunOK
	u.Jobs = u.Jobs[1:]
	*now = now.Add(15 * time.Minute)
	if res, err := m.UpdateSource(ctx, "pve-01", u); err != nil || res.Jobs != 11 {
		t.Fatalf("second ingest: %+v %v", res, err)
	}
	p, _ = m.Plan(ctx, day, day.AddDate(0, 0, 1))
	if jobByName(p, "Backup VMs") != nil || len(p.Extra) != 2 { // der Lauf des verschwundenen Jobs steht jetzt ohne Aufgabe da
		t.Fatalf("after second ingest: %d extra", len(p.Extra))
	}
	for _, it := range p.Extra {
		if it.Name == "vzdump 104" && (it.Status != StOK || it.End.Sub(it.Start) != 10*time.Minute) {
			t.Fatalf("manual backup: %+v", it)
		}
	}
	if r := jobByName(p, "rsync-medien.sh"); r.ExpectedSet != 90*60 {
		t.Fatal("prefs lost")
	}

	if err := m.DeleteSource(ctx, "pve-01"); err != nil {
		t.Fatal(err)
	}
	if m.DeleteSource(ctx, "pve-01") != ErrNotFound {
		t.Fatal("delete twice")
	}
	if p, _ = m.Plan(ctx, day, day.AddDate(0, 0, 1)); len(p.Jobs) != 0 || len(p.Extra) != 0 || len(p.Sources) != 0 {
		t.Fatal("source not gone")
	}
}

func boolp(b bool) *bool { return &b }

// TestOverdueLongAndAlerts: ueberfaellig nur mit gemeldeten Laeufen und frischer Quelle; langer Lauf; ein Lauf, dessen
// Ende nie gemeldet wird; Meldungen einmal, Zustaende nach dem Ende erneut.
func TestOverdueLongAndAlerts(t *testing.T) {
	m, now, _ := modTest(t)
	ctx := context.Background()
	var sent []notify.Alert
	m.SetNotifier(func(a notify.Alert) { sent = append(sent, a) })
	run := func(start string, min int, status string) SourceRun {
		s := at(t, start).Unix()
		if min == 0 {
			return SourceRun{Start: s}
		}
		return SourceRun{Start: s, End: s + int64(min)*60, Status: status}
	}
	upd := func() *SourceUpdate {
		return &SourceUpdate{Label: "pve-02", Timezone: "Europe/Berlin", Jobs: []SourceJob{
			{ID: "sync", Name: "Offsite-Sync", Kind: KindBackup, Schedule: "03:30", History: true, Target: "Standort B",
				Runs: []SourceRun{run("2026-11-15 03:30", 60, RunOK), run("2026-11-16 03:30", 62, RunOK), run("2026-11-17 03:30", 58, RunOK)}},
			{ID: "lxc", Name: "Backup LXC", Kind: KindBackup, Schedule: "0 9 * * *", History: true,
				Runs: []SourceRun{run("2026-11-16 09:00", 20, RunOK), run("2026-11-17 09:00", 20, RunOK), run("2026-11-18 09:00", 0, "")}},
			{ID: "off", Name: "Aus", Kind: KindCron, Schedule: "0 4 * * *", History: true, Enabled: boolp(false)},
			{ID: "bad", Name: "", Kind: "nonsense"},
		}}
	}
	// erste Meldung am 14.11.: davor gibt es kein "ueberfaellig"
	*now = at(t, "2026-11-14 12:00")
	if res, err := m.UpdateSource(ctx, "pve-02", upd()); err != nil || res.Jobs != 3 || res.Skipped != 1+6 { // Laeufe in der Zukunft zaehlen nicht
		t.Fatalf("ingest: %+v %v", res, err)
	}
	*now = at(t, "2026-11-18 10:30")
	if _, err := m.UpdateSource(ctx, "pve-02", upd()); err != nil {
		t.Fatal(err)
	}
	day := at(t, "2026-11-18 00:00")
	p, err := m.Plan(ctx, day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	s := jobByName(p, "Offsite-Sync")
	if s == nil || len(s.Items) != 1 || s.Items[0].Status != StOverdue || !s.Overdue || s.Expected != time.Hour {
		t.Fatalf("overdue: %+v", s)
	}
	l := jobByName(p, "Backup LXC")
	if len(l.Items) != 1 || l.Items[0].Status != StRunning || !l.Items[0].Long || l.Expected != 20*time.Minute {
		t.Fatalf("long: %+v", l.Items[0])
	}
	if len(p.Overlaps) != 0 { // ueberfaellig zaehlt nicht als gleichzeitig
		t.Fatalf("overlaps: %+v", p.Overlaps)
	}
	if err := m.CheckAlerts(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ := m.Alerts(ctx, 10)
	kinds := map[string]int{}
	for _, a := range list {
		kinds[a.Kind]++
	}
	if len(list) != 2 || kinds[AlertOverdue] != 1 || kinds[AlertLong] != 1 {
		t.Fatalf("alerts: %+v", list)
	}
	// verschickt wird nur Frisches: der lange Lauf, nicht die seit 03:40 ueberfaellige Aufgabe
	if len(sent) != 1 || sent[0].Kind != notify.KindOps || sent[0].Data["text"] != "notify.text.ops64_long" || sent[0].Data["duration"] != "1 h 30 min" ||
		sent[0].Data["expected"] != "20 min" || sent[0].Check.Name != "Backup LXC" {
		t.Fatalf("sent: %+v", sent)
	}
	if err := m.CheckAlerts(ctx); err != nil || len(sent) != 1 {
		t.Fatalf("sent twice: %d %v", len(sent), err)
	}
	// der Host meldet sich nicht mehr: nach einer Stunde Meldung; solange gibt es kein neues "ueberfaellig"
	*now = at(t, "2026-11-19 04:00")
	p, _ = m.Plan(ctx, day.AddDate(0, 0, 1), day.AddDate(0, 0, 2))
	if s := jobByName(p, "Offsite-Sync"); s.Items[0].Status != StUnknown {
		t.Fatalf("stale source: %s", s.Items[0].Status)
	}
	if err := m.CheckAlerts(ctx); err != nil || len(sent) != 2 || sent[1].Data["text"] != "notify.text.ops64_stale" || sent[1].Data["host"] != "pve-02" {
		t.Fatalf("stale: %+v %v", sent, err)
	}
	// er meldet sich wieder, der lange Lauf fehlt in der Meldung: Ende unbekannt, Zustand "stumm" vorbei
	u := upd()
	u.Jobs[1].Runs = u.Jobs[1].Runs[:2]
	if _, err := m.UpdateSource(ctx, "pve-02", u); err != nil {
		t.Fatal(err)
	}
	runs, _ := m.RunsOf(ctx, "pve-02", "lxc", 5)
	if len(runs) != 3 || runs[0].Status != RunUnknown || !runs[0].End.Equal(*now) {
		t.Fatalf("lost run: %+v", runs)
	}
	if err := m.CheckAlerts(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ = m.Alerts(ctx, 10)
	for _, a := range list {
		if a.Kind == AlertStale && a.Open {
			t.Fatal("stale still open")
		}
	}
	// abgeschaltete Benachrichtigung: steht in der Liste, wird nicht verschickt
	if err := m.SetNotify(ctx, AlertFail, false); err != nil || m.SetNotify(ctx, "x", true) != ErrInput {
		t.Fatal("set notify")
	}
	u = upd()
	u.Jobs[0].Runs = append(u.Jobs[0].Runs, run("2026-11-19 03:30", 5, RunFail))
	n := len(sent)
	if _, err := m.UpdateSource(ctx, "pve-02", u); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckAlerts(ctx); err != nil {
		t.Fatal(err)
	}
	list, _ = m.Alerts(ctx, 10)
	failed := 0
	for _, a := range list {
		if a.Kind == AlertFail && a.Data["job"] == "Offsite-Sync" {
			failed++
		}
	}
	if failed != 1 || len(sent) != n {
		t.Fatalf("fail alert: %+v, sent %d", list, len(sent)-n)
	}
}

// TestOverlapAlert: gleichzeitig geplante Aufgaben zweier Hosts werden einmal gemeldet.
func TestOverlapAlert(t *testing.T) {
	m, _, _ := modTest(t)
	ctx := context.Background()
	var sent []notify.Alert
	m.SetNotifier(func(a notify.Alert) { sent = append(sent, a) })
	for host, sched := range map[string]string{"host-a": "0 22 * * *", "host-b": "22:20"} {
		if _, err := m.UpdateSource(ctx, host, &SourceUpdate{Jobs: []SourceJob{{ID: "j", Name: "Job " + host, Kind: KindCron, Schedule: sched, Expected: 3600}}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := m.CheckAlerts(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(sent) != 1 || sent[0].Data["text"] != "notify.text.ops64_overlap" || sent[0].Data["jobs"] != "Job host-a (host-a), Job host-b (host-b)" ||
		sent[0].Data["time"] != "18.11. 22:20–23:00" {
		t.Fatalf("sent: %+v", sent)
	}
}

func TestIngestLimits(t *testing.T) {
	m, _, _ := modTest(t)
	ctx := context.Background()
	for _, bad := range []string{"", "Host", "a b", "-x", strings.Repeat("a", 64)} {
		if _, err := m.UpdateSource(ctx, bad, &SourceUpdate{}); err != ErrInput {
			t.Errorf("name %q: %v", bad, err)
		}
	}
	for _, raw := range []string{`{"jobs":[{"id":"a","kind":"cron","nope":1}]}`, `{"jobs":[]} x`, `[]`, ``} {
		if _, err := sampleUpdate([]byte(raw)); err != ErrInput {
			t.Errorf("%q: %v", raw, err)
		}
	}
	u, err := sampleUpdate([]byte(`{"label":"<b>x</b>","timezone":"Mars/Phobos","jobs":[{"id":"a","kind":"cron","name":"A\u0007B","schedule":"* * * * *"},{"id":"a","kind":"cron"}],
		"runs":[{"kind":"cron","start":1,"end":0},{"kind":"x","start":1}]}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := m.UpdateSource(ctx, "h1", u)
	if err != nil || res.Jobs != 1 || res.Skipped != 2 || res.Runs != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	jobs, _ := m.Jobs(ctx)
	if len(jobs) != 1 || jobs[0].Name != "A B" {
		t.Fatalf("jobs: %+v", jobs)
	}
	if src, _ := m.Sources(ctx); src[0].Timezone != "" || src[0].Label != "<b>x</b>" {
		t.Fatalf("source: %+v", src[0])
	}
}

// TestPagesRender: Seite, Formulare und API ueber den echten Webserver (Modul aus = 404).
func TestPagesRender(t *testing.T) {
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
	var csrfTok string
	do := func(method, path, body, ctype string) (int, string) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		if csrfTok != "" && method != "GET" {
			req.Header.Set("X-CSRF-Token", csrfTok)
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
		form.Set("_csrf", csrfTok)
		return do("POST", path, form.Encode(), "application/x-www-form-urlencoded")
	}
	_, body := get("/setup")
	csrf := csrfRe.FindStringSubmatch(body)
	if csrf == nil {
		t.Fatal("no csrf")
	}
	csrfTok = csrf[1]
	if code, _ := post("/setup", url.Values{"lang": {"de"}, "username": {"admin"}, "password": {"sehr geheimes passwort"},
		"password2": {"sehr geheimes passwort"}, "timezone": {"Europe/Berlin"}, "node_name": {"pi"}}); code != http.StatusSeeOther {
		t.Fatalf("setup: %d", code)
	}
	raw, _ := os.ReadFile("testdata/linux.json")
	for _, p := range []string{"/ops64", "/api/ops64/sources", "/api/ops64/plan", "/ops64/collector/linux.py"} {
		if code, _ := get(p); code != http.StatusNotFound {
			t.Fatalf("module off %s: %d", p, code)
		}
	}
	_, body = get("/")
	csrfTok = csrfRe.FindStringSubmatch(body)[1]
	if code, _ := do("PUT", "/api/ops64/sources/pve-01", string(raw), "application/json"); code != http.StatusNotFound {
		t.Fatalf("module off put: %d", code)
	}
	if err := reg.Enable(context.Background(), Name); err != nil {
		t.Fatal(err)
	}
	if _, body = get("/"); !strings.Contains(body, `href="/ops64"`) {
		t.Fatal("no menu entry")
	}
	untranslated := func(b string) bool {
		return strings.Contains(strings.NewReplacer("/api/ops64/", "", "/ops64", "").Replace(b), "ops64.")
	}
	code, body := get("/ops64")
	if code != 200 || !strings.Contains(body, `id="ops-guide"`) || !strings.Contains(body, `id="ops-sources-empty"`) || strings.Contains(body, `id="ops-plan"`) || untranslated(body) {
		t.Fatalf("empty page: %d\n%s", code, body)
	}
	for _, p := range []string{"/ops64/collector/linux.py", "/api/ops64/collector/linux.py"} {
		if code, body = get(p); code != 200 || !strings.HasPrefix(body, "#!/usr/bin/env python3") {
			t.Fatalf("collector %s: %d", p, code)
		}
	}
	// Meldung ueber die API (mit Sitzung), Fehlerfaelle
	if code, body = do("PUT", "/api/ops64/sources/pve-01", string(raw), "application/json"); code != 200 || !strings.Contains(body, `"jobs":12`) {
		t.Fatalf("put: %d %s", code, body)
	}
	if code, _ = do("PUT", "/api/ops64/sources/pve-01", `{"nope":1}`, "application/json"); code != http.StatusBadRequest {
		t.Fatalf("bad put: %d", code)
	}
	if code, _ = do("PUT", "/api/ops64/sources/Bad_Name", `{}`, "application/json"); code != http.StatusBadRequest {
		t.Fatalf("bad name: %d", code)
	}
	if code, body = get("/api/ops64/sources"); code != 200 || !strings.Contains(body, `"name":"pve-01"`) {
		t.Fatalf("sources: %d %s", code, body)
	}
	if code, body = get("/api/ops64/jobs"); code != 200 || !strings.Contains(body, `"name":"Backup VMs"`) || !strings.Contains(body, `"expected_from":"runs"`) || strings.Contains(body, "geheim") {
		t.Fatalf("jobs: %d %s", code, body)
	}
	if code, body = get("/api/ops64/plan?from=2026-11-18"); code != 200 || !strings.Contains(body, `"status":"ok"`) || !strings.Contains(body, `"overlaps":[{`) {
		t.Fatalf("plan: %d %s", code, body)
	}
	if code, _ = get("/api/ops64/plan?from=2026-11-18&to=2027-11-18"); code != http.StatusBadRequest {
		t.Fatalf("plan range: %d", code)
	}
	// Woche mit Balken, Zustand je Wochentag, gleichzeitig, Kennzahlen
	code, body = get("/ops64")
	for _, want := range []string{`id="ops-plan"`, `id="ops-timeline"`, "Backup VMs", "pve-01 → pbs-standort-b · täglich 01:00", "ops-bar ops-st-ok", "01:00–02:12", "ops-bar ops-st-running",
		"ops-bar ops-st-unknown ops-unclear", "ops-cell ops-st-fail", `class="ops-band"`, `class="ops-now"`, "Gleichzeitig 01:30–02:00", "KW 47 · 16.11.–22.11.2026",
		"Läuft häufig", `id="ops-kpi-running">1<`, `id="ops-kpi-overlaps">1<`, `id="ops-kpi-failed">2<`, "außer der Reihe", `id="ops-area-table"`} {
		if !strings.Contains(body, want) {
			t.Errorf("week: missing %q", want)
		}
	}
	if code != 200 || untranslated(body) || strings.Contains(body, "geheim123") || t.Failed() {
		t.Fatalf("week: %d\n%s", code, body)
	}
	if code, body = get("/ops64?view=day&d=2026-11-17&win=full"); code != 200 || !strings.Contains(body, "ops-tl-day") || !strings.Contains(body, "ops-bar ops-st-fail") ||
		!strings.Contains(body, "Di 17.11.2026") || untranslated(body) {
		t.Fatalf("day: %d\n%s", code, body)
	}
	if code, body = get("/ops64?view=month&d=2026-11-18"); code != 200 || !strings.Contains(body, `id="ops-month"`) || !strings.Contains(body, "November 2026") ||
		!strings.Contains(body, "1 Fehler") || !strings.Contains(body, "ops-today") || untranslated(body) {
		t.Fatalf("month: %d\n%s", code, body)
	}
	if code, body = get("/ops64?kind=timer&status=fail"); code != 200 || !strings.Contains(body, "snapshot-prune") || strings.Contains(body, `class="ops-task" href="/ops64?kind=timer&amp;sel=pve-01%7Cvzdump`) {
		t.Fatalf("filter: %d", code)
	}
	if !strings.Contains(body, "1 von 1 Aufgaben") {
		t.Fatalf("filter count\n%s", body)
	}
	// Auswahl mit Verlauf, Einstellungen speichern
	sel := url.QueryEscape("pve-01|vzdump-backup-1a2b3c4d-0001")
	if code, body = get("/ops64?sel=" + sel); code != 200 || !strings.Contains(body, `id="ops-sel-name">Backup VMs<`) || !strings.Contains(body, `id="ops-sel-runs"`) ||
		!strings.Contains(body, "job errors") || !strings.Contains(body, `id="ops-sel-overlap"`) || !strings.Contains(body, "1 h 12 min") || untranslated(body) {
		t.Fatalf("selection: %d\n%s", code, body)
	}
	if code, _ = post("/ops64/job", url.Values{"source": {"pve-01"}, "job": {"vzdump-backup-1a2b3c4d-0001"}, "expected": {"45"}, "visible": {"show"}, "back": {"/ops64?sel=" + sel}}); code != http.StatusSeeOther {
		t.Fatalf("job save: %d", code)
	}
	if _, body = get("/ops64?sel=" + sel); !strings.Contains(body, "45 min") || !strings.Contains(body, "von Hand") {
		t.Fatalf("job saved\n%s", body)
	}
	if code, _ = post("/ops64/job", url.Values{"source": {"pve-01"}, "job": {"nope"}, "expected": {"abc"}}); code != http.StatusSeeOther {
		t.Fatalf("bad job save: %d", code)
	}
	if code, body = get("/ops64?tab=tasks&sys=1"); code != 200 || !strings.Contains(body, "apt-daily") || !strings.Contains(body, "--password=***") || untranslated(body) {
		t.Fatalf("tasks: %d\n%s", code, body)
	}
	// Meldungen und Schalter
	if err := m.CheckAlerts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code, _ = post("/ops64/notify", url.Values{"fail": {"1"}}); code != http.StatusSeeOther || m.NotifyOn(context.Background(), AlertOverdue) || !m.NotifyOn(context.Background(), AlertFail) {
		t.Fatalf("notify: %d", code)
	}
	if _, body = get("/ops64"); !strings.Contains(body, "snapshot-prune auf pve-01 ist fehlgeschlagen") || !strings.Contains(body, "Gleichzeitig geplant") || untranslated(body) {
		t.Fatalf("alerts\n%s", body)
	}
	// Host entfernen
	if code, _ = post("/ops64/source/delete", url.Values{"name": {"pve-01"}}); code != http.StatusSeeOther {
		t.Fatalf("delete: %d", code)
	}
	if code, _ = do("DELETE", "/api/ops64/sources/pve-01", "", ""); code != http.StatusNotFound {
		t.Fatalf("delete again: %d", code)
	}
	if _, body = get("/ops64"); !strings.Contains(body, `id="ops-sources-empty"`) {
		t.Fatal("source still listed")
	}
	// Einzeiler fuer die Shell des Hosts; Abruf ueber die Proxmox-API: ohne Monitor64 nicht einschaltbar
	if !strings.Contains(body, `id="ops-oneliner"`) || !strings.Contains(body, srv.URL) || !strings.Contains(body, "$U/api/ops64/collector/linux.py") || strings.Contains(body, "skip = pve,pbs") ||
		!strings.Contains(body, `id="ops-api-none"`) || !strings.Contains(body, `id="ops-api-on" disabled`) {
		t.Fatalf("guide and api card\n%s", body)
	}
	api := &fakeAPI{data: map[string]string{
		"/cluster/backup": `[{"id":"backup-0001","schedule":"01:00","storage":"pbs-standort-b","all":1,"comment":"Backup VMs"}]`,
		"/nodes/pve-01/tasks?typefilter=vzdump&source=all&limit=200": `[]`,
	}}
	m.newGetter = func(proxmox.System) (getter, error) { return api, nil }
	m.SetAPIAccesses(func(context.Context) ([]APIAccess, bool) {
		return []APIAccess{{UID: "u1", Host: "pve-01", Node: "pve-01", System: proxmox.System{Kind: proxmox.KindPVE}}}, true
	})
	if _, body = get("/ops64"); !strings.Contains(body, `id="ops-api-table"`) || strings.Contains(body, `id="ops-api-on" disabled`) || untranslated(body) {
		t.Fatalf("api card with access\n%s", body)
	}
	if code, _ = post("/ops64/api", url.Values{"action": {"on"}}); code != http.StatusSeeOther || !m.APIOn(context.Background()) {
		t.Fatalf("api on: %d", code)
	}
	if _, body = get("/ops64"); !strings.Contains(body, "1 Backup-Jobs gelesen") || !strings.Contains(body, "Abruf über die Proxmox-API") || !strings.Contains(body, "skip = pve,pbs") ||
		!strings.Contains(body, `id="ops-api-poll"`) || !strings.Contains(body, "Backup VMs") || untranslated(body) {
		t.Fatalf("api polled\n%s", body)
	}
	for _, p := range api.seen { // nur lesende Abfragen, keine Geheimnisse in der Seite
		if !strings.HasPrefix(p, "/cluster/") && !strings.HasPrefix(p, "/nodes/") {
			t.Fatalf("unexpected api path %s", p)
		}
	}
	delete(api.data, "/cluster/backup")
	if code, _ = post("/ops64/api", url.Values{"action": {"poll"}}); code != http.StatusSeeOther {
		t.Fatalf("api poll: %d", code)
	}
	if _, body = get("/ops64"); !strings.Contains(body, "permission denied") {
		t.Fatalf("api error not shown\n%s", body)
	}
	if code, _ = post("/ops64/api", url.Values{"action": {"off"}}); code != http.StatusSeeOther || m.APIOn(context.Background()) {
		t.Fatalf("api off: %d", code)
	}
}

// fakeAPI spielt die Proxmox-API: Antworten je Pfad (JSON des Feldes "data"), fehlende Pfade sind ein Fehler.
type fakeAPI struct {
	data map[string]string
	seen []string
}

func (f *fakeAPI) Get(_ context.Context, path string, v any) error {
	f.seen = append(f.seen, path)
	raw, ok := f.data[path]
	if !ok {
		return &proxmox.Error{Kind: proxmox.ErrPermission, Status: 403, Detail: "permission check failed"}
	}
	return json.Unmarshal([]byte(raw), v)
}

// TestAPIPoll: Backup-Jobs und Laeufe ueber die API von Proxmox VE und Proxmox Backup Server (nachgestellte Antworten).
func TestAPIPoll(t *testing.T) {
	m, now, _ := modTest(t)
	ctx := context.Background()
	ts := func(s string) string { return strconv.FormatInt(at(t, s).Unix(), 10) }
	hex := func(s string) string { return strings.ToUpper(strconv.FormatInt(at(t, s).Unix(), 16)) }
	pve := &fakeAPI{data: map[string]string{
		"/cluster/status": `[{"type":"cluster","name":"lab","quorate":1},{"type":"node","name":"pve-02","local":0},{"type":"node","name":"pve-01","local":1}]`,
		"/cluster/backup": `[{"id":"backup-0001","schedule":"01:00","storage":"pbs-standort-b","all":1,"enabled":1,"mode":"snapshot","comment":"Backup VMs"},
			{"id":"backup-0002","schedule":"sat 03:30","storage":"local","vmid":"101,102","enabled":"0"},
			{"id":"backup-0003","schedule":"02:00","storage":"local","all":1,"node":"pve-02"},
			{"id":"backup-0004","starttime":"04:15","dow":"mon,wed","storage":"local","pool":"prod"}]`,
		"/nodes/pve-01/tasks?typefilter=vzdump&source=all&limit=200": `[
			{"upid":"UPID:pve-01:1","starttime":` + ts("2026-11-18 01:00") + `,"endtime":` + ts("2026-11-18 02:10") + `,"status":"OK","id":""},
			{"upid":"UPID:pve-01:2","starttime":"` + ts("2026-11-17 01:00") + `","endtime":` + ts("2026-11-17 01:05") + `,"status":"job errors"},
			{"upid":"UPID:pve-01:3","starttime":` + ts("2026-11-18 10:10") + `,"id":104}]`,
		"/nodes/pve-01/time": `{"timezone":"Europe/Berlin","time":1,"localtime":1}`,
	}}
	pbsUPID := func(start, typ, wid string) string {
		return "UPID:pbs:00000A:00000B:00000001:" + hex(start) + ":" + typ + ":" + wid + ":root@pam:"
	}
	pbs := &fakeAPI{data: map[string]string{
		"/admin/sync": `[{"id":"s-1","store":"tank","remote":"standort-a","remote-store":"main","schedule":"03:30","comment":"Offsite-Sync",
			"last-run-upid":"` + pbsUPID("2026-11-18 03:30", "syncjob", "standort-a:main:tank::s-1") + `","last-run-state":"OK","last-run-endtime":` + ts("2026-11-18 04:58") + `}]`,
		"/admin/verify": `[{"id":"v-1","store":"tank","schedule":"sat 10:00","last-run-upid":"` + pbsUPID("2026-11-18 10:00", "verificationjob", "tank:v-1") + `"}]`,
		"/admin/gc":     `[{"store":"tank","schedule":"daily","last-run-upid":"` + pbsUPID("2026-11-18 00:00", "garbage_collection", "tank") + `","last-run-state":"WARNINGS: 2","last-run-endtime":` + ts("2026-11-18 00:20") + `}]`,
		"/nodes/localhost/tasks?limit=300": `[
			{"upid":"` + pbsUPID("2026-11-18 03:30", "syncjob", "standort-a:main:tank::s-1") + `","worker_type":"syncjob","worker_id":"standort-a:main:tank::s-1","starttime":` + ts("2026-11-18 03:30") + `,"endtime":` + ts("2026-11-18 04:58") + `,"status":"OK"},
			{"upid":"` + pbsUPID("2026-11-17 03:30", "syncjob", "standort-a:main:tank::s-1") + `","worker_type":"syncjob","worker_id":"standort-a:main:tank::s-1","starttime":` + ts("2026-11-17 03:30") + `,"endtime":` + ts("2026-11-17 03:31") + `,"status":"connection error"},
			{"upid":"x","worker_type":"backup","worker_id":"tank:vm/100","starttime":` + ts("2026-11-18 01:00") + `,"endtime":` + ts("2026-11-18 01:10") + `,"status":"OK"}]`,
		"/nodes/localhost/time": `{"timezone":"Europe/Berlin"}`,
	}}
	bad := &fakeAPI{data: map[string]string{}}
	clients := map[string]*fakeAPI{"https://pve": pve, "https://pbs": pbs, "https://bad": bad}
	m.newGetter = func(sys proxmox.System) (getter, error) { return clients[sys.Addresses[0]], nil }
	m64 := true
	acc := []APIAccess{
		{UID: "A1-b2", Host: "pve-01", System: proxmox.System{Kind: proxmox.KindPVE, Addresses: []string{"https://pve"}}},
		{UID: "c3d4", Host: "Standort B", System: proxmox.System{Kind: proxmox.KindPBS, Addresses: []string{"https://pbs"}}},
		{UID: "e5f6", Host: "kaputt", System: proxmox.System{Kind: proxmox.KindPVE, Addresses: []string{"https://bad"}}},
	}
	m.SetAPIAccesses(func(context.Context) ([]APIAccess, bool) { return acc, m64 })
	if m.APIOn(ctx) {
		t.Fatal("api on by default")
	}
	if st, ok := m.APIStates(ctx); !ok || len(st) != 3 || !st[0].At.IsZero() || st[2].Source != "api-pve-a1b2" {
		t.Fatalf("states before: %+v", st)
	}
	m.PollAPI(ctx)
	st, _ := m.APIStates(ctx)
	byHost := map[string]APIState{}
	for _, s := range st {
		byHost[s.Host] = s
	}
	if s := byHost["pve-01"]; s.Err != "" || s.Jobs != 3 || s.At.IsZero() {
		t.Fatalf("pve state: %+v", s)
	}
	if s := byHost["Standort B"]; s.Err != "" || s.Jobs != 3 {
		t.Fatalf("pbs state: %+v", s)
	}
	if s := byHost["kaputt"]; s.Err == "" || !strings.Contains(s.Err, "403") {
		t.Fatalf("bad state: %+v", s)
	}
	day := at(t, "2026-11-18 00:00")
	p, err := m.Plan(ctx, day, day.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Sources) != 2 {
		t.Fatalf("sources: %+v", p.Sources)
	}
	for _, s := range p.Sources {
		if !s.API() || s.Timezone != "Europe/Berlin" || len(s.Notes) != 0 && s.Host() == "pve-01" {
			t.Fatalf("source: %+v", s)
		}
		if s.Host() == "Standort B" && (len(s.Notes) != 1 || !strings.HasPrefix(s.Notes[0], "admin/prune: ")) { // Teilfehler als Hinweis
			t.Fatalf("pbs notes: %+v", s.Notes)
		}
	}
	// Proxmox VE: Job der Node, Lauf zugeordnet; Job von pve-02 fehlt; alte Schreibweise mit dow; abgeschaltet
	b := jobByName(p, "Backup VMs")
	if b == nil || b.Source != "api-pve-a1b2" || len(b.Items) != 1 || b.Items[0].Status != StOK || b.Items[0].End.Sub(b.Items[0].Start) != 70*time.Minute ||
		b.Command != "vzdump --all --storage pbs-standort-b --mode snapshot" {
		t.Fatalf("pve job: %+v", b)
	}
	if j := jobByName(p, "vzdump 101,102"); j == nil || j.Enabled {
		t.Fatalf("disabled job: %+v", j)
	}
	if j := jobByName(p, "vzdump --pool prod"); j == nil || j.Schedule != "mon,wed 04:15" || len(j.Items) != 1 || !j.Items[0].Planned.Equal(at(t, "2026-11-18 04:15")) {
		t.Fatalf("legacy schedule: %+v", j)
	}
	for _, j := range p.Jobs {
		if strings.Contains(j.ID, "backup-0003") {
			t.Fatal("job of another node")
		}
	}
	if len(p.Extra) != 1 || p.Extra[0].Name != "vzdump 104" || p.Extra[0].Status != StRunning {
		t.Fatalf("manual backup: %+v", p.Extra)
	}
	// PBS: Sync mit Verlauf aus der Aufgabenliste (kein doppelter Lauf), Verify laeuft, GC mit Warnungen = gelaufen
	s := jobByName(p, "Offsite-Sync")
	if s == nil || s.Target != "standort-a:main → tank" || len(s.Items) != 1 || s.Items[0].Status != StOK || s.Items[0].End.Sub(s.Items[0].Start) != 88*time.Minute {
		t.Fatalf("sync: %+v", s)
	}
	if runs, _ := m.RunsOf(ctx, s.Source, s.ID, 10); len(runs) != 2 || runs[1].Status != RunFail || runs[1].Detail != "connection error" {
		t.Fatalf("sync runs: %+v", runs)
	}
	if v := jobByName(p, "verify v-1"); v == nil || v.Last == nil || v.Last.Status != RunRunning || !v.Last.Start.Equal(at(t, "2026-11-18 10:00")) {
		t.Fatalf("verify: %+v", v)
	}
	if g := jobByName(p, "garbage collection tank"); g == nil || len(g.Items) != 1 || g.Items[0].Status != StOK || g.Items[0].Run.Detail != "WARNINGS: 2" {
		t.Fatalf("gc: %+v", g)
	}
	// abgeschaltet: ein stummer API-Host loest keine Meldung aus; eingeschaltet schon
	var sent []notify.Alert
	m.SetNotifier(func(a notify.Alert) { sent = append(sent, a) })
	*now = now.Add(3 * time.Hour)
	stale := func() int {
		n := 0
		list, err := m.findAlerts(ctx, *now)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range list {
			if a.Kind == AlertStale {
				n++
			}
		}
		return n
	}
	if n := stale(); n != 0 {
		t.Fatalf("stale while off: %d", n)
	}
	if err := m.SetAPI(ctx, true); err != nil || !m.APIOn(ctx) {
		t.Fatal("set api")
	}
	if n := stale(); n != 2 {
		t.Fatalf("stale while on: %d", n)
	}
	// faellig erst wieder nach apiEvery
	if !m.apiDue(*now) || m.apiDue(now.Add(time.Minute)) || !m.apiDue(now.Add(apiEvery)) {
		t.Fatal("apiDue")
	}
	m64 = false
	if _, ok := m.APIStates(ctx); ok {
		t.Fatal("monitor64 off not reported")
	}
}
