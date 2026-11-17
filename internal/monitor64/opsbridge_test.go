package monitor64

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ipv64net/node64/internal/proxmox"
)

// TestProxmoxAccesses: nur eingeschaltete Zugaenge pve/pbs nicht pausierter Hosts, mit Adresse, Token und Geheimnis.
func TestProxmoxAccesses(t *testing.T) {
	m, _, _ := newTestModule(t)
	ctx := context.Background()
	by := Actor{Kind: "system"}
	host := func(name, addr string, paused bool) *Host {
		h := &Host{Name: name, Address: addr, Paused: paused}
		if err := m.store.SaveHost(ctx, h, by); err != nil {
			t.Fatal(err)
		}
		return h
	}
	access := func(h *Host, typ, cfg, secret string, on bool) {
		a := &Access{HostUID: h.UID, Type: typ, Config: json.RawMessage(cfg), Enabled: on}
		if secret != "" {
			a.Secret = m.box.Encrypt(secret)
		}
		if err := m.store.SaveAccess(ctx, a, false, by); err != nil {
			t.Fatal(err)
		}
	}
	pve := host("pve-01", "192.0.2.10", false)
	access(pve, "pve", `{"token_id":"node64@pve!ro","node":"pve-01","pin":"`+"ab"+`"}`, "s3cret", true)
	access(pve, "ssh", `{"user":"root"}`, "x", true)
	pbs := host("pbs-01", "192.0.2.20", false)
	access(pbs, "pbs", `{"token_id":"node64@pbs!ro","url":"https://pbs.example:8007"}`, "t0ken", true)
	access(host("pve-02", "192.0.2.11", false), "pve", `{"token_id":"node64@pve!ro"}`, "x", false) // Zugang aus
	access(host("pve-03", "192.0.2.12", true), "pve", `{"token_id":"node64@pve!ro"}`, "x", true)   // Host pausiert
	list, err := m.ProxmoxAccesses(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("accesses: %+v %v", list, err)
	}
	byHost := map[string]ProxmoxAccess{}
	for _, a := range list {
		byHost[a.Host] = a
	}
	if a := byHost["pve-01"]; a.System.Kind != proxmox.KindPVE || a.Node != "pve-01" || a.System.Secret != "s3cret" || a.System.TokenID != "node64@pve!ro" ||
		len(a.System.Addresses) != 1 || a.System.Addresses[0] != "https://192.0.2.10:8006" || a.UID == "" {
		t.Fatalf("pve: %+v", a)
	}
	if a := byHost["pbs-01"]; a.System.Kind != proxmox.KindPBS || a.System.Secret != "t0ken" || a.System.Addresses[0] != "https://pbs.example:8007" {
		t.Fatalf("pbs: %+v", a)
	}
}
