package ops64

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ipv64net/node64/internal/proxmox"
)

// Abruf ueber die API: Backup-Jobs und ihre Laeufe liest die Node auf Wunsch selbst von Proxmox VE und Proxmox Backup
// Server - mit den Zugaengen, die in Monitor64 eingerichtet sind (nur lesend). Auf den Hosts muss dafuer nichts
// installiert sein. Cronjobs und Timer kennt die API nicht; dafuer bleibt der Sammler. Jeder Zugang erscheint als
// eigener Host (Kennung "api-..."), die Daten laufen durch dieselbe Annahme wie die Meldungen der Sammler.

const (
	setAPI     = "ops64.api"      // "1": Abruf ueber die API eingeschaltet (Standard aus)
	apiEvery   = 5 * time.Minute  // Abstand der Abrufe
	apiTimeout = 45 * time.Second // je Zugang
	apiPrefix  = "api "           // Source.Collector der so gelesenen Hosts ("api pve", "api pbs")
)

// APIAccess ist ein Zugang zu Proxmox VE bzw. Proxmox Backup Server (aus Monitor64).
type APIAccess struct {
	UID    string
	Host   string // Anzeigename
	Node   string // Proxmox VE: Node-Name (leer = erkennen)
	System proxmox.System
}

// APIState ist der Stand eines Zugangs fuer die Anzeige.
type APIState struct {
	UID    string
	Host   string
	Kind   string    // pve | pbs
	Source string    // Kennung des Hosts in OPS64
	At     time.Time // letzter Versuch (Null = noch keiner)
	Err    string    // Fehler des letzten Versuchs ("" = hat geklappt)
	Jobs   int
}

// getter ist der Teil des Proxmox-Clients, den der Abruf braucht (Tests ersetzen ihn).
type getter interface {
	Get(ctx context.Context, path string, v any) error
}

// SetAPIAccesses haengt die Quelle der Zugaenge an (vor dem Start). ok = false: Monitor64 laeuft nicht.
func (m *Module) SetAPIAccesses(f func(ctx context.Context) (list []APIAccess, ok bool)) {
	m.apiAccesses = f
}

// APIOn: der Abruf ueber die API ist eingeschaltet.
func (m *Module) APIOn(ctx context.Context) bool { return m.core.SettingOr(ctx, setAPI, "0") == "1" }

// SetAPI schaltet den Abruf ueber die API.
func (m *Module) SetAPI(ctx context.Context, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	return m.core.SetSetting(ctx, setAPI, v)
}

// apiSourceName ist die Kennung des Hosts zu einem Zugang.
func apiSourceName(a APIAccess) string {
	var b strings.Builder
	for _, r := range strings.ToLower(a.UID) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		if b.Len() >= 24 {
			break
		}
	}
	return "api-" + a.System.Kind + "-" + b.String()
}

// APIStates liefert die Zugaenge mit dem Stand ihres letzten Abrufs. ok = false: Monitor64 laeuft nicht.
func (m *Module) APIStates(ctx context.Context) (list []APIState, ok bool) {
	if m.apiAccesses == nil {
		return nil, false
	}
	acc, ok := m.apiAccesses(ctx)
	m.apiMu.Lock()
	defer m.apiMu.Unlock()
	for _, a := range acc {
		st := m.apiStates[a.UID]
		st.UID, st.Host, st.Kind, st.Source = a.UID, a.Host, a.System.Kind, apiSourceName(a)
		list = append(list, st)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Host < list[j].Host })
	return list, ok
}

// PollAPI fragt alle Zugaenge ab und uebernimmt, was sie liefern. Ein Fehler eines Zugangs haelt die anderen nicht auf.
func (m *Module) PollAPI(ctx context.Context) {
	if m.apiAccesses == nil {
		return
	}
	acc, _ := m.apiAccesses(ctx)
	for _, a := range acc {
		if ctx.Err() != nil {
			return
		}
		st := APIState{At: m.now()}
		c, cancel := context.WithTimeout(ctx, apiTimeout)
		u, err := m.fetchAPI(c, a)
		cancel()
		if err == nil {
			var res IngestResult
			if res, err = m.UpdateSource(ctx, apiSourceName(a), u); err == nil {
				st.Jobs = res.Jobs
			}
		}
		if err != nil {
			st.Err = clip(err.Error(), 300)
			m.log.Warn("ops64: api", "host", a.Host, "kind", a.System.Kind, "err", err)
		}
		m.apiMu.Lock()
		if m.apiStates == nil {
			m.apiStates = map[string]APIState{}
		}
		m.apiStates[a.UID] = st
		m.apiMu.Unlock()
	}
}

func (m *Module) fetchAPI(ctx context.Context, a APIAccess) (*SourceUpdate, error) {
	newGetter := m.newGetter
	if newGetter == nil {
		newGetter = func(sys proxmox.System) (getter, error) { return proxmox.NewClient(sys) }
	}
	g, err := newGetter(a.System)
	if err != nil {
		return nil, err
	}
	var u *SourceUpdate
	switch a.System.Kind {
	case proxmox.KindPVE:
		u, err = fetchPVE(ctx, g, a.Node)
	case proxmox.KindPBS:
		u, err = fetchPBS(ctx, g)
	default:
		return nil, errors.New("ops64: unsupported system kind")
	}
	if err != nil {
		return nil, err
	}
	u.Label, u.Collector = a.Host, apiPrefix+a.System.Kind
	return u, nil
}

// flex liest Zahlen und Schalter, die Proxmox mal als Zahl, mal als Text liefert.
type flex string

func (f *flex) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" {
		s = ""
	}
	if strings.HasPrefix(s, `"`) {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		s = v
	}
	*f = flex(s)
	return nil
}

func (f flex) int() int64 {
	n, err := strconv.ParseFloat(strings.TrimSpace(string(f)), 64)
	if err != nil {
		return 0
	}
	return int64(n)
}

// on: 1 bzw. true; leer zaehlt wie def.
func (f flex) on(def bool) bool {
	switch strings.ToLower(strings.TrimSpace(string(f))) {
	case "":
		return def
	case "0", "false":
		return false
	}
	return true
}

// taskResult bewertet den Status einer beendeten Aufgabe: "OK" und "WARNINGS: n" gelten als gelaufen.
func taskResult(status string) (result, detail string) {
	status = strings.TrimSpace(status)
	switch {
	case status == "OK":
		return RunOK, ""
	case strings.HasPrefix(strings.ToUpper(status), "WARNINGS"):
		return RunOK, status
	}
	return RunFail, status
}

type timeInfo struct {
	Timezone string `json:"timezone"`
}

// ---- Proxmox VE ----

type pveBackupJob struct {
	ID        string `json:"id"`
	Schedule  string `json:"schedule"`
	StartTime string `json:"starttime"` // alte Schreibweise, zusammen mit dow
	DOW       string `json:"dow"`
	Storage   string `json:"storage"`
	Node      string `json:"node"`
	VMID      flex   `json:"vmid"`
	Pool      string `json:"pool"`
	All       flex   `json:"all"`
	Enabled   flex   `json:"enabled"`
	Mode      string `json:"mode"`
	Comment   string `json:"comment"`
}

type pveTask struct {
	UPID   string `json:"upid"`
	ID     flex   `json:"id"`
	Start  flex   `json:"starttime"`
	End    flex   `json:"endtime"`
	Status string `json:"status"`
}

// fetchPVE liest Backup-Jobs (/cluster/backup) und Backup-Laeufe (Aufgaben vom Typ vzdump) einer Node.
func fetchPVE(ctx context.Context, g getter, node string) (*SourceUpdate, error) {
	if node == "" {
		var st []struct {
			Type  string `json:"type"`
			Name  string `json:"name"`
			Local flex   `json:"local"`
		}
		if err := g.Get(ctx, "/cluster/status", &st); err != nil {
			return nil, err
		}
		for _, s := range st {
			if s.Type == "node" && s.Local.on(false) {
				node = s.Name
			}
		}
		if node == "" {
			return nil, errors.New("ops64: node name not found in /cluster/status")
		}
	}
	var jobs []pveBackupJob
	if err := g.Get(ctx, "/cluster/backup", &jobs); err != nil {
		return nil, err
	}
	u := &SourceUpdate{}
	for _, j := range jobs {
		if j.ID == "" || (j.Node != "" && j.Node != node) { // Job eines anderen Knotens im Cluster
			continue
		}
		sched := j.Schedule
		if sched == "" && j.StartTime != "" {
			sched = strings.TrimSpace(j.DOW + " " + j.StartTime)
		}
		sel := string(j.VMID)
		switch {
		case j.All.on(false):
			sel = "--all"
		case j.Pool != "":
			sel = "--pool " + j.Pool
		}
		name := j.Comment
		if name == "" {
			name = "vzdump " + sel
		}
		mode := j.Mode
		if mode == "" {
			mode = "snapshot"
		}
		enabled := j.Enabled.on(true)
		u.Jobs = append(u.Jobs, SourceJob{ID: "vzdump-" + j.ID, Name: name, Kind: KindBackup, Target: j.Storage, Schedule: sched, Syntax: SyntaxCalendar,
			Command: "vzdump " + sel + " --storage " + j.Storage + " --mode " + mode, Enabled: &enabled, History: true})
	}
	var tasks []pveTask
	if err := g.Get(ctx, "/nodes/"+url.PathEscape(node)+"/tasks?typefilter=vzdump&source=all&limit=200", &tasks); err != nil {
		u.Notes = append(u.Notes, "tasks: "+err.Error())
	}
	for _, t := range tasks {
		start := t.Start.int()
		if start <= 0 {
			continue
		}
		r := SourceRun{Kind: KindBackup, Name: "vzdump", Start: start, End: t.End.int(), Ref: t.UPID}
		if id := strings.TrimSpace(string(t.ID)); id != "" {
			r.Name = "vzdump " + id
		}
		if r.End > 0 {
			r.Status, r.Detail = taskResult(t.Status)
		}
		u.Runs = append(u.Runs, r)
	}
	var ti timeInfo
	if err := g.Get(ctx, "/nodes/"+url.PathEscape(node)+"/time", &ti); err == nil {
		u.Timezone = ti.Timezone
	}
	return u, nil
}

// ---- Proxmox Backup Server ----

type pbsAdminJob struct {
	ID          string `json:"id"`
	Store       string `json:"store"`
	Remote      string `json:"remote"`
	RemoteStore string `json:"remote-store"`
	Schedule    string `json:"schedule"`
	Comment     string `json:"comment"`
	Disable     flex   `json:"disable"`
	UPID        string `json:"last-run-upid"`
	State       string `json:"last-run-state"`
	End         flex   `json:"last-run-endtime"`
}

type pbsTask struct {
	UPID   string `json:"upid"`
	Type   string `json:"worker_type"`
	WID    string `json:"worker_id"`
	Start  flex   `json:"starttime"`
	End    flex   `json:"endtime"`
	Status string `json:"status"`
}

// pbsWorkers: Art der Aufgabe in der Aufgabenliste je Job-Art.
var pbsWorkers = map[string]string{"syncjob": "sync", "verificationjob": "verify", "prunejob": "prune", "garbage_collection": "gc"}

// upidStart liest die Startzeit aus einer UPID des PBS (UPID:node:pid:pstart:task:starttime:art:id:benutzer:, hex).
func upidStart(upid string) int64 {
	f := strings.Split(upid, ":")
	if len(f) < 6 {
		return 0
	}
	n, err := strconv.ParseInt(f[5], 16, 64)
	if err != nil {
		return 0
	}
	return n
}

// fetchPBS liest Sync-, Verify- und Prune-Jobs, die Garbage Collection (/admin/...) und ihre Laeufe (Aufgabenliste;
// der letzte Lauf steht auch am Job selbst).
func fetchPBS(ctx context.Context, g getter) (*SourceUpdate, error) {
	u := &SourceUpdate{}
	idx := map[string]int{} // art + "\x00" + id -> Index in u.Jobs
	var firstErr error
	failed := 0
	kinds := []string{"sync", "verify", "prune", "gc"}
	for _, kind := range kinds {
		var list []pbsAdminJob
		if err := g.Get(ctx, "/admin/"+kind, &list); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			u.Notes = append(u.Notes, "admin/"+kind+": "+err.Error())
			continue
		}
		for _, j := range list {
			id, name, target := j.ID, kind+" "+j.ID, j.Store
			switch kind {
			case "gc":
				id, name = j.Store, "garbage collection "+j.Store
			case "sync":
				remote := j.Remote
				if remote == "" {
					remote = "local"
				}
				target = remote + ":" + j.RemoteStore + " → " + j.Store
			}
			if id == "" {
				continue
			}
			if j.Comment != "" {
				name = j.Comment
			}
			enabled := !j.Disable.on(false)
			job := SourceJob{ID: "pbs-" + kind + "-" + id, Name: name, Kind: KindBackup, Target: target, Schedule: j.Schedule, Syntax: SyntaxCalendar,
				Enabled: &enabled, History: true}
			if start := upidStart(j.UPID); start > 0 { // letzter Lauf laut Job (laeuft noch: ohne Ende und Ergebnis)
				r := SourceRun{Start: start, Ref: j.UPID}
				if end := j.End.int(); end >= start && strings.TrimSpace(j.State) != "" {
					r.End = end
					r.Status, r.Detail = taskResult(j.State)
				}
				job.Runs = append(job.Runs, r)
			}
			idx[kind+"\x00"+id] = len(u.Jobs)
			u.Jobs = append(u.Jobs, job)
		}
	}
	if failed == len(kinds) {
		return nil, firstErr
	}
	var tasks []pbsTask
	if err := g.Get(ctx, "/nodes/localhost/tasks?limit=300", &tasks); err != nil {
		u.Notes = append(u.Notes, "tasks: "+err.Error())
	}
	for _, t := range tasks {
		kind, ok := pbsWorkers[t.Type]
		start := t.Start.int()
		if !ok || start <= 0 {
			continue
		}
		i, ok := idx[kind+"\x00"+t.WID]
		if !ok { // Sync-, Verify- und Prune-Jobs: die Kennung des Jobs steht am Ende
			i, ok = idx[kind+"\x00"+t.WID[strings.LastIndexByte(t.WID, ':')+1:]]
		}
		if !ok {
			continue
		}
		job := &u.Jobs[i]
		r := SourceRun{Start: start, End: t.End.int(), Ref: t.UPID}
		if r.End > 0 {
			r.Status, r.Detail = taskResult(t.Status)
		}
		dup := false
		for k := range job.Runs { // der letzte Lauf steht schon am Job: die Aufgabenliste weiss mehr (Ende, Ergebnis)
			if job.Runs[k].Ref == r.Ref {
				job.Runs[k], dup = r, true
			}
		}
		if !dup && len(job.Runs) < 50 {
			job.Runs = append(job.Runs, r)
		}
	}
	var ti timeInfo
	if err := g.Get(ctx, "/nodes/localhost/time", &ti); err == nil {
		u.Timezone = ti.Timezone
	}
	return u, nil
}
