package ops64

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/ipv64net/node64/internal/notify"
)

// Meldungen: Fehler, ueberfaellige Aufgaben, ungewoehnlich lange Laeufe, gleichzeitig geplante Aufgaben und Hosts,
// die sich nicht mehr melden. Jede Meldung geht einmal ueber die Standardkanaele (je Art abschaltbar) und steht in der
// Liste auf der Seite. Zustaende (gleichzeitig, Host meldet sich nicht) melden sich erneut, wenn sie nach dem Ende
// wieder auftreten.

// Arten der Meldungen.
const (
	AlertFail    = "fail"
	AlertOverdue = "overdue"
	AlertLong    = "long"
	AlertOverlap = "overlap"
	AlertStale   = "stale"
)

// AlertKinds in der Reihenfolge der Anzeige.
var AlertKinds = []string{AlertFail, AlertOverdue, AlertLong, AlertOverlap, AlertStale}

const (
	alertBack    = 24 * time.Hour     // Fehler und ueberfaellige Aufgaben so weit zurueck beachten
	alertAhead   = 8 * 24 * time.Hour // gleichzeitig geplante Aufgaben so weit voraus suchen (deckt woechentliche ab)
	alertFresh   = 2 * time.Hour      // nur was so frisch ist, wird verschickt (kein Schwall nach dem Einrichten)
	staleAlert   = time.Hour          // Host meldet sich so lange nicht: Meldung
	setNotifyPre = "ops64.notify."    // Schalter je Art ("1" an)
)

// Alert ist eine Meldung.
type Alert struct {
	Key    string
	Kind   string
	At     time.Time
	Source string
	Job    string
	Name   string
	Data   map[string]string
	Open   bool
}

func validAlertKind(k string) bool {
	for _, v := range AlertKinds {
		if v == k {
			return true
		}
	}
	return false
}

// NotifyOn: Meldungen dieser Art gehen ueber die Benachrichtigungen (Standard an).
func (m *Module) NotifyOn(ctx context.Context, kind string) bool {
	return m.core.SettingOr(ctx, setNotifyPre+kind, "1") == "1"
}

// SetNotify schaltet die Benachrichtigung einer Art.
func (m *Module) SetNotify(ctx context.Context, kind string, on bool) error {
	if !validAlertKind(kind) {
		return ErrInput
	}
	v := "0"
	if on {
		v = "1"
	}
	return m.core.SetSetting(ctx, setNotifyPre+kind, v)
}

// FormatDur schreibt eine Dauer kurz und ohne Sprache: "45 min", "2 h 41 min", "3 d 4 h".
func FormatDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	min := int(d.Round(time.Minute) / time.Minute)
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + " s"
	case min < 60:
		return strconv.Itoa(min) + " min"
	case min < 24*60:
		return strconv.Itoa(min/60) + " h " + strconv.Itoa(min%60) + " min"
	}
	return strconv.Itoa(min/(24*60)) + " d " + strconv.Itoa(min%(24*60)/60) + " h"
}

// findAlerts liefert, was im Augenblick zu melden ist.
func (m *Module) findAlerts(ctx context.Context, now time.Time) ([]Alert, error) {
	p, err := m.Plan(ctx, now.Add(-alertBack), now.Add(alertAhead))
	if err != nil {
		return nil, err
	}
	var out []Alert
	stamp := func(t time.Time) string { return t.In(time.Local).Format("02.01. 15:04") }
	for _, it := range p.Items() {
		job := ""
		if it.Job != nil {
			job = it.Job.ID
		}
		a := Alert{Source: it.Source, Job: job, Name: it.Name, Data: map[string]string{"job": it.Name, "host": it.Host}}
		switch {
		case it.Status == StFail && it.Run != nil:
			a.Kind, a.At = AlertFail, it.Run.End
			a.Key = AlertFail + "|" + it.Key() + "|" + strconv.FormatInt(it.Run.Start.Unix(), 10)
			a.Data["detail"] = it.Run.Detail
		case it.Status == StOverdue:
			a.Kind, a.At = AlertOverdue, it.Planned.Add(overdueGrace)
			a.Key = AlertOverdue + "|" + it.Key() + "|" + strconv.FormatInt(it.Planned.Unix(), 10)
			a.Data["time"] = stamp(it.Planned)
		case it.Status == StRunning && it.Long && it.Job != nil:
			a.Kind, a.At = AlertLong, now
			a.Key = AlertLong + "|" + it.Key() + "|" + strconv.FormatInt(it.Start.Unix(), 10)
			a.Data["duration"], a.Data["expected"] = FormatDur(now.Sub(it.Start)), FormatDur(it.Job.Expected)
		default:
			continue
		}
		if a.At.Before(now.Add(-alertBack)) {
			continue
		}
		out = append(out, a)
	}
	seen := map[string]bool{}
	for _, o := range p.Overlaps {
		key := AlertOverlap + "|" + strings.Join(o.Keys, "+")
		if !o.End.After(now) || seen[key] { // nur was noch kommt bzw. gerade laeuft, je Gruppe einmal
			continue
		}
		seen[key] = true
		out = append(out, Alert{Key: key, Kind: AlertOverlap, At: now, Name: strings.Join(o.Names, ", "), Open: true,
			Data: map[string]string{"jobs": strings.Join(o.Names, ", "), "time": stamp(o.Start) + "–" + o.End.In(time.Local).Format("15:04")}})
	}
	apiOn := m.APIOn(ctx)
	for _, s := range p.Sources {
		if s.API() && !apiOn { // Abruf ueber die API abgeschaltet: kein "meldet sich nicht"
			continue
		}
		if s.Jobs > 0 && now.Sub(s.Updated) >= staleAlert {
			out = append(out, Alert{Key: AlertStale + "|" + s.Name, Kind: AlertStale, At: now, Source: s.Name, Name: s.Host(), Open: true,
				Data: map[string]string{"host": s.Host(), "since": stamp(s.Updated)}})
		}
	}
	return out, nil
}

// CheckAlerts gleicht die Meldungen mit dem Gedaechtnis ab und verschickt neue (Hintergrundschleife).
func (m *Module) CheckAlerts(ctx context.Context) error {
	now := m.now()
	found, err := m.findAlerts(ctx, now)
	if err != nil {
		return err
	}
	known := map[string]bool{} // key -> besteht noch
	rows, err := m.core.Reader().QueryContext(ctx, `SELECT key, open FROM ops_alerts`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var (
			k    string
			open bool
		)
		if err := rows.Scan(&k, &open); err != nil {
			rows.Close()
			return err
		}
		known[k] = open
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	w, cur := m.core.Writer(), map[string]bool{}
	for _, a := range found {
		cur[a.Key] = true
		open, ok := known[a.Key]
		if ok && (open || !a.Open) { // schon gemeldet (Zustaende: besteht noch)
			continue
		}
		data, _ := json.Marshal(a.Data)
		if _, err := w.ExecContext(ctx, `INSERT INTO ops_alerts (key, kind, at, source, job, name, data, open) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET at = excluded.at, name = excluded.name, data = excluded.data, open = excluded.open`,
			a.Key, a.Kind, a.At.Unix(), a.Source, a.Job, a.Name, string(data), a.Open); err != nil {
			return err
		}
		if m.notifier != nil && m.NotifyOn(ctx, a.Kind) && now.Sub(a.At) <= alertFresh {
			m.send(a, now)
		}
	}
	for k, open := range known { // Zustand vorbei: beim naechsten Auftreten wieder melden
		if open && !cur[k] {
			if _, err := w.ExecContext(ctx, `UPDATE ops_alerts SET open = 0 WHERE key = ?`, k); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Module) send(a Alert, now time.Time) {
	d := map[string]string{"text": "notify.text.ops64_" + a.Kind}
	for k, v := range a.Data {
		d[k] = v
	}
	m.log.Info("notification", "kind", notify.KindOps, "alert", a.Kind, "name", a.Name)
	m.notifier(notify.Alert{EventID: "ops64-" + strconv.FormatInt(now.UnixNano(), 36), Kind: notify.KindOps, Time: now.UTC(),
		Check: notify.CheckInfo{Name: a.Name, NotifyDown: true, NotifyUp: true, Local: true}, Data: d})
}

// Alerts liefert die letzten Meldungen, neueste zuerst.
func (m *Module) Alerts(ctx context.Context, limit int) ([]Alert, error) {
	rows, err := m.core.Reader().QueryContext(ctx, `SELECT key, kind, at, source, job, name, data, open FROM ops_alerts ORDER BY at DESC, key LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		var (
			a    Alert
			at   int64
			data string
		)
		if err := rows.Scan(&a.Key, &a.Kind, &at, &a.Source, &a.Job, &a.Name, &data, &a.Open); err != nil {
			return nil, err
		}
		a.At = unix(at)
		_ = json.Unmarshal([]byte(data), &a.Data)
		out = append(out, a)
	}
	return out, rows.Err()
}
