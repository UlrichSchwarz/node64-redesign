package ops64

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ipv64net/node64/internal/module"
)

// Lokale JSON-API des Moduls (Doku: internal/web/static/openapi.json, Tag "OPS64"). Ist das Modul aus: 404. Rolle
// "Nur lesen": aendernde Anfragen beantwortet der Kern mit 403.

const planMaxDays = 62 // groesster Zeitraum von GET /api/ops64/plan

func (m *Module) apiRoutes(mux *http.ServeMux, g module.Guards) {
	api := func(h http.HandlerFunc) http.Handler { return g.Enabled(g.Auth(h)) }
	mux.Handle("GET /api/ops64/sources", api(m.apiSources))
	mux.Handle("PUT /api/ops64/sources/{name}", api(m.apiSourcePut))
	mux.Handle("DELETE /api/ops64/sources/{name}", api(m.apiSourceDelete))
	mux.Handle("GET /api/ops64/jobs", api(m.apiJobs))
	mux.Handle("GET /api/ops64/plan", api(m.apiPlan))
	mux.Handle("GET /api/ops64/collector/linux.py", api(m.collectorLinux)) // mit API-Key ladbar (curl auf dem Host)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (m *Module) apiFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrInput):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		m.log.Error("ops64: api", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}

func (m *Module) apiSources(w http.ResponseWriter, r *http.Request) {
	list, err := m.Sources(r.Context())
	if err != nil {
		m.apiFail(w, err)
		return
	}
	if list == nil {
		list = []Source{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": list})
}

// apiSourcePut nimmt die Meldung eines Hosts an (Sammler).
func (m *Module) apiSourcePut(w http.ResponseWriter, r *http.Request) {
	u, err := DecodeSourceUpdate(http.MaxBytesReader(w, r.Body, MaxBodySize))
	if err != nil {
		m.apiFail(w, err)
		return
	}
	name := r.PathValue("name")
	known := false
	if list, err := m.Sources(r.Context()); err == nil {
		for _, s := range list {
			known = known || s.Name == name
		}
	}
	res, err := m.UpdateSource(r.Context(), name, u)
	if err != nil {
		m.apiFail(w, err)
		return
	}
	if !known { // nur das erste Mal ins Audit (danach alle paar Minuten)
		m.g.Audit(r, "ops64.source.new", name, u.Label)
	}
	writeJSON(w, http.StatusOK, res)
}

func (m *Module) apiSourceDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := m.DeleteSource(r.Context(), name); err != nil {
		m.apiFail(w, err)
		return
	}
	m.g.Audit(r, "ops64.source.delete", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// apiRun ist ein Lauf in den Antworten der API.
type apiRun struct {
	Start  time.Time  `json:"start"`
	End    *time.Time `json:"end,omitempty"`
	Status string     `json:"status"`
	Detail string     `json:"detail,omitempty"`
}

func toAPIRun(r *Run) *apiRun {
	if r == nil {
		return nil
	}
	out := &apiRun{Start: r.Start.UTC(), Status: r.Status, Detail: r.Detail}
	if !r.End.IsZero() {
		e := r.End.UTC()
		out.End = &e
	}
	return out
}

type apiJob struct {
	Job
	Host         string     `json:"host"`
	Shown        bool       `json:"shown"`
	Readable     bool       `json:"schedule_ok"`
	Frequent     bool       `json:"frequent,omitempty"`
	ExpectedSec  int64      `json:"expected_sec"`
	ExpectedFrom string     `json:"expected_from,omitempty"`
	Next         *time.Time `json:"next,omitempty"`
	Last         *apiRun    `json:"last,omitempty"`
	Overdue      bool       `json:"overdue"`
}

func (m *Module) apiJobs(w http.ResponseWriter, r *http.Request) {
	now := m.now()
	p, err := m.Plan(r.Context(), now.Add(-24*time.Hour), now.Add(time.Minute))
	if err != nil {
		m.apiFail(w, err)
		return
	}
	out := make([]apiJob, 0, len(p.Jobs))
	for _, j := range p.Jobs {
		a := apiJob{Job: j.Job, Host: j.Host, Shown: j.Shown(), Readable: j.Sched != nil, Frequent: j.Frequent, ExpectedSec: int64(j.Expected / time.Second),
			ExpectedFrom: j.ExpectedFrom, Last: toAPIRun(j.Last), Overdue: j.Overdue}
		if !j.Next.IsZero() {
			n := j.Next.UTC()
			a.Next = &n
		}
		out = append(out, a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

type apiItem struct {
	Source  string     `json:"source"`
	Host    string     `json:"host"`
	Job     string     `json:"job,omitempty"`
	Name    string     `json:"name"`
	Kind    string     `json:"kind"`
	Planned *time.Time `json:"planned,omitempty"`
	Start   time.Time  `json:"start"`
	End     time.Time  `json:"end"`
	Status  string     `json:"status"`
	Unclear bool       `json:"duration_unclear,omitempty"`
	Long    bool       `json:"long,omitempty"`
}

// apiPlan: Zeitplan fuer from..to (RFC 3339 oder Datum JJJJ-MM-TT; Standard heute, hoechstens planMaxDays Tage).
func (m *Module) apiPlan(w http.ResponseWriter, r *http.Request) {
	loc := m.g.Timezone(r)
	now := m.now().In(loc)
	y, mo, d := now.Date()
	from := time.Date(y, mo, d, 0, 0, 0, 0, loc)
	to := from.AddDate(0, 0, 1)
	parse := func(s string, def time.Time) (time.Time, bool) {
		if s == "" {
			return def, true
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t, true
		}
		t, err := time.ParseInLocation("2006-01-02", s, loc)
		return t, err == nil
	}
	var ok1, ok2 bool
	from, ok1 = parse(r.URL.Query().Get("from"), from)
	if r.URL.Query().Get("to") == "" {
		to = from.AddDate(0, 0, 1)
	}
	to, ok2 = parse(r.URL.Query().Get("to"), to)
	if !ok1 || !ok2 || !to.After(from) || to.Sub(from) > planMaxDays*24*time.Hour {
		m.apiFail(w, ErrInput)
		return
	}
	p, err := m.Plan(r.Context(), from, to)
	if err != nil {
		m.apiFail(w, err)
		return
	}
	items := []apiItem{}
	for _, it := range p.Items() {
		a := apiItem{Source: it.Source, Host: it.Host, Name: it.Name, Kind: it.Kind, Start: it.Start.UTC(), End: it.End.UTC(), Status: it.Status,
			Unclear: it.Unclear, Long: it.Long}
		if it.Job != nil {
			a.Job = it.Job.ID
		}
		if !it.Planned.IsZero() {
			t := it.Planned.UTC()
			a.Planned = &t
		}
		items = append(items, a)
	}
	over := p.Overlaps
	if over == nil {
		over = []Overlap{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from.UTC(), "to": to.UTC(), "items": items, "overlaps": over})
}
