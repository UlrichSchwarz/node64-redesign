// Package ops64 ist das optionale Node64-Modul OPS64: Betrieb und Wartung. Hosts (Proxmox VE, Container, andere
// Linux-Systeme) melden ihre Backup-Jobs, Cronjobs und Timer samt Laeufen ueber die API (Sammler unter collectors);
// die Node liest die Zeitplaene selbst, zeigt alles in einem Zeitplan, erkennt gleichzeitige Aufgaben und meldet
// Fehler, ueberfaellige Aufgaben und ungewoehnlich lange Laeufe ueber die Standardkanaele.
//
// OPS64 liest nur: Es startet und sperrt nichts auf den Hosts. Standard aus: ausgeschaltet gibt es keinen Menuepunkt
// und keine Hintergrundarbeit; gemeldete Daten werden dann abgewiesen (404).
package ops64

import (
	"context"
	_ "embed"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ipv64net/node64/internal/module"
	"github.com/ipv64net/node64/internal/notify"
	"github.com/ipv64net/node64/internal/proxmox"
	"github.com/ipv64net/node64/internal/store"
)

// changelog wird unter Einstellungen > Module angezeigt (Version siehe Info).
//
//go:embed CHANGELOG.md
var changelog string

//go:embed CHANGELOG.en.md
var changelogEN string

// Name ist der technische Schluessel des Moduls.
const Name = "ops64"

const (
	version = "0.2.0"

	checkEvery = time.Minute         // Meldungen pruefen
	runKeep    = 90 * 24 * time.Hour // Laeufe aufbewahren
	alertKeep  = 30 * 24 * time.Hour // erledigte Meldungen aufbewahren
	stopWait   = 10 * time.Second
)

// Module implementiert module.Module.
type Module struct {
	core *store.Store
	log  *slog.Logger
	g    module.Guards
	now  func() time.Time

	notifier func(notify.Alert)

	apiAccesses func(ctx context.Context) ([]APIAccess, bool) // Zugaenge aus Monitor64 (apipoll.go)
	newGetter   func(sys proxmox.System) (getter, error)      // nil = proxmox.NewClient (Tests ersetzen es)
	apiMu       sync.Mutex
	apiStates   map[string]APIState // Stand des letzten Abrufs je Zugang (nur im Speicher)
	apiAt       time.Time           // letzter Abruf aller Zugaenge

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// New erzeugt das Modul.
func New(core *store.Store, log *slog.Logger) *Module {
	return &Module{core: core, log: log.With("module", Name), now: time.Now}
}

// SetNotifier haengt die Benachrichtigungen an (vor dem Start).
func (m *Module) SetNotifier(f func(notify.Alert)) { m.notifier = f }

func (m *Module) Info() module.Info {
	return module.Info{Name: Name, TitleKey: "nav.ops64", Icon: "bi-calendar2-check", Path: "/ops64", Available: true, Order: 46,
		Version: version, Changelog: changelog, ChangelogEN: changelogEN}
}

func (m *Module) Migrations() []store.Migration { return Migrations() }

func (m *Module) Routes(mux *http.ServeMux, g module.Guards) {
	m.g = g
	m.uiRoutes(mux, g)
	m.apiRoutes(mux, g)
}

// Start prueft ab jetzt jede Minute, ob etwas zu melden ist.
func (m *Module) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		return nil
	}
	life, cancel := context.WithCancel(context.Background())
	m.cancel, m.done = cancel, make(chan struct{})
	go m.loop(life, m.done)
	return nil
}

// Stop beendet die Hintergrundarbeit.
func (m *Module) Stop(ctx context.Context) error {
	m.mu.Lock()
	cancel, done := m.cancel, m.done
	m.cancel, m.done = nil, nil
	m.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-time.After(stopWait):
	case <-ctx.Done():
	}
	return nil
}

func (m *Module) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	var pruned time.Time
	for {
		now := m.now()
		if now.Sub(pruned) >= 24*time.Hour {
			if err := m.prune(ctx, now); err != nil && ctx.Err() == nil {
				m.log.Warn("ops64: prune", "err", err)
			}
			pruned = now
		}
		if m.APIOn(ctx) && m.apiDue(now) {
			m.PollAPI(ctx)
		}
		if err := m.CheckAlerts(ctx); err != nil && ctx.Err() == nil {
			m.log.Warn("ops64: alerts", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// apiDue meldet, ob der naechste Abruf ueber die API faellig ist, und merkt ihn vor.
func (m *Module) apiDue(now time.Time) bool {
	m.apiMu.Lock()
	defer m.apiMu.Unlock()
	if !m.apiAt.IsZero() && now.Sub(m.apiAt) < apiEvery {
		return false
	}
	m.apiAt = now
	return true
}
