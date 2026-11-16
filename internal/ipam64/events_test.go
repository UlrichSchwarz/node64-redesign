package ipam64

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func mustP(s string) netip.Prefix { return netip.MustParsePrefix(s) }

func kinds(list []Event) string {
	var out []string
	for i := len(list) - 1; i >= 0; i-- { // aelteste zuerst
		out = append(out, list[i].Kind+" "+list[i].IP)
	}
	return strings.Join(out, ", ")
}

// Ereignisse aus dem Vergleich zweier Meldungen: neue Vergabe, Verlaengerung (keins), Geraetewechsel, abgelaufen,
// zurueckgegeben, feste Zuweisungen, Netze; die erste Meldung zaehlt nur als solche.
func TestEvents(t *testing.T) {
	m, _, _ := modTest(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	m.now = func() time.Time { return now }
	lease := func(ip, mac, name string, in time.Duration) SourceLease {
		return SourceLease{IP: ip, MAC: mac, Name: name, Expires: FlexTime{now.Add(in)}}
	}
	net := []SourceNetwork{{Subnet: "192.168.1.0/24", Name: "LAN"}}
	res, err := m.UpdateSource(ctx, "pfsense", SourceUpdate{Label: "pfSense", Networks: net,
		Reservations: []SourceLease{{IP: "192.168.1.10", MAC: "02:00:00:00:00:10", Name: "nas"}, {IP: "192.168.1.11", MAC: "02:00:00:00:00:11", Name: "pve"}},
		Leases: []SourceLease{lease("192.168.1.101", "02:00:00:00:01:01", "macbook", time.Hour), lease("192.168.1.102", "02:00:00:00:01:02", "iphone", 2*time.Minute),
			lease("192.168.1.103", "02:00:00:00:01:03", "tv", time.Hour), lease("192.168.1.104", "02:00:00:00:01:04", "tablet", time.Hour)}})
	if err != nil || res.Events != 1 {
		t.Fatalf("first report: %+v %v", res, err)
	}
	list, _ := m.Events(ctx, EventFilter{})
	if kinds(list) != "source_new " || list[0].Label != "pfSense" {
		t.Fatalf("first: %s", kinds(list))
	}
	// 5 Minuten spaeter: .101 verlaengert, .102 abgelaufen, .103 an ein anderes Geraet, .104 vor Ablauf weg, .105 neu;
	// feste Zuweisung .10 auf neue MAC, .11 entfernt, .12 neu; Netz IoT dazu
	expired := now.Add(2 * time.Minute)
	now = now.Add(5 * time.Minute)
	res, err = m.UpdateSource(ctx, "pfsense", SourceUpdate{Label: "pfSense", Networks: append(net, SourceNetwork{Subnet: "192.168.20.0/24"}),
		Reservations: []SourceLease{{IP: "192.168.1.10", MAC: "02:00:00:00:00:99", Name: "nas"}, {IP: "192.168.1.12", MAC: "02:00:00:00:00:12", Name: "neu"}},
		Leases: []SourceLease{lease("192.168.1.101", "02:00:00:00:01:01", "macbook", 2*time.Hour), lease("192.168.1.103", "02:00:00:00:01:ff", "konsole", time.Hour),
			lease("192.168.1.105", "02:00:00:00:01:05", "gast", time.Hour)}})
	if err != nil || res.Events != 8 {
		t.Fatalf("second report: %+v %v", res, err)
	}
	list, _ = m.Events(ctx, EventFilter{})
	want := "source_new , lease_expired 192.168.1.102, net_new 192.168.20.0/24, fixed_change 192.168.1.10, fixed_gone 192.168.1.11, fixed_new 192.168.1.12, " +
		"lease_change 192.168.1.103, lease_released 192.168.1.104, lease_new 192.168.1.105"
	if got := kinds(list); got != want {
		t.Fatalf("events:\n got %s\nwant %s", got, want)
	}
	for _, e := range list {
		switch e.Kind {
		case EvLeaseExpired:
			if !e.Time.Equal(expired) || e.Name != "iphone" || e.MAC != "02:00:00:00:01:02" {
				t.Fatalf("expired: %+v", e)
			}
		case EvLeaseChange:
			if e.MAC != "02:00:00:00:01:ff" || e.Detail != "02:00:00:00:01:03" || e.Name != "konsole" || !e.Time.Equal(now) {
				t.Fatalf("change: %+v", e)
			}
		case EvFixedChange:
			if e.Detail != "02:00:00:00:00:10" {
				t.Fatalf("fixed change: %+v", e)
			}
		}
	}
	// dieselbe Meldung noch einmal: nichts Neues
	now = now.Add(5 * time.Minute)
	res, _ = m.UpdateSource(ctx, "pfsense", SourceUpdate{Label: "pfSense", Networks: append(net, SourceNetwork{Subnet: "192.168.20.0/24"}),
		Reservations: []SourceLease{{IP: "192.168.1.10", MAC: "02:00:00:00:00:99", Name: "nas"}, {IP: "192.168.1.12", MAC: "02:00:00:00:00:12", Name: "neu"}},
		Leases: []SourceLease{lease("192.168.1.101", "02:00:00:00:01:01", "macbook", 2*time.Hour), lease("192.168.1.103", "02:00:00:00:01:ff", "konsole", time.Hour),
			lease("192.168.1.105", "02:00:00:00:01:05", "gast", time.Hour)}})
	if res.Events != 0 {
		t.Fatalf("repeat: %+v", res)
	}
	// Filter
	for f, n := range map[EventFilter]int{{Kind: EvLeaseNew}: 1, {IP: "192.168.1.103"}: 1, {Query: "KONSOLE"}: 1, {Query: "01:03"}: 1, {Source: "andere"}: 0,
		{Prefix: mustP("192.168.20.0/24")}: 1, {Prefix: mustP("192.168.1.0/24")}: 7, {Prefix: mustP("192.168.1.104/30")}: 2} {
		if got, _ := m.Events(ctx, f); len(got) != n {
			t.Errorf("filter %+v: %d, want %d", f, len(got), n)
		}
	}
	// die Tabelle bleibt klein: hoechstens eventsMax, nichts aelter als eventsKeep
	tx, _ := m.core.Writer().BeginTx(ctx, nil)
	var many []Event
	for range eventsMax + 50 {
		many = append(many, Event{Time: now, Kind: EvLeaseNew, IP: "192.168.1.200"})
	}
	if err := writeEvents(ctx, tx, "pfsense", "pfSense", many, now); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit()
	if list, _ = m.Events(ctx, EventFilter{}); len(list) != eventsMax {
		t.Fatalf("cap: %d", len(list))
	}
	now = now.Add(eventsKeep + time.Hour)
	tx, _ = m.core.Writer().BeginTx(ctx, nil)
	_ = writeEvents(ctx, tx, "pfsense", "pfSense", []Event{{Time: now, Kind: EvNetNew, IP: "10.0.0.0/24"}}, now)
	_ = tx.Commit()
	if list, _ = m.Events(ctx, EventFilter{}); len(list) != 1 {
		t.Fatalf("age: %d", len(list))
	}
}
