package ops64

import (
	"context"
	"embed"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
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
	mux.Handle("GET /ops64", page(m.pageOverview))
	mux.Handle("POST /ops64/job", page(m.postJob))
	mux.Handle("POST /ops64/notify", page(m.postNotify))
	mux.Handle("POST /ops64/source/delete", page(m.postSourceDelete))
	mux.Handle("POST /ops64/api", page(m.postAPI))
	mux.Handle("GET /ops64/collector/linux.py", page(m.collectorLinux))
}

// Ansichten und Fenster des Zeitplans.
const (
	viewDay   = "day"
	viewWeek  = "week"
	viewMonth = "month"
	winNight  = "night" // 0 bis 12 Uhr
	winFull   = "full"  // ganzer Tag
)

// query sind die Angaben in der Adresse der Seite.
type query struct {
	View, Win, Host, Kind, Status, Sel, Tab string
	Sys                                     bool // System-Aufgaben in den Listen zeigen
	Day                                     time.Time
	dayText                                 string
}

func oneOf(s, def string, allowed ...string) string {
	for _, a := range allowed {
		if s == a {
			return s
		}
	}
	return def
}

func parseQuery(r *http.Request, now time.Time, loc *time.Location) query {
	v := r.URL.Query()
	q := query{View: oneOf(v.Get("view"), viewWeek, viewDay, viewWeek, viewMonth), Win: oneOf(v.Get("win"), "", winNight, winFull), Host: v.Get("host"),
		Kind: oneOf(v.Get("kind"), "", Kinds...), Status: oneOf(v.Get("status"), "", Statuses...), Sel: v.Get("sel"),
		Tab: oneOf(v.Get("tab"), KindBackup, KindBackup, "tasks"), Sys: v.Get("sys") == "1"}
	y, mo, d := now.In(loc).Date()
	q.Day = time.Date(y, mo, d, 0, 0, 0, 0, loc)
	if t, err := time.ParseInLocation("2006-01-02", v.Get("d"), loc); err == nil && t.Year() >= 2000 && t.Year() < 2200 {
		q.Day, q.dayText = t, v.Get("d")
	}
	return q
}

// D ist der Tag aus der Adresse ("" = heute).
func (q query) D() string { return q.dayText }

// URL baut die Adresse der Seite mit geaenderten Angaben (Paare Name, Wert; leerer Wert = weglassen).
func (q query) URL(kv ...string) string {
	v := url.Values{}
	set := func(k, s string) {
		if s != "" {
			v.Set(k, s)
		} else {
			v.Del(k)
		}
	}
	if q.View != viewWeek {
		set("view", q.View)
	}
	set("win", q.Win)
	set("host", q.Host)
	set("kind", q.Kind)
	set("status", q.Status)
	set("sel", q.Sel)
	set("d", q.dayText)
	if q.Tab != KindBackup {
		set("tab", q.Tab)
	}
	if q.Sys {
		set("sys", "1")
	}
	anchor := ""
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i] == "#" {
			anchor = "#" + kv[i+1]
			continue
		}
		set(kv[i], kv[i+1])
	}
	if len(v) == 0 {
		return "/ops64" + anchor
	}
	return "/ops64?" + v.Encode() + anchor
}

type tick struct {
	Label string
	Left  float64
}

type bar struct {
	Left, Width float64
	Status      string
	Unclear     bool
	Long        bool
	Text        string // "01:00–02:15"
	Title       string
	TextLeft    bool // Beschriftung links vom Balken (kein Platz rechts)
	Extra       bool // ausser der Reihe
}

type cell struct {
	Status string // "" = nichts an diesem Tag
	Title  string
	URL    string
}

type row struct {
	Name, Sub string
	Kind      string
	URL       string
	Selected  bool
	Bars      []bar
	Cells     []cell
	first     time.Time
}

type band struct {
	Left, Width float64
}

type overlapNote struct {
	Time  string
	Names string
}

type dayHead struct {
	Label, Date, URL string
	Selected, Today  bool
}

type monthCell struct {
	Day                         int // 0 = leer
	URL                         string
	Today                       bool
	Count, Fail, Overdue, Run   int
	Overlap                     bool
	OK, Planned, Unknown, Total int
}

type kpi struct {
	Today, Done        int
	Running            int
	RunningText        string
	Failed             int // letzte 7 Tage
	Overdue            int
	OverdueText        string
	Overlaps           int
	OverlapText        string
	Hosts, Jobs, Stale int
}

type runRow struct {
	Start  time.Time
	Dur    string
	Status string
	Detail string
	Long   bool
	Extra  bool
}

type selData struct {
	Job         *JobPlan
	Status      string
	Sched       string
	Expected    string
	Range       string // "18 min – 2 h 41 min"
	ExpectedMin int64
	Runs        []runRow
	Partners    []string
	BackURL     string
}

type areaRow struct {
	Job      *JobPlan
	Sched    string
	Status   string // ok | fail | running | overdue | off | hidden | none
	LastDur  string
	URL      string
	BadSched bool
}

type sourceRow struct {
	Source
	Stale bool
}

type alertRow struct {
	Alert
	Text string
}

type pageData struct {
	Q        query
	Empty    bool
	KPI      kpi
	Title    string
	PrevURL  string
	NextURL  string
	TodayURL string
	Win      string
	Hosts    []Source
	Kinds    []string
	Statuses []string

	Ticks    []tick
	Rows     []row
	Bands    []band
	Notes    []overlapNote
	Days     []dayHead
	NowLeft  float64
	ShowNow  bool
	DayLabel string
	Shown    int
	Total    int
	Frequent []string // Aufgaben, die haeufiger laufen als frequentPerDay
	Weeks    [][]monthCell

	Sel *selData

	Backups     []areaRow
	Tasks       []areaRow
	NBackups    int
	NTasks      int
	HiddenCount int
	Sources     []sourceRow
	Alerts      []alertRow
	Notify      []notifyFlag
	BaseURL     string

	APIOn   bool       // Abruf ueber die Proxmox-API eingeschaltet
	APIM64  bool       // Monitor64 laeuft (von dort kommen die Zugaenge)
	APIList []APIState // Zugaenge mit dem Stand des letzten Abrufs
}

type notifyFlag struct {
	Kind string
	On   bool
}

func dayStart(t time.Time, loc *time.Location) time.Time {
	y, mo, d := t.In(loc).Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, loc)
}

// monday: Beginn der Woche (Montag) des Tages.
func monday(day time.Time) time.Time {
	return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
}

var statusRank = map[string]int{StFail: 6, StOverdue: 5, StRunning: 4, StOK: 3, StUnknown: 2, StPlanned: 1}

// schedText beschreibt den Zeitplan einer Aufgabe in der Sprache des Aufrufers (einfache Faelle, sonst der Text der Quelle).
func (m *Module) schedText(r *http.Request, j *JobPlan) string {
	if j.Sched == nil {
		return j.Schedule
	}
	if j.Sched.Never() {
		return m.g.T(r, "ops64.sched.never")
	}
	kind, clock, days, n := j.Sched.Summary()
	switch kind {
	case "daily":
		return m.g.T(r, "ops64.sched.daily", "time", clock)
	case "days":
		var parts []string
		for i := 0; i < len(days); i++ { // aufeinander folgende Tage zusammenfassen (Mo–Fr)
			k := i
			for k+1 < len(days) && (days[k]+1)%7 == days[k+1] {
				k++
			}
			a := m.g.T(r, "ops64.dow."+strconv.Itoa(days[i]))
			if k-i >= 2 {
				a += "–" + m.g.T(r, "ops64.dow."+strconv.Itoa(days[k]))
				i = k
			}
			parts = append(parts, a)
		}
		return strings.Join(parts, ", ") + " " + clock
	case "monthly":
		return m.g.T(r, "ops64.sched.monthly", "day", n, "time", clock)
	case "hourly":
		return m.g.T(r, "ops64.sched.hourly", "min", two(n))
	case "minutes":
		return m.g.T(r, "ops64.sched.minutes", "n", n)
	}
	return j.Schedule
}

func (m *Module) fail(w http.ResponseWriter, r *http.Request, err error) {
	m.log.Error("ops64: page", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func (m *Module) pageOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	loc := m.g.Timezone(r)
	if loc == nil {
		loc = time.Local
	}
	now := m.now().In(loc)
	q := parseQuery(r, now, loc)
	d := &pageData{Q: q, Kinds: Kinds, Statuses: Statuses, BaseURL: baseURL(r)}
	today := dayStart(now, loc)

	// Zeitraum: Woche des Tages bzw. Monat
	from, to := monday(q.Day), monday(q.Day).AddDate(0, 0, 7)
	if q.View == viewMonth {
		from = time.Date(q.Day.Year(), q.Day.Month(), 1, 0, 0, 0, 0, loc)
		to = from.AddDate(0, 1, 0)
	}
	p, err := m.Plan(ctx, from, to)
	if err != nil {
		m.fail(w, r, err)
		return
	}
	tp := p // Zeitplan von heute fuer die Kennzahlen
	if today.Before(from) || !today.Before(to) {
		if tp, err = m.Plan(ctx, today, today.AddDate(0, 0, 1)); err != nil {
			m.fail(w, r, err)
			return
		}
	}
	d.Hosts, d.Empty = p.Sources, len(p.Sources) == 0
	for _, s := range p.Sources {
		d.Sources = append(d.Sources, sourceRow{s, now.Sub(s.Updated) >= staleAfter})
		if now.Sub(s.Updated) >= staleAfter {
			d.KPI.Stale++
		}
	}
	d.KPI.Hosts, d.KPI.Jobs = len(p.Sources), len(p.Jobs)
	m.kpis(ctx, d, tp, today, loc)

	pass := func(source, kind string) bool {
		return (q.Host == "" || q.Host == source) && (q.Kind == "" || q.Kind == kind)
	}
	switch q.View {
	case viewMonth:
		d.Title = m.g.T(r, "ops64.month."+strconv.Itoa(int(from.Month()))) + " " + strconv.Itoa(from.Year())
		prev, next := from.AddDate(0, -1, 0), from.AddDate(0, 1, 0)
		d.PrevURL, d.NextURL = q.URL("d", prev.Format("2006-01-02")), q.URL("d", next.Format("2006-01-02"))
		m.month(d, p, from, to, today, loc, pass)
	default:
		m.timeline(r, d, p, from, today, now, loc, pass)
	}
	d.TodayURL = q.URL("d", "")
	m.areas(r, d, p)
	if q.Sel != "" {
		m.selection(r, d, p, loc)
	}
	alerts, err := m.Alerts(ctx, 12)
	if err != nil {
		m.fail(w, r, err)
		return
	}
	for _, a := range alerts {
		args := make([]any, 0, 2*len(a.Data))
		for k, v := range a.Data {
			args = append(args, k, v)
		}
		d.Alerts = append(d.Alerts, alertRow{a, m.g.T(r, "notify.text.ops64_"+a.Kind, args...)})
	}
	for _, k := range AlertKinds {
		d.Notify = append(d.Notify, notifyFlag{k, m.NotifyOn(ctx, k)})
	}
	d.APIOn = m.APIOn(ctx)
	d.APIList, d.APIM64 = m.APIStates(ctx)
	m.g.Render(w, r, http.StatusOK, "ops64/overview", "ops64.title", d)
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// kpis fuellt die Kennzahlen aus dem Zeitplan von heute.
func (m *Module) kpis(ctx context.Context, d *pageData, p *Plan, today time.Time, loc *time.Location) {
	end := today.AddDate(0, 0, 1)
	for _, it := range p.Items() {
		a := it.Anchor()
		if !it.Planned.IsZero() && !a.Before(today) && a.Before(end) {
			d.KPI.Today++
			if it.Status == StOK {
				d.KPI.Done++
			}
		}
		if it.Status == StRunning {
			d.KPI.Running++
			if d.KPI.RunningText == "" {
				d.KPI.RunningText = it.Name + " · " + it.Start.In(loc).Format("15:04")
			}
		}
	}
	for _, j := range p.Jobs {
		if j.Overdue && j.Planned() {
			d.KPI.Overdue++
			if d.KPI.OverdueText == "" {
				d.KPI.OverdueText = j.Name
			}
		}
	}
	for _, o := range p.Overlaps {
		if o.Start.Before(end) && o.End.After(today) {
			d.KPI.Overlaps++
			if d.KPI.OverlapText == "" {
				d.KPI.OverlapText = o.Start.In(loc).Format("15:04") + "–" + o.End.In(loc).Format("15:04")
			}
		}
	}
	shown := map[string]bool{}
	for _, j := range p.Jobs {
		shown[j.Key()] = j.Shown()
	}
	if runs, err := m.RunsBetween(ctx, p.Now.Add(-7*24*time.Hour), p.Now.Add(time.Minute)); err == nil {
		for _, r := range runs {
			if r.Status == RunFail && (r.Job == "" || shown[r.Source+"|"+r.Job]) {
				d.KPI.Failed++
			}
		}
	}
}

// timeline baut Tages- und Wochenansicht: je Aufgabe eine Zeile mit Balken auf der Zeitachse des gewaehlten Tages
// und (Woche) dem Zustand je Wochentag.
func (m *Module) timeline(r *http.Request, d *pageData, p *Plan, week, today, now time.Time, loc *time.Location, pass func(source, kind string) bool) {
	q := d.Q
	day := q.Day
	dow := func(t time.Time) string { return m.g.T(r, "ops64.dow."+strconv.Itoa(int(t.Weekday()))) }
	d.DayLabel = dow(day) + " " + day.Format("02.01.")
	step := 7
	if q.View == viewDay {
		step = 1
		d.Title = dow(day) + " " + day.Format("02.01.2006")
	} else {
		_, kw := week.ISOWeek()
		d.Title = m.g.T(r, "ops64.week", "n", kw) + " · " + week.Format("02.01.") + "–" + week.AddDate(0, 0, 6).Format("02.01.2006")
	}
	d.PrevURL, d.NextURL = q.URL("d", day.AddDate(0, 0, -step).Format("2006-01-02")), q.URL("d", day.AddDate(0, 0, step).Format("2006-01-02"))

	items := p.Items()
	win := q.Win
	if win == "" { // von selbst: der ganze Tag, sobald am Nachmittag oder Abend etwas liegt
		win = winNight
		noon := day.Add(12 * time.Hour)
		for _, it := range items {
			if pass(it.Source, it.Kind) && it.End.After(noon) && it.Start.Before(day.AddDate(0, 0, 1)) && it.Status != StUnknown {
				win = winFull
				break
			}
		}
	}
	d.Win = win
	wFrom, wTo := day, day.Add(12*time.Hour)
	if win == winFull {
		wTo = day.AddDate(0, 0, 1)
	}
	span := wTo.Sub(wFrom).Seconds()
	pos := func(t time.Time) float64 { return 100 * t.Sub(wFrom).Seconds() / span }
	for t := wFrom; !t.After(wTo); t = t.Add(2 * time.Hour) {
		d.Ticks = append(d.Ticks, tick{two(int(t.Sub(wFrom).Hours())), pos(t)})
	}
	if now.After(wFrom) && now.Before(wTo) {
		d.NowLeft, d.ShowNow = pos(now), true
	}
	for i := 0; i < 7 && q.View != viewDay; i++ {
		t := week.AddDate(0, 0, i)
		d.Days = append(d.Days, dayHead{Label: dow(t), Date: t.Format("02.01."), URL: q.URL("d", t.Format("2006-01-02")), Selected: t.Equal(day), Today: t.Equal(today)})
	}

	clock := func(t time.Time) string { return t.In(loc).Format("15:04") }
	rows := map[string]*row{}
	var order []string
	total := map[string]bool{}
	for _, it := range items {
		if !pass(it.Source, it.Kind) {
			continue
		}
		key := it.Key()
		total[key] = true
		rw := rows[key]
		if rw == nil {
			rw = &row{Name: it.Name, Kind: it.Kind, URL: q.URL("sel", key, "#", "ops-sel"), Selected: q.Sel == key}
			if it.Job != nil {
				rw.Sub = it.Host
				if it.Job.Target != "" {
					rw.Sub += " → " + it.Job.Target
				}
				rw.Sub += " · " + m.schedText(r, it.Job)
			} else {
				rw.URL, rw.Sub = "", it.Host+" · "+m.g.T(r, "ops64.extra")
			}
			rw.Cells = make([]cell, len(d.Days))
			rows[key] = rw
			order = append(order, key)
		}
		st := m.g.T(r, "ops64.status."+it.Status)
		text := clock(it.Start) + "–" + clock(it.End)
		if it.Unclear {
			text = clock(it.Start) + "–? " + m.g.T(r, "ops64.unclear")
		} else if it.Status == StRunning {
			text = m.g.T(r, "ops64.since", "time", clock(it.Start))
		}
		// Zustand je Wochentag
		if i := int(dayStart(it.Anchor(), loc).Sub(week).Hours()+1) / 24; i >= 0 && i < len(rw.Cells) && !it.Anchor().Before(week) {
			c := &rw.Cells[i]
			if statusRank[it.Status] > statusRank[c.Status] {
				c.Status = it.Status
			}
			if c.Title != "" {
				c.Title += "\n"
			}
			c.Title += dow(it.Anchor().In(loc)) + " " + it.Anchor().In(loc).Format("02.01.") + " " + text + " · " + st
			c.URL = q.URL("d", week.AddDate(0, 0, i).Format("2006-01-02"), "sel", key)
		}
		// Balken im Fenster des Tages
		if !it.End.After(wFrom) || !it.Start.Before(wTo) {
			continue
		}
		a, b := it.Start, it.End
		if a.Before(wFrom) {
			a = wFrom
		}
		if b.After(wTo) {
			b = wTo
		}
		bw := bar{Left: pos(a), Width: max(pos(b)-pos(a), 0.6), Status: it.Status, Unclear: it.Unclear, Long: it.Long, Text: text, Extra: it.Planned.IsZero() && it.Job != nil}
		bw.Title = it.Name + " · " + st + " · " + text
		if it.Long {
			bw.Title += " · " + m.g.T(r, "ops64.long")
		}
		if it.Run != nil && it.Run.Detail != "" {
			bw.Title += " · " + it.Run.Detail
		}
		bw.TextLeft = bw.Left+bw.Width > 82
		if rw.first.IsZero() || a.Before(rw.first) {
			rw.first = a
		}
		rw.Bars = append(rw.Bars, bw)
	}
	for _, key := range order {
		rw := rows[key]
		if len(rw.Bars) == 0 && q.View == viewDay {
			continue
		}
		if q.Status != "" {
			hit := false
			for _, b := range rw.Bars {
				hit = hit || b.Status == q.Status
			}
			for _, c := range rw.Cells {
				hit = hit || c.Status == q.Status
			}
			if !hit {
				continue
			}
		}
		d.Rows = append(d.Rows, *rw)
	}
	sort.SliceStable(d.Rows, func(i, j int) bool {
		a, b := d.Rows[i], d.Rows[j]
		if a.first.IsZero() != b.first.IsZero() {
			return !a.first.IsZero()
		}
		if !a.first.Equal(b.first) {
			return a.first.Before(b.first)
		}
		return a.Name < b.Name
	})
	d.Shown, d.Total = len(d.Rows), len(total)
	for _, o := range p.Overlaps {
		if !o.End.After(wFrom) || !o.Start.Before(day.AddDate(0, 0, 1)) {
			continue
		}
		if o.Start.Before(wTo) {
			a, b := o.Start, o.End
			if a.Before(wFrom) {
				a = wFrom
			}
			if b.After(wTo) {
				b = wTo
			}
			d.Bands = append(d.Bands, band{pos(a), max(pos(b)-pos(a), 0.3)})
		}
		d.Notes = append(d.Notes, overlapNote{clock(o.Start) + "–" + clock(o.End), strings.Join(o.Names, ", ")})
	}
	for _, j := range p.Jobs {
		if j.Frequent && j.Enabled && j.Shown() && pass(j.Source, j.Kind) {
			d.Frequent = append(d.Frequent, j.Name+" ("+j.Host+", "+m.schedText(r, j)+")")
		}
	}
}

// month baut die Monatsansicht: je Tag die Zahl der Aufgaben und was auffaellt.
func (m *Module) month(d *pageData, p *Plan, from, to, today time.Time, loc *time.Location, pass func(source, kind string) bool) {
	days := int(to.Sub(from).Hours()+1) / 24
	cells := make([]monthCell, days)
	for i := range cells {
		t := from.AddDate(0, 0, i)
		cells[i] = monthCell{Day: i + 1, Today: t.Equal(today), URL: d.Q.URL("view", "", "d", t.Format("2006-01-02"))}
	}
	for _, it := range p.Items() {
		a := it.Anchor().In(loc)
		if !pass(it.Source, it.Kind) || a.Before(from) || !a.Before(to) {
			continue
		}
		c := &cells[a.Day()-1]
		c.Count++
		switch it.Status {
		case StFail:
			c.Fail++
		case StOverdue:
			c.Overdue++
		case StRunning:
			c.Run++
		case StOK:
			c.OK++
		}
	}
	for _, o := range p.Overlaps {
		for t := dayStart(o.Start, loc); t.Before(o.End) && t.Before(to); t = t.AddDate(0, 0, 1) {
			if !t.Before(from) {
				cells[t.Day()-1].Overlap = true
			}
		}
	}
	week := make([]monthCell, (int(from.Weekday())+6)%7) // leere Felder vor dem Ersten
	for _, c := range cells {
		week = append(week, c)
		if len(week) == 7 {
			d.Weeks, week = append(d.Weeks, week), nil
		}
	}
	if len(week) > 0 {
		d.Weeks = append(d.Weeks, append(week, make([]monthCell, 7-len(week))...))
	}
}

// areas fuellt die Listen "Backups" und "Cronjobs & Timer".
func (m *Module) areas(r *http.Request, d *pageData, p *Plan) {
	for _, j := range p.Jobs {
		if !j.Shown() {
			d.HiddenCount++
			if !d.Q.Sys {
				continue
			}
		}
		a := areaRow{Job: j, Sched: m.schedText(r, j), URL: d.Q.URL("sel", j.Key(), "#", "ops-sel"), BadSched: j.Sched == nil && j.Schedule != "", Status: "none"}
		switch {
		case !j.Shown():
			a.Status = "hidden"
		case !j.Enabled:
			a.Status = "off"
		case j.Overdue:
			a.Status = StOverdue
		case j.Last != nil:
			a.Status = j.Last.Status
		case !j.History: // die Quelle meldet keine Laeufe (z. B. Cronjob)
			a.Status = StUnknown
		}
		if j.Last != nil {
			a.LastDur = FormatDur(j.Last.Duration(p.Now))
		}
		if j.Kind == KindBackup {
			d.Backups = append(d.Backups, a)
		} else {
			d.Tasks = append(d.Tasks, a)
		}
	}
	d.NBackups, d.NTasks = len(d.Backups), len(d.Tasks)
}

// selection fuellt die Einzelheiten zur gewaehlten Aufgabe.
func (m *Module) selection(r *http.Request, d *pageData, p *Plan, loc *time.Location) {
	var j *JobPlan
	for _, x := range p.Jobs {
		if x.Key() == d.Q.Sel {
			j = x
		}
	}
	if j == nil {
		return
	}
	s := &selData{Job: j, Sched: m.schedText(r, j), ExpectedMin: j.ExpectedSet / 60, BackURL: d.Q.URL("sel", "")}
	if j.Expected > 0 {
		s.Expected = FormatDur(j.Expected)
	}
	if j.MaxDur > 0 && j.MaxDur != j.MinDur {
		s.Range = FormatDur(j.MinDur) + " – " + FormatDur(j.MaxDur)
	}
	switch {
	case !j.Enabled:
		s.Status = "off"
	case j.Overdue:
		s.Status = StOverdue
	case j.Last != nil:
		s.Status = j.Last.Status
	}
	runs, err := m.RunsOf(r.Context(), j.Source, j.ID, 12)
	if err != nil {
		m.log.Warn("ops64: runs", "err", err)
	}
	limit := longAfter(j.Expected)
	for _, x := range runs {
		dur := x.Duration(p.Now)
		s.Runs = append(s.Runs, runRow{Start: x.Start, Dur: FormatDur(dur), Status: x.Status, Detail: x.Detail, Long: limit > 0 && x.Status != RunFail && dur > limit})
	}
	seen := map[string]bool{}
	for _, o := range p.Overlaps {
		if !contains(o.Keys, j.Key()) {
			continue
		}
		own := j.Name + " (" + j.Host + ")"
		for _, n := range o.Names {
			if n != own && !seen[n] {
				seen[n] = true
				s.Partners = append(s.Partners, n)
			}
		}
	}
	d.Sel = s
}

func (m *Module) postJob(w http.ResponseWriter, r *http.Request) {
	source, job := r.PostFormValue("source"), r.PostFormValue("job")
	back := r.PostFormValue("back")
	if !strings.HasPrefix(back, "/ops64") {
		back = "/ops64"
	}
	min, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("expected")), 10, 64)
	if r.PostFormValue("expected") == "" {
		min, err = 0, nil
	}
	if err == nil {
		err = m.SetPrefs(r.Context(), source, job, r.PostFormValue("visible"), min*60)
	} else {
		err = ErrInput
	}
	if err != nil {
		m.g.Flash(w, r, "danger", m.g.T(r, "ops64.err.input"))
	} else {
		m.g.Audit(r, "ops64.job.save", source+"/"+job, "visible="+r.PostFormValue("visible")+" expected_min="+strconv.FormatInt(min, 10))
		m.g.Flash(w, r, "success", m.g.T(r, "ops64.flash.job_saved"))
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

func (m *Module) postNotify(w http.ResponseWriter, r *http.Request) {
	var on []string
	for _, k := range AlertKinds {
		v := r.PostFormValue(k) == "1"
		if err := m.SetNotify(r.Context(), k, v); err != nil {
			m.g.Flash(w, r, "danger", m.g.T(r, "ops64.err.input"))
			http.Redirect(w, r, "/ops64#ops-alerts", http.StatusSeeOther)
			return
		}
		if v {
			on = append(on, k)
		}
	}
	m.g.Audit(r, "ops64.notify", Name, strings.Join(on, " "))
	m.g.Flash(w, r, "success", m.g.T(r, "ops64.flash.notify_saved"))
	http.Redirect(w, r, "/ops64#ops-alerts", http.StatusSeeOther)
}

func (m *Module) postSourceDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PostFormValue("name")
	if err := m.DeleteSource(r.Context(), name); err != nil {
		m.g.Flash(w, r, "danger", m.g.T(r, "ops64.err.not_found"))
	} else {
		m.g.Audit(r, "ops64.source.delete", name, "")
		m.g.Flash(w, r, "success", m.g.T(r, "ops64.flash.source_deleted"))
	}
	http.Redirect(w, r, "/ops64#ops-sources", http.StatusSeeOther)
}

// postAPI schaltet den Abruf ueber die Proxmox-API (action on/off) bzw. fragt sofort ab (action poll).
func (m *Module) postAPI(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	action := r.PostFormValue("action")
	switch action {
	case "on", "off":
		if err := m.SetAPI(ctx, action == "on"); err != nil {
			m.fail(w, r, err)
			return
		}
		m.g.Audit(r, "ops64.api", Name, action)
		m.g.Flash(w, r, "success", m.g.T(r, "ops64.flash.api_"+action))
	case "poll":
		if !m.APIOn(ctx) {
			action = ""
		}
	default:
		action = ""
	}
	if action == "on" || action == "poll" { // gleich abrufen, damit die Seite den Stand zeigt (begrenzt, der Rest folgt im Hintergrund)
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		m.apiDue(m.now())
		m.PollAPI(c)
		cancel()
		if action == "poll" {
			m.g.Flash(w, r, "success", m.g.T(r, "ops64.flash.api_polled"))
		}
	}
	http.Redirect(w, r, "/ops64#ops-api", http.StatusSeeOther)
}
