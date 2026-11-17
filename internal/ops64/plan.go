package ops64

import (
	"context"
	"sort"
	"time"
)

// Zeitplan rechnen: Zeitpunkte aus den Zeitplaenen, dazu die gemeldeten Laeufe, daraus Zustand, erwartete Dauer und
// die Zeiten, in denen mehrere Aufgaben gleichzeitig laufen.

// Zustand eines Eintrags im Zeitplan.
const (
	StPlanned = "planned"
	StRunning = "running"
	StOK      = "ok"
	StFail    = "fail"
	StOverdue = "overdue" // faellig, aber nicht gelaufen (nur wenn die Quelle Laeufe meldet)
	StUnknown = "unknown" // vergangen, ohne Rueckmeldung
)

// Statuses in der Reihenfolge der Anzeige.
var Statuses = []string{StPlanned, StRunning, StOK, StFail, StOverdue}

const (
	frequentPerDay = 24               // mehr Zeitpunkte am Tag: "laeuft haeufig", ohne einzelne Eintraege
	overdueGrace   = 10 * time.Minute // so lange nach dem Zeitpunkt gilt eine Aufgabe noch als geplant
	assumeDur      = 30 * time.Minute // Balken bei unklarer Laufzeit
	pairMax        = 24 * time.Hour   // ein Lauf gehoert hoechstens so lange nach dem Zeitpunkt noch zu ihm
	lookBack       = 36 * time.Hour   // vor dem Fenster beginnen (lange Laeufe, spaete Starts)
	staleAfter     = 30 * time.Minute // Host meldet sich nicht (Anzeige)
)

// Item ist ein Eintrag im Zeitplan: ein geplanter Zeitpunkt (mit oder ohne Lauf) oder ein Lauf ausser der Reihe.
type Item struct {
	Job     *JobPlan // nil: Lauf ohne Aufgabe
	Source  string
	Host    string
	Name    string
	Kind    string
	Planned time.Time // Zeitpunkt laut Zeitplan (Null = ausser der Reihe)
	Start   time.Time // Balken: tatsaechlich bzw. geplant
	End     time.Time // tatsaechlich, bei laufenden und geplanten erwartet
	Status  string
	Unclear bool // Laufzeit unklar, Ende angenommen
	Long    bool // laeuft bzw. lief ungewoehnlich lange
	Run     *Run
}

// Anchor ist der Zeitpunkt, zu dem der Eintrag im Kalender steht.
func (it *Item) Anchor() time.Time {
	if !it.Planned.IsZero() {
		return it.Planned
	}
	return it.Start
}

// Key ist die Kennung der Aufgabe (Lauf ohne Aufgabe: aus Quelle, Art und Name).
func (it *Item) Key() string {
	if it.Job != nil {
		return it.Job.Key()
	}
	return it.Source + "||" + it.Kind + "|" + it.Name
}

// JobPlan ist eine Aufgabe mit allem, was der Zeitplan ueber sie weiss.
type JobPlan struct {
	Job
	Host         string
	Loc          *time.Location
	Sched        *Schedule // nil: Zeitplan nicht lesbar bzw. leer
	Frequent     bool      // mehr als frequentPerDay Zeitpunkte am Tag
	Expected     time.Duration
	ExpectedFrom string        // set (von Hand) | runs (aus den Laeufen) | source | "" (unklar)
	MinDur       time.Duration // kuerzester und laengster der letzten erfolgreichen Laeufe
	MaxDur       time.Duration
	Varies       bool // Laufzeit schwankt stark
	Items        []*Item
	Next         time.Time
	Last         *Run
	Overdue      bool // der letzte faellige Zeitpunkt blieb ohne Lauf
}

// Planned: die Aufgabe steht mit einzelnen Eintraegen im Zeitplan.
func (j *JobPlan) Planned() bool {
	return j.Enabled && j.Shown() && j.Sched != nil && !j.Sched.Never() && !j.Frequent
}

// Overlap ist ein Zeitraum, in dem mehrere Aufgaben gleichzeitig laufen (sollen).
type Overlap struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Names []string  `json:"jobs"`
	Keys  []string  `json:"-"`
}

// Plan ist der Zeitplan eines Zeitraums.
type Plan struct {
	From, To, Now time.Time
	Sources       []Source
	Jobs          []*JobPlan
	Extra         []*Item // Laeufe ohne Aufgabe
	Overlaps      []Overlap
}

// Items liefert alle Eintraege nach Beginn.
func (p *Plan) Items() []*Item {
	var out []*Item
	for _, j := range p.Jobs {
		out = append(out, j.Items...)
	}
	out = append(out, p.Extra...)
	sort.SliceStable(out, func(i, k int) bool { return out[i].Start.Before(out[k].Start) })
	return out
}

// longAfter: ab dieser Laufzeit gilt ein Lauf als ungewoehnlich lang (0 = erwartete Dauer unklar).
func longAfter(expected time.Duration) time.Duration {
	if expected <= 0 {
		return 0
	}
	return max(2*expected, expected+15*time.Minute)
}

// expect bestimmt die erwartete Dauer einer Aufgabe: von Hand, sonst aus den letzten erfolgreichen Laeufen (Median;
// schwanken sie stark, der laengste), sonst laut Quelle.
func (j *JobPlan) expect(durs []int64) {
	if len(durs) > 0 {
		s := append([]int64(nil), durs...)
		sort.Slice(s, func(a, b int) bool { return s[a] < s[b] })
		j.MinDur, j.MaxDur = time.Duration(s[0])*time.Second, time.Duration(s[len(s)-1])*time.Second
		j.Varies = len(s) >= 3 && j.MaxDur > 3*j.MinDur && j.MaxDur-j.MinDur > 30*time.Minute
		j.Expected, j.ExpectedFrom = time.Duration(s[len(s)/2])*time.Second, "runs"
		if j.Varies {
			j.Expected = j.MaxDur
		}
	}
	switch {
	case j.ExpectedSet > 0:
		j.Expected, j.ExpectedFrom, j.Varies = time.Duration(j.ExpectedSet)*time.Second, "set", false
	case j.ExpectedFrom == "" && j.Job.Expected > 0:
		j.Expected, j.ExpectedFrom = time.Duration(j.Job.Expected)*time.Second, "source"
	}
}

// Plan rechnet den Zeitplan fuer [from, to).
func (m *Module) Plan(ctx context.Context, from, to time.Time) (*Plan, error) {
	now := m.now()
	p := &Plan{From: from, To: to, Now: now}
	var err error
	if p.Sources, err = m.Sources(ctx); err != nil {
		return nil, err
	}
	jobs, err := m.Jobs(ctx)
	if err != nil {
		return nil, err
	}
	runs, err := m.RunsBetween(ctx, from.Add(-lookBack), to)
	if err != nil {
		return nil, err
	}
	durs, err := m.durations(ctx)
	if err != nil {
		return nil, err
	}
	last, err := m.lastRuns(ctx)
	if err != nil {
		return nil, err
	}
	srcs := map[string]Source{}
	for _, s := range p.Sources {
		srcs[s.Name] = s
	}
	byJob := map[string][]*Run{}
	for i := range runs {
		r := &runs[i]
		byJob[r.Source+"|"+r.Job] = append(byJob[r.Source+"|"+r.Job], r)
	}
	inWindow := func(it *Item) bool { return it.End.After(from) && it.Start.Before(to) }
	for _, j := range jobs {
		src := srcs[j.Source]
		jp := &JobPlan{Job: j, Host: src.Host(), Loc: src.Location()}
		if s, err := ParseSchedule(j.Syntax, j.Schedule); err == nil {
			jp.Sched, jp.Frequent = s, s.PerDay() > frequentPerDay
			if j.Enabled {
				jp.Next = s.Next(now, jp.Loc)
			}
		}
		jp.expect(durs[j.Key()])
		if r, ok := last[j.Key()]; ok {
			jp.Last = &r
		}
		p.Jobs = append(p.Jobs, jp)
		if !jp.Planned() {
			continue
		}
		occs := jp.Sched.Between(from.Add(-lookBack), to, jp.Loc, 0)
		paired := make([]*Run, len(occs))
		var extra []*Run
		for _, r := range byJob[j.Key()] {
			i := sort.Search(len(occs), func(i int) bool { return occs[i].After(r.Start.Add(matchBefore)) }) - 1
			if i >= 0 && paired[i] == nil && r.Start.Sub(occs[i]) <= pairMax {
				paired[i] = r
			} else {
				extra = append(extra, r)
			}
		}
		for i, occ := range occs {
			it := jp.item(occ, paired[i], now, src)
			if occ.Before(now) && i == lastPast(occs, now) {
				jp.Overdue = it.Status == StOverdue
			}
			if inWindow(it) {
				jp.Items = append(jp.Items, it)
			}
		}
		for _, r := range extra {
			if it := jp.item(time.Time{}, r, now, src); inWindow(it) {
				jp.Items = append(jp.Items, it)
			}
		}
		sort.SliceStable(jp.Items, func(a, b int) bool { return jp.Items[a].Start.Before(jp.Items[b].Start) })
	}
	for i := range runs { // Laeufe ohne Aufgabe (z. B. Backup von Hand)
		r := &runs[i]
		if r.Job != "" {
			continue
		}
		it := &Item{Source: r.Source, Host: srcs[r.Source].Host(), Name: r.Name, Kind: r.Kind, Start: r.Start, End: r.End, Status: r.Status, Run: r}
		if r.End.IsZero() {
			it.End = now
		}
		if inWindow(it) {
			p.Extra = append(p.Extra, it)
		}
	}
	p.Overlaps = overlaps(p.Items(), from, to)
	return p, nil
}

// lastPast: Index des letzten Zeitpunkts vor now (-1 = keiner).
func lastPast(occs []time.Time, now time.Time) int {
	return sort.Search(len(occs), func(i int) bool { return !occs[i].Before(now) }) - 1
}

// item baut den Eintrag zu einem Zeitpunkt (occ Null: Lauf ausser der Reihe) und seinem Lauf (nil: keiner).
func (j *JobPlan) item(occ time.Time, r *Run, now time.Time, src Source) *Item {
	it := &Item{Job: j, Source: j.Source, Host: j.Host, Name: j.Name, Kind: j.Kind, Planned: occ, Run: r}
	dur := j.Expected
	if dur <= 0 {
		dur, it.Unclear = assumeDur, true
	}
	if r == nil {
		it.Start, it.End = occ, occ.Add(dur)
		reported := j.History && !occ.Before(j.First)
		switch {
		case occ.After(now), reported && now.Sub(occ) < overdueGrace:
			it.Status = StPlanned
		case reported && !src.Updated.Before(occ.Add(overdueGrace)):
			it.Status = StOverdue
		default:
			it.Status = StUnknown
		}
		return it
	}
	it.Start, it.End, it.Status, it.Unclear = r.Start, r.End, r.Status, false
	limit := longAfter(j.Expected)
	if r.End.IsZero() { // laeuft: bis jetzt bzw. bis zum erwarteten Ende
		it.End, it.Unclear = now, j.Expected <= 0
		if e := r.Start.Add(dur); e.After(now) {
			it.End = e
		}
		it.Long = limit > 0 && now.Sub(r.Start) > limit
	} else {
		it.Long = limit > 0 && r.Status == RunOK && r.End.Sub(r.Start) > limit
	}
	return it
}

// overlaps findet die Zeitraeume in [from, to), in denen Eintraege verschiedener Aufgaben gleichzeitig laufen.
// Ueberfaellige Eintraege zaehlen nicht (sie sind nicht gelaufen).
func overlaps(items []*Item, from, to time.Time) []Overlap {
	type span struct {
		a, b      time.Time
		key, name string
	}
	var (
		spans []span
		cuts  []time.Time
	)
	for _, it := range items {
		a, b := it.Start, it.End
		if it.Status == StOverdue || !b.After(a) {
			continue
		}
		if a.Before(from) {
			a = from
		}
		if b.After(to) {
			b = to
		}
		if !b.After(a) {
			continue
		}
		name := it.Name
		if it.Host != "" {
			name += " (" + it.Host + ")"
		}
		spans = append(spans, span{a, b, it.Key(), name})
		cuts = append(cuts, a, b)
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i].Before(cuts[j]) })
	var out []Overlap
	for i := 0; i+1 < len(cuts); i++ {
		a, b := cuts[i], cuts[i+1]
		if !b.After(a) {
			continue
		}
		keys := map[string]string{}
		for _, s := range spans {
			if !s.a.After(a) && !s.b.Before(b) {
				keys[s.key] = s.name
			}
		}
		if len(keys) < 2 {
			continue
		}
		if n := len(out); n > 0 && out[n-1].End.Equal(a) { // schliesst an: ein Zeitraum
			o := &out[n-1]
			o.End = b
			for k, name := range keys {
				if !contains(o.Keys, k) {
					o.Keys, o.Names = append(o.Keys, k), append(o.Names, name)
				}
			}
			continue
		}
		o := Overlap{Start: a, End: b}
		for k, name := range keys {
			o.Keys, o.Names = append(o.Keys, k), append(o.Names, name)
		}
		out = append(out, o)
	}
	for i := range out {
		o := &out[i]
		sort.Slice(o.Names, func(a, b int) bool { return o.Names[a] < o.Names[b] })
		sort.Strings(o.Keys)
	}
	// unter einer Minute: kein Befund
	keep := out[:0]
	for _, o := range out {
		if o.End.Sub(o.Start) >= time.Minute {
			keep = append(keep, o)
		}
	}
	return keep
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}
