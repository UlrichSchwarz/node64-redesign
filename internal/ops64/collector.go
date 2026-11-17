package ops64

import (
	_ "embed"
	"net/http"
)

// collectorLinuxPy ist der Sammler fuer Linux-Hosts (Proxmox VE, Proxmox Backup Server, systemd-Timer, cron): Python 3
// ohne Zusatzpakete, laeuft per cron auf dem Host und meldet an PUT /api/ops64/sources/{name}.
//
//go:embed collectors/linux.py
var collectorLinuxPy []byte

func (m *Module) collectorLinux(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-python; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="node64-ops.py"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(collectorLinuxPy)
}
