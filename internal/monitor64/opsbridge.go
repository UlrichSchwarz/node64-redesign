package monitor64

import (
	"context"
	"encoding/json"

	"github.com/ipv64net/node64/internal/proxmox"
)

// ProxmoxAccess ist ein eingeschalteter Zugang zu Proxmox VE bzw. Proxmox Backup Server samt Token - nur fuer andere
// Module derselben Node (OPS64 liest darueber Backup-Jobs), nie fuer die API oder die Oberflaeche.
type ProxmoxAccess struct {
	UID    string // Kennung des Zugangs
	Host   string // Name des Hosts in Monitor64
	Node   string // Proxmox VE: Node-Name (leer = die Node hinter der Adresse)
	System proxmox.System
}

// ProxmoxAccesses liefert die eingeschalteten Zugaenge der Arten pve und pbs aller nicht pausierten Hosts. Zugaenge,
// deren Geheimnis sich nicht lesen laesst, fehlen.
func (m *Module) ProxmoxAccesses(ctx context.Context) ([]ProxmoxAccess, error) {
	hosts, err := m.store.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	var out []ProxmoxAccess
	for _, h := range hosts {
		if h.Paused {
			continue
		}
		list, err := m.store.Accesses(ctx, h.UID)
		if err != nil {
			return nil, err
		}
		for _, a := range list {
			kind, port := proxmox.KindPVE, 8006
			switch a.Type {
			case "pve":
			case "pbs":
				kind, port = proxmox.KindPBS, 8007
			default:
				continue
			}
			if !a.Enabled || a.Secret == "" {
				continue
			}
			secret, err := m.box.Decrypt(a.Secret)
			if err != nil {
				continue
			}
			var cfg accessConfig
			_ = json.Unmarshal(a.Config, &cfg)
			addr := cfg.URL
			if addr == "" {
				addr = "https://" + apiHostPort(h.Address, port)
			}
			out = append(out, ProxmoxAccess{UID: a.UID, Host: h.Name, Node: cfg.Node,
				System: proxmox.System{Kind: kind, Name: h.Name, Addresses: splitAddrs(addr), TokenID: cfg.TokenID, Secret: secret, PinSHA256: cfg.Pin}})
		}
	}
	return out, nil
}
