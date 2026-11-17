package ops64

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Annahme der Meldungen der Hosts (PUT /api/ops64/sources/{name}): ein Host schickt jedes Mal alle seine Aufgaben und
// die Laeufe, die er kennt. Aufgaben werden ersetzt, Laeufe ergaenzt (ein Host kennt oft nur den letzten Lauf).

// Grenzen einer Meldung.
const (
	MaxSources  = 64
	MaxJobs     = 500
	MaxRuns     = 5000    // je Meldung
	MaxBodySize = 2 << 20 // 2 MiB (web.bodyLimitFor)

	matchBefore = 2 * time.Minute  // Lauf ohne Aufgabe: so viel vor dem Zeitpunkt des Zeitplans ...
	matchAfter  = 15 * time.Minute // ... bis so viel danach gehoert er zur Aufgabe
)

var sourceNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidSourceName: Kennung eines Hosts in der URL.
func ValidSourceName(s string) bool { return sourceNameRe.MatchString(s) }

// SourceUpdate ist die Meldung eines Hosts.
type SourceUpdate struct {
	Label     string      `json:"label"`     // Anzeigename (leer: die Kennung)
	Collector string      `json:"collector"` // Sammler und Version
	Timezone  string      `json:"timezone"`  // IANA, z. B. Europe/Berlin (leer: die der Node)
	Notes     []string    `json:"notes"`     // Hinweise des Sammlers (z. B. "pvesh nicht gefunden")
	Jobs      []SourceJob `json:"jobs"`
	Runs      []SourceRun `json:"runs"` // Laeufe ohne Aufgabe (kind, name): die Node ordnet sie nach der Zeit zu
}

// SourceJob ist eine gemeldete Aufgabe.
type SourceJob struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Kind     string      `json:"kind"`
	Target   string      `json:"target"`
	Schedule string      `json:"schedule"`
	Syntax   string      `json:"syntax"`
	Command  string      `json:"command"`
	Enabled  *bool       `json:"enabled"` // fehlt = an
	System   bool        `json:"system"`
	History  bool        `json:"history"`
	Expected int64       `json:"expected"` // Sekunden
	Runs     []SourceRun `json:"runs"`
}

// SourceRun ist ein gemeldeter Lauf. End 0 = laeuft noch.
type SourceRun struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Start  int64  `json:"start"`
	End    int64  `json:"end"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	Ref    string `json:"ref"`
}

// IngestResult fasst zusammen, was aus einer Meldung uebernommen wurde.
type IngestResult struct {
	Jobs    int `json:"jobs"`
	Runs    int `json:"runs"`
	Skipped int `json:"skipped"` // ungueltige Eintraege
}

// DecodeSourceUpdate liest eine Meldung streng (unbekannte Felder = Fehler).
func DecodeSourceUpdate(r io.Reader) (*SourceUpdate, error) {
	dec := json.NewDecoder(io.LimitReader(r, MaxBodySize+1))
	dec.DisallowUnknownFields()
	var u SourceUpdate
	if err := dec.Decode(&u); err != nil {
		return nil, ErrInput
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, ErrInput
	}
	return &u, nil
}

// clip kuerzt Text der Quelle auf n Zeichen und entfernt Steuerzeichen.
func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, strings.TrimSpace(strings.ToValidUTF8(s, "")))
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n-1]) + "…"
	}
	return s
}

func validKind(k string) bool { return k == KindBackup || k == KindCron || k == KindTimer }

// cleanRun prueft einen Lauf; ok = false: ungueltig.
func cleanRun(r SourceRun, now time.Time) (SourceRun, bool) {
	if r.Start <= 0 || r.Start > now.Add(time.Hour).Unix() || r.End < 0 || (r.End > 0 && r.End < r.Start) {
		return r, false
	}
	switch {
	case r.End == 0:
		r.Status = RunRunning
	case r.Status != RunOK && r.Status != RunFail && r.Status != RunUnknown:
		return r, false
	}
	r.Name, r.Detail, r.Ref = clip(r.Name, 120), clip(r.Detail, 300), clip(r.Ref, 200)
	return r, true
}

// UpdateSource uebernimmt die Meldung eines Hosts. Ungueltige Eintraege werden uebersprungen und gezaehlt.
func (m *Module) UpdateSource(ctx context.Context, name string, u *SourceUpdate) (IngestResult, error) {
	var res IngestResult
	if !ValidSourceName(name) || u == nil || len(u.Jobs) > MaxJobs {
		return res, ErrInput
	}
	now := m.now()
	tz := strings.TrimSpace(u.Timezone)
	if tz != "" {
		if _, err := time.LoadLocation(tz); err != nil || len(tz) > 64 {
			tz = ""
		}
	}
	src := Source{Name: name, Timezone: tz}
	loc := src.Location()
	notes := make([]string, 0, len(u.Notes))
	for _, n := range u.Notes {
		if n = clip(n, 200); n != "" && len(notes) < 10 {
			notes = append(notes, n)
		}
	}
	notesJSON, _ := json.Marshal(notes)

	tx, err := m.core.Writer().BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()
	var known int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ops_sources WHERE name = ?`, name).Scan(&known); err != nil {
		return res, err
	}
	if known == 0 {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ops_sources`).Scan(&n); err != nil {
			return res, err
		}
		if n >= MaxSources {
			return res, ErrInput
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ops_sources (name, label, collector, timezone, notes, first, updated) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET label = excluded.label, collector = excluded.collector, timezone = excluded.timezone, notes = excluded.notes,
		updated = excluded.updated`, name, clip(u.Label, 80), clip(u.Collector, 40), tz, string(notesJSON), now.Unix(), now.Unix()); err != nil {
		return res, err
	}

	type sched struct {
		id, kind string
		s        *Schedule
	}
	var (
		scheds []sched
		seen   = map[string]bool{}
		ids    []any
	)
	putRun := func(job, kind string, r SourceRun) error {
		if res.Runs >= MaxRuns {
			res.Skipped++
			return nil
		}
		r, ok := cleanRun(r, now)
		if !ok {
			res.Skipped++
			return nil
		}
		res.Runs++
		_, err := tx.ExecContext(ctx, `INSERT INTO ops_runs (source, job, kind, name, start, finish, status, detail, ref) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(source, job, start, ref) DO UPDATE SET finish = excluded.finish, status = excluded.status, detail = excluded.detail,
			name = excluded.name, kind = excluded.kind`, name, job, kind, r.Name, r.Start, r.End, r.Status, r.Detail, r.Ref)
		return err
	}
	// Laeufe, die als laufend gemeldet waren und in dieser Meldung fehlen, bekommen spaeter "Ende unbekannt".
	if _, err := tx.ExecContext(ctx, `UPDATE ops_runs SET status = 'pending' WHERE source = ? AND status = 'running'`, name); err != nil {
		return res, err
	}
	for _, j := range u.Jobs {
		j.ID, j.Name = clip(j.ID, 120), clip(j.Name, 120)
		if j.ID == "" || !validKind(j.Kind) || seen[j.ID] {
			res.Skipped++
			continue
		}
		if j.Name == "" {
			j.Name = j.ID
		}
		if j.Syntax != "" && j.Syntax != SyntaxCron && j.Syntax != SyntaxCalendar {
			j.Syntax = ""
		}
		if j.Expected < 0 || j.Expected > 7*24*3600 {
			j.Expected = 0
		}
		seen[j.ID] = true
		ids = append(ids, j.ID)
		enabled := j.Enabled == nil || *j.Enabled
		j.Schedule = clip(j.Schedule, 200)
		if _, err := tx.ExecContext(ctx, `INSERT INTO ops_jobs (source, id, name, kind, target, schedule, syntax, command, enabled, system, history, expected, first)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(source, id) DO UPDATE SET name = excluded.name, kind = excluded.kind, target = excluded.target, schedule = excluded.schedule,
			syntax = excluded.syntax, command = excluded.command, enabled = excluded.enabled, system = excluded.system, history = excluded.history,
			expected = excluded.expected`, name, j.ID, j.Name, j.Kind, clip(j.Target, 120), j.Schedule, j.Syntax, clip(j.Command, 300), enabled, j.System,
			j.History, j.Expected, now.Unix()); err != nil {
			return res, err
		}
		res.Jobs++
		if s, err := ParseSchedule(j.Syntax, j.Schedule); err == nil && enabled && s.PerDay() <= frequentPerDay {
			scheds = append(scheds, sched{j.ID, j.Kind, s})
		}
		for _, r := range j.Runs {
			if err := putRun(j.ID, j.Kind, r); err != nil {
				return res, err
			}
		}
	}
	// Aufgaben, die der Host nicht mehr meldet, verschwinden; ihre Laeufe bleiben bis zum Ablauf der Aufbewahrung.
	q := `DELETE FROM ops_jobs WHERE source = ?`
	if len(ids) > 0 {
		q += ` AND id NOT IN (?` + strings.Repeat(",?", len(ids)-1) + `)`
	}
	if _, err := tx.ExecContext(ctx, q, append([]any{name}, ids...)...); err != nil {
		return res, err
	}
	for _, r := range u.Runs {
		if !validKind(r.Kind) {
			res.Skipped++
			continue
		}
		job, start := "", time.Unix(r.Start, 0)
		best := matchAfter + time.Second
		for _, sc := range scheds { // die Aufgabe derselben Art, deren Zeitpunkt am naechsten liegt
			if sc.kind != r.Kind {
				continue
			}
			for _, t := range sc.s.Between(start.Add(-matchAfter), start.Add(matchBefore+time.Minute), loc, 0) {
				d := start.Sub(t)
				if d < -matchBefore || d > matchAfter {
					continue
				}
				if d < 0 {
					d = -d
				}
				if d < best {
					best, job = d, sc.id
				}
			}
		}
		// derselbe Lauf stand evtl. schon unter einer anderen Aufgabe (Zeitplan geaendert)
		if _, err := tx.ExecContext(ctx, `DELETE FROM ops_runs WHERE source = ? AND start = ? AND ref = ? AND kind = ? AND job <> ?`, name, r.Start,
			clip(r.Ref, 200), r.Kind, job); err != nil {
			return res, err
		}
		if err := putRun(job, r.Kind, r); err != nil {
			return res, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ops_runs SET status = ?, finish = ? WHERE source = ? AND status = 'pending'`, RunUnknown, now.Unix(), name); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

// sampleUpdate liest eine Meldung aus einem JSON-Text (Tests, Beispieldaten).
func sampleUpdate(raw []byte) (*SourceUpdate, error) { return DecodeSourceUpdate(bytes.NewReader(raw)) }
