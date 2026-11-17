package ops64

import (
	"context"
	"time"

	"github.com/ipv64net/node64/internal/module"
)

// Card liefert die Dashboard-Karte (module.CardProvider): Aufgaben heute, laufende, fehlgeschlagene (24 Stunden) und
// ueberfaellige.
func (m *Module) Card(ctx context.Context) (*module.Card, error) {
	now := m.now()
	p, err := m.Plan(ctx, now.Add(-24*time.Hour), now.Add(24*time.Hour))
	if err != nil {
		return nil, err
	}
	var planned, failed, overdue int64
	for _, it := range p.Items() {
		switch {
		case it.Status == StFail:
			failed++
		case it.Status == StPlanned && it.Start.After(now):
			planned++
		}
	}
	for _, j := range p.Jobs {
		if j.Overdue && j.Planned() {
			overdue++
		}
	}
	c := &module.Card{TitleKey: "nav.ops64", Icon: "bi-calendar2-check", Path: "/ops64", Stats: []module.CardStat{
		{LabelKey: "ops64.card.planned", Value: planned},
		{LabelKey: "ops64.card.failed", Value: failed},
		{LabelKey: "ops64.card.overdue", Value: overdue},
	}}
	if failed > 0 {
		c.Stats[1].Class = "text-danger"
	}
	if overdue > 0 {
		c.Stats[2].Class = "text-warning"
	}
	return c, nil
}
