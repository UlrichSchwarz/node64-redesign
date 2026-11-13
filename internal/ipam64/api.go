package ipam64

import (
	"encoding/json"
	"net/http"

	"github.com/ipv64net/node64/internal/module"
)

// Lokale JSON-API des Moduls (Doku: internal/web/static/openapi.json, Tag "IPAM64"). Ist das Modul aus: 404.

func (m *Module) apiRoutes(mux *http.ServeMux, g module.Guards) {
	api := func(h http.HandlerFunc) http.Handler { return g.Enabled(g.Auth(h)) }
	mux.Handle("GET /api/ipam64/networks", api(m.apiNetworks))
	mux.Handle("GET /api/ipam64/addresses", api(m.apiAddresses))
	mux.Handle("POST /api/ipam64/scan", api(m.apiScan))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (m *Module) apiFail(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errKey(err) == "" {
		m.log.Error("ipam64: api", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if err == ErrNotFound {
		status = http.StatusNotFound
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func (m *Module) apiNetworks(w http.ResponseWriter, r *http.Request) {
	snap, err := m.Snapshot(r.Context())
	if err != nil {
		m.apiFail(w, err)
		return
	}
	out := snap.Networks
	if out == nil {
		out = []Network{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"networks": out})
}

// AddressOut ist eine belegte Adresse mit ihrem Netz (GET /api/ipam64/addresses).
type AddressOut struct {
	Network string `json:"network"`
	Entry
}

// apiAddresses: belegte Adressen aller Netze bzw. eines Netzes (?network=192.168.1.0/24).
func (m *Module) apiAddresses(w http.ResponseWriter, r *http.Request) {
	snap, err := m.Snapshot(r.Context())
	if err != nil {
		m.apiFail(w, err)
		return
	}
	only := r.URL.Query().Get("network")
	if only != "" && snap.Network(only) == nil {
		m.apiFail(w, ErrNotFound)
		return
	}
	out := []AddressOut{}
	for _, n := range snap.Networks {
		if only != "" && n.Subnet != only {
			continue
		}
		for _, e := range snap.Entries(n.Prefix) {
			if e.Sources == nil {
				e.Sources = []string{}
			}
			out = append(out, AddressOut{n.Subnet, e})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"addresses": out})
}

// apiScan: Netz-Scan eines direkt angeschlossenen Netzes ({"network":"192.168.1.0/24"}).
func (m *Module) apiScan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Network string `json:"network"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil {
		m.apiFail(w, ErrSubnet)
		return
	}
	n, err := m.Scan(r.Context(), in.Network)
	if err != nil {
		m.apiFail(w, err)
		return
	}
	m.g.Audit(r, "ipam64.scan", in.Network, "")
	writeJSON(w, http.StatusOK, map[string]any{"network": in.Network, "found": n})
}
