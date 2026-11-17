package ops64

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"sort"
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

// Arten einer Aufgabe.
const (
	KindBackup = "backup"
	KindCron   = "cron"
	KindTimer  = "timer"
)

// Kinds in der Reihenfolge der Anzeige.
var Kinds = []string{KindBackup, KindCron, KindTimer}

// Ergebnis eines Laufs.
const (
	RunOK      = "ok"
	RunFail    = "fail"
	RunRunning = "running"
	RunUnknown = "unknown" // lief, das Ende wurde nie gemeldet
)

// Sichtbarkeit einer Aufgabe im Zeitplan (Einstellung der Node).
const (
	VisibleShow = "show"
	VisibleHide = "hide"
)

var (
	// ErrNotFound: Quelle bzw. Aufgabe gibt es nicht.
	ErrNotFound = errors.New("ops64: not found")
	// ErrInput: ungueltige Angabe.
	ErrInput = errors.New("ops64: invalid input")
)

// Source ist ein Host, der seine Aufgaben meldet.
type Source struct {
	Name      string    `json:"name"`
	Label     string    `json:"label"`
	Collector string    `json:"collector,omitempty"`
	Timezone  string    `json:"timezone,omitempty"`
	Notes     []string  `json:"notes,omitempty"`
	First     time.Time `json:"first"`
	Updated   time.Time `json:"updated"`
	Jobs      int       `json:"jobs"`
}

// Host ist der Anzeigename.
func (s Source) Host() string {
	if s.Label != "" {
		return s.Label
	}
	return s.Name
}

// Location ist die Zeitzone des Hosts (unbekannt: die der Node).
func (s Source) Location() *time.Location {
	if s.Timezone != "" {
		if loc, err := time.LoadLocation(s.Timezone); err == nil {
			return loc
		}
	}
	return time.Local
}

// Job ist eine gemeldete Aufgabe samt den Einstellungen der Node.
type Job struct {
	Source   string    `json:"source"`
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Target   string    `json:"target,omitempty"`
	Schedule string    `json:"schedule,omitempty"`
	Syntax   string    `json:"syntax,omitempty"`
	Command  string    `json:"command,omitempty"`
	Enabled  bool      `json:"enabled"`
	System   bool      `json:"system,omitempty"`
	History  bool      `json:"history"`
	Expected int64     `json:"-"` // Sekunden laut Quelle
	First    time.Time `json:"-"`

	Visible     string `json:"-"` // Einstellung: "", show, hide
	ExpectedSet int64  `json:"-"` // Sekunden von Hand
}

// Key ist die Kennung ueber alle Quellen.
func (j Job) Key() string { return j.Source + "|" + j.ID }

// Shown: die Aufgabe steht im Zeitplan (System-Aufgaben nur, wenn eingeblendet).
func (j Job) Shown() bool {
	switch j.Visible {
	case VisibleShow:
		return true
	case VisibleHide:
		return false
	}
	return !j.System
}

// Run ist ein Lauf einer Aufgabe. Job leer: keinem Zeitplan zugeordnet (z. B. Backup von Hand).
type Run struct {
	Source string    `json:"source"`
	Job    string    `json:"job,omitempty"`
	Kind   string    `json:"kind,omitempty"`
	Name   string    `json:"name,omitempty"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"` // Null = laeuft noch
	Status string    `json:"status"`
	Detail string    `json:"detail,omitempty"`
	Ref    string    `json:"-"`
}

// Duration ist die Laufzeit (laufend: bis now).
func (r Run) Duration(now time.Time) time.Duration {
	if r.End.IsZero() {
		return now.Sub(r.Start)
	}
	return r.End.Sub(r.Start)
}

func unix(n int64) time.Time {
	if n <= 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func toUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// Sources liefert die Hosts nach Anzeigename.
func (m *Module) Sources(ctx context.Context) ([]Source, error) {
	rows, err := m.core.Reader().QueryContext(ctx, `SELECT s.name, s.label, s.collector, s.timezone, s.notes, s.first, s.updated,
		(SELECT COUNT(*) FROM ops_jobs j WHERE j.source = s.name) FROM ops_sources s`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var (
			s              Source
			notes          string
			first, updated int64
		)
		if err := rows.Scan(&s.Name, &s.Label, &s.Collector, &s.Timezone, &notes, &first, &updated, &s.Jobs); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(notes), &s.Notes)
		s.First, s.Updated = unix(first), unix(updated)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host() < out[j].Host() })
	return out, rows.Err()
}

// Jobs liefert alle Aufgaben mit den Einstellungen der Node.
func (m *Module) Jobs(ctx context.Context) ([]Job, error) {
	rows, err := m.core.Reader().QueryContext(ctx, `SELECT j.source, j.id, j.name, j.kind, j.target, j.schedule, j.syntax, j.command, j.enabled, j.system,
		j.history, j.expected, j.first, COALESCE(p.visible, ''), COALESCE(p.expected, 0)
		FROM ops_jobs j LEFT JOIN ops_prefs p ON p.source = j.source AND p.job = j.id ORDER BY j.source, j.name, j.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var (
			j     Job
			first int64
		)
		if err := rows.Scan(&j.Source, &j.ID, &j.Name, &j.Kind, &j.Target, &j.Schedule, &j.Syntax, &j.Command, &j.Enabled, &j.System, &j.History,
			&j.Expected, &first, &j.Visible, &j.ExpectedSet); err != nil {
			return nil, err
		}
		j.First = unix(first)
		out = append(out, j)
	}
	return out, rows.Err()
}

const runCols = `source, job, kind, name, start, finish, status, detail, ref`

func scanRuns(rows *sql.Rows) ([]Run, error) {
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var (
			r          Run
			start, end int64
		)
		if err := rows.Scan(&r.Source, &r.Job, &r.Kind, &r.Name, &start, &end, &r.Status, &r.Detail, &r.Ref); err != nil {
			return nil, err
		}
		r.Start, r.End = unix(start), unix(end)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunsBetween liefert die Laeufe, die in [from, to) begonnen haben oder dort noch liefen, aufsteigend.
func (m *Module) RunsBetween(ctx context.Context, from, to time.Time) ([]Run, error) {
	rows, err := m.core.Reader().QueryContext(ctx, `SELECT `+runCols+` FROM ops_runs WHERE start < ? AND (start >= ? OR finish >= ? OR (finish = 0 AND start >= ?))
		ORDER BY start`, to.Unix(), from.Unix(), from.Unix(), from.Add(-7*24*time.Hour).Unix())
	if err != nil {
		return nil, err
	}
	return scanRuns(rows)
}

// RunsOf liefert die letzten Laeufe einer Aufgabe, neueste zuerst.
func (m *Module) RunsOf(ctx context.Context, source, job string, limit int) ([]Run, error) {
	rows, err := m.core.Reader().QueryContext(ctx, `SELECT `+runCols+` FROM ops_runs WHERE source = ? AND job = ? ORDER BY start DESC LIMIT ?`, source, job, limit)
	if err != nil {
		return nil, err
	}
	return scanRuns(rows)
}

// durations liefert je Aufgabe die Dauer der letzten erfolgreichen Laeufe in Sekunden (hoechstens 10, neueste zuerst).
func (m *Module) durations(ctx context.Context) (map[string][]int64, error) {
	rows, err := m.core.Reader().QueryContext(ctx, `SELECT source, job, finish - start FROM ops_runs WHERE status = 'ok' AND job <> '' AND finish >= start
		ORDER BY start DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]int64{}
	for rows.Next() {
		var (
			src, job string
			d        int64
		)
		if err := rows.Scan(&src, &job, &d); err != nil {
			return nil, err
		}
		if k := src + "|" + job; len(out[k]) < 10 {
			out[k] = append(out[k], d)
		}
	}
	return out, rows.Err()
}

// lastRuns liefert je Aufgabe den letzten Lauf.
func (m *Module) lastRuns(ctx context.Context) (map[string]Run, error) {
	rows, err := m.core.Reader().QueryContext(ctx, `SELECT `+runCols+` FROM ops_runs r WHERE job <> '' AND start = (SELECT MAX(start) FROM ops_runs x
		WHERE x.source = r.source AND x.job = r.job)`)
	if err != nil {
		return nil, err
	}
	list, err := scanRuns(rows)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Run, len(list))
	for _, r := range list {
		out[r.Source+"|"+r.Job] = r
	}
	return out, nil
}

// SetPrefs stellt Sichtbarkeit und erwartete Dauer einer Aufgabe ein (visible: "", show, hide; Sekunden, 0 = aus den
// Laeufen).
func (m *Module) SetPrefs(ctx context.Context, source, job, visible string, expected int64) error {
	if visible != "" && visible != VisibleShow && visible != VisibleHide || expected < 0 || expected > 7*24*3600 {
		return ErrInput
	}
	var n int
	if err := m.core.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM ops_jobs WHERE source = ? AND id = ?`, source, job).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	_, err := m.core.Writer().ExecContext(ctx, `INSERT INTO ops_prefs (source, job, visible, expected) VALUES (?, ?, ?, ?)
		ON CONFLICT(source, job) DO UPDATE SET visible = excluded.visible, expected = excluded.expected`, source, job, visible, expected)
	return err
}

// DeleteSource entfernt einen Host mit Aufgaben, Laeufen, Einstellungen und Meldungen.
func (m *Module) DeleteSource(ctx context.Context, name string) error {
	tx, err := m.core.Writer().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM ops_sources WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	for _, q := range []string{`DELETE FROM ops_jobs WHERE source = ?`, `DELETE FROM ops_runs WHERE source = ?`, `DELETE FROM ops_prefs WHERE source = ?`,
		`DELETE FROM ops_alerts WHERE source = ?`} {
		if _, err := tx.ExecContext(ctx, q, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// prune loescht alte Laeufe und erledigte Meldungen.
func (m *Module) prune(ctx context.Context, now time.Time) error {
	w := m.core.Writer()
	if _, err := w.ExecContext(ctx, `DELETE FROM ops_runs WHERE start < ?`, now.Add(-runKeep).Unix()); err != nil {
		return err
	}
	_, err := w.ExecContext(ctx, `DELETE FROM ops_alerts WHERE open = 0 AND at < ?`, now.Add(-alertKeep).Unix())
	return err
}
