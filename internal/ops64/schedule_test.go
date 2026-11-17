package ops64

import (
	"testing"
	"time"
)

func mustSched(t *testing.T, syntax, text string) *Schedule {
	t.Helper()
	s, err := ParseSchedule(syntax, text)
	if err != nil {
		t.Fatalf("%q: %v", text, err)
	}
	return s
}

func clocks(ts []time.Time) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Format("Mon 02.01. 15:04")
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSchedules: cron, systemd und Proxmox, jeweils die Zeitpunkte in einer festen Woche (Mo 16.11.2026 bis So).
func TestSchedules(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip(err)
	}
	from := time.Date(2026, 11, 16, 0, 0, 0, 0, loc)
	for _, c := range []struct {
		syntax, text string
		days         int
		want         []string
	}{
		{"cron", "30 1 * * *", 2, []string{"Mon 16.11. 01:30", "Tue 17.11. 01:30"}},
		{"", "0 3 * * 6,0", 7, []string{"Sat 21.11. 03:00", "Sun 22.11. 03:00"}},
		{"", "0 3 * * 7", 7, []string{"Sun 22.11. 03:00"}},
		{"", "15 6-8/2 * * mon", 7, []string{"Mon 16.11. 06:15", "Mon 16.11. 08:15"}},
		{"", "0 4 18 * 1", 3, []string{"Mon 16.11. 04:00", "Wed 18.11. 04:00"}}, // cron: Tag ODER Wochentag
		{"", "@daily", 1, []string{"Mon 16.11. 00:00"}},
		{"", "*/20 23 * nov *", 1, []string{"Mon 16.11. 23:00", "Mon 16.11. 23:20", "Mon 16.11. 23:40"}},
		{"", "02:00", 2, []string{"Mon 16.11. 02:00", "Tue 17.11. 02:00"}},
		{"", "mon..fri 21:00", 7, []string{"Mon 16.11. 21:00", "Tue 17.11. 21:00", "Wed 18.11. 21:00", "Thu 19.11. 21:00", "Fri 20.11. 21:00"}},
		{"", "sat 03:30", 7, []string{"Sat 21.11. 03:30"}},
		{"", "Sat,Sun *-*-* 10:00:00", 7, []string{"Sat 21.11. 10:00", "Sun 22.11. 10:00"}},
		{"", "*-*-18 04:00:00", 7, []string{"Wed 18.11. 04:00"}},
		{"", "2,22:30", 1, []string{"Mon 16.11. 02:30", "Mon 16.11. 22:30"}},
		{"", "weekly", 7, []string{"Mon 16.11. 00:00"}},
		{"", "Mon *-*-* 06,18:00:00", 2, []string{"Mon 16.11. 06:00", "Mon 16.11. 18:00"}},
		{"", "tue 8..9:0/30", 2, []string{"Tue 17.11. 08:00", "Tue 17.11. 08:30", "Tue 17.11. 09:00", "Tue 17.11. 09:30"}},
		{"", "sun 01:00; wed 05:45", 7, []string{"Wed 18.11. 05:45", "Sun 22.11. 01:00"}},
		{"calendar", "sat..mon 12:00", 7, []string{"Mon 16.11. 12:00", "Sat 21.11. 12:00", "Sun 22.11. 12:00"}},
	} {
		got := clocks(mustSched(t, c.syntax, c.text).Between(from, from.AddDate(0, 0, c.days), loc, 0))
		if !eq(got, c.want) {
			t.Errorf("%q: got %v, want %v", c.text, got, c.want)
		}
	}
	if n := len(mustSched(t, "", "*/30").Between(from, from.AddDate(0, 0, 1), loc, 0)); n != 48 { // Proxmox: nur Minuten
		t.Errorf("*/30: %d", n)
	}
	if n := len(mustSched(t, "", "*:0/15").Between(from, from.AddDate(0, 0, 1), loc, 10)); n != 10 {
		t.Errorf("limit: %d", n)
	}
	if s := mustSched(t, "", "@reboot"); !s.Never() || s.Between(from, from.AddDate(0, 0, 7), loc, 0) != nil {
		t.Error("@reboot has times")
	}
	for _, bad := range []string{"", "bogus", "61 * * * *", "* * *", "*-*~1 04:00", "2027-01-01 00:00", "mon..xyz 02:00", "25:00", "02:00 UTC", "1 2 3 4 5 6"} {
		if _, err := ParseSchedule("", bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}

// TestScheduleDST: eine Stunde, die es am Tag der Umstellung nicht gibt, faellt aus; Next und Prev.
func TestScheduleDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip(err)
	}
	s := mustSched(t, "", "30 2 * * *")
	got := clocks(s.Between(time.Date(2026, 3, 28, 0, 0, 0, 0, loc), time.Date(2026, 3, 31, 0, 0, 0, 0, loc), loc, 0))
	if !eq(got, []string{"Sat 28.03. 02:30", "Mon 30.03. 02:30"}) {
		t.Errorf("dst: %v", got)
	}
	now := time.Date(2026, 11, 18, 2, 30, 0, 0, loc)
	if n := s.Next(now, loc); n.Format("02.01. 15:04") != "19.11. 02:30" {
		t.Errorf("next: %v", n)
	}
	if p := s.Prev(now, loc); !p.Equal(now) {
		t.Errorf("prev: %v", p)
	}
	if p := mustSched(t, "", "*-*-01 04:00").Prev(now, loc); p.Format("02.01. 15:04") != "01.11. 04:00" {
		t.Errorf("prev monthly: %v", p)
	}
}

func TestScheduleSummary(t *testing.T) {
	for _, c := range []struct {
		text, kind, clock string
		n                 int
	}{
		{"30 1 * * *", "daily", "01:30", 0},
		{"mon..fri 21:00", "days", "21:00", 0},
		{"*-*-18 04:00:00", "monthly", "04:00", 18},
		{"hourly", "hourly", "", 0},
		{"*/15 * * * *", "minutes", "", 15},
		{"2,22:30", "", "", 0},
	} {
		kind, clock, days, n := mustSched(t, "", c.text).Summary()
		if kind != c.kind || clock != c.clock || n != c.n {
			t.Errorf("%q: %s %s %d", c.text, kind, clock, n)
		}
		if c.kind == "days" && (len(days) != 5 || days[0] != 1 || days[4] != 5) {
			t.Errorf("%q: days %v", c.text, days)
		}
	}
	if n := mustSched(t, "", "*/5 * * * *").PerDay(); n != 288 {
		t.Errorf("per day: %d", n)
	}
}
