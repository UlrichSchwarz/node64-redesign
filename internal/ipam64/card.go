package ipam64

import (
	"context"

	"github.com/ipv64net/node64/internal/module"
)

// Card liefert die Dashboard-Karte (module.CardProvider): Netze, belegte Adressen, Konflikte.
func (m *Module) Card(ctx context.Context) (*module.Card, error) {
	snap, err := m.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	used, size, conflicts := 0, 0, 0
	for _, n := range snap.Networks {
		used, size, conflicts = used+n.Used, size+n.Size, conflicts+n.Conflicts
	}
	c := &module.Card{TitleKey: "nav.ipam64", Icon: "bi-grid-3x3-gap", Path: "/ipam64", Stats: []module.CardStat{
		{LabelKey: "ipam64.card.networks", Value: int64(len(snap.Networks))},
		{LabelKey: "ipam64.card.used", Value: int64(used)},
		{LabelKey: "ipam64.card.conflicts", Value: int64(conflicts)},
	}}
	if size > 0 {
		c.Stats[1].Pct = float64(used) * 100 / float64(size)
	}
	if conflicts > 0 {
		c.Stats[2].Class = "text-danger"
	}
	return c, nil
}
