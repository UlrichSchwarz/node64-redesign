package ipam64

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// Paket fuer pfSense (pfSense-pkg-Node64IPAM): eigene Seite unter Services > Node64 IPAM64 (Adresse, API-Key, Intervall,
// Vorschau, Senden), Zeitplan ueber install_cron_job und als Sammler collectors/pfsense.php. Die Node baut das Paket
// beim ersten Abruf selbst (FreeBSD-pkg: tar mit +COMPACT_MANIFEST und +MANIFEST voran, zstd), so passt der Sammler
// immer zur Node. Die Dateien unter collectors/pfsense-pkg stammen aus dem Paket 0.1.1 der Community.

const (
	pfPkgName    = "pfSense-pkg-Node64IPAM"
	pfPkgVersion = "0.2.0"    // bei jeder Aenderung am Paket oder am Sammler erhoehen
	pfPkgTime    = 1794787200 // feste Zeit der Dateien (2026-11-16 UTC): gleicher Inhalt, gleiches Paket
	pfPkgABI     = "FreeBSD:16:amd64"
)

//go:embed collectors/pfsense-pkg
var pfPkgFS embed.FS

// pfPkgFiles: Ziel auf der pfSense -> Quelle (leer = der Sammler collectors/pfsense.php).
var pfPkgFiles = [][2]string{
	{"/etc/inc/priv/node64ipam.priv.inc", "node64ipam.priv.inc"},
	{"/usr/local/pkg/node64ipam-collector.php", ""},
	{"/usr/local/pkg/node64ipam-run.php", "node64ipam-run.php"},
	{"/usr/local/pkg/node64ipam.inc", "node64ipam.inc"},
	{"/usr/local/pkg/node64ipam.xml", "node64ipam.xml"},
	{"/usr/local/share/" + pfPkgName + "/info.xml", "info.xml"},
	{"/usr/local/www/packages/node64ipam/index.php", "index.php"},
}

// PfPkgFileName ist der Dateiname des Pakets zum Herunterladen.
func PfPkgFileName() string { return pfPkgName + "-" + pfPkgVersion + ".pkg" }

var (
	pfPkgOnce sync.Once
	pfPkgData []byte
	pfPkgErr  error
)

// pfPkg liefert das fertige Paket (einmal gebaut).
func pfPkg() ([]byte, error) {
	pfPkgOnce.Do(func() { pfPkgData, pfPkgErr = buildPfPkg() })
	return pfPkgData, pfPkgErr
}

type pfPkgFile struct {
	Sum   string `json:"sum"`
	Uname string `json:"uname"`
	Gname string `json:"gname"`
	Perm  string `json:"perm"`
	Mtime int64  `json:"mtime"`
}

func buildPfPkg() ([]byte, error) {
	type entry struct {
		name string
		data []byte
	}
	var files []entry
	meta := map[string]pfPkgFile{}
	flat := 0
	for _, f := range pfPkgFiles {
		data := collectorPfsense
		if f[1] != "" {
			b, err := pfPkgFS.ReadFile("collectors/pfsense-pkg/" + f[1])
			if err != nil {
				return nil, err
			}
			data = []byte(strings.ReplaceAll(string(b), "@VERSION@", pfPkgVersion))
		}
		sum := sha256.Sum256(data)
		meta[f[0]] = pfPkgFile{Sum: "1$" + hex.EncodeToString(sum[:]), Uname: "root", Gname: "wheel", Perm: "0644", Mtime: pfPkgTime}
		files = append(files, entry{f[0], data})
		flat += len(data)
	}
	// Felder wie pkg-create sie schreibt; encoding/json sortiert die Schluessel einer map (Dateien) nach Namen.
	head := []any{
		"name", pfPkgName, "origin", "sysutils/" + pfPkgName, "version", pfPkgVersion,
		"comment", "Node64 IPAM64 DHCP collector for pfSense", "maintainer", "nobody@example.invalid", "www", "https://github.com/ipv64net/node64",
		"abi", pfPkgABI, "arch", "freebsd:16:x86:64", "prefix", "/usr/local", "flatsize", flat, "licenselogic", "single", "licenses", []string{"APACHE20"},
		"desc", "Collect DHCPv4 networks, static mappings, and Kea leases from pfSense\nand report them to the IPAM64 module of a Node64 instance. Configure the\ndestination and the optional schedule in the pfSense web interface.",
		"categories", []string{"sysutils"}, "annotations", map[string]string{"FreeBSD_version": "1600018"},
	}
	scripts := map[string]string{
		"install":   "#!/bin/sh\n[ \"$2\" = POST-INSTALL ] || exit 0\n${PKG_ROOTDIR}/usr/local/bin/php -f ${PKG_ROOTDIR}/etc/rc.packages " + pfPkgName + " \"$2\"",
		"deinstall": "#!/bin/sh\n/usr/local/bin/php -f /etc/rc.packages " + pfPkgName + " \"$2\"",
	}
	compact, err := orderedJSON(head)
	if err != nil {
		return nil, err
	}
	manifest, err := orderedJSON(append(head, "files", meta, "scripts", scripts))
	if err != nil {
		return nil, err
	}
	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	add := func(name string, data []byte, mtime int64) error {
		h := &tar.Header{Typeflag: tar.TypeReg, Name: name, Size: int64(len(data)), Mode: 0o644, Uname: "root", Gname: "wheel", Format: tar.FormatUSTAR}
		h.ModTime = time.Unix(mtime, 0)
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	if err := add("+COMPACT_MANIFEST", compact, 0); err != nil {
		return nil, err
	}
	if err := add("+MANIFEST", manifest, 0); err != nil {
		return nil, err
	}
	for _, f := range files {
		if err := add(f.name, f.data, pfPkgTime); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	zw, err := zstd.NewWriter(&out)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(raw.Bytes()); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// orderedJSON schreibt ein JSON-Objekt mit den Schluesseln in der gegebenen Reihenfolge (kv: Schluessel, Wert, ...).
func orderedJSON(kv []any) ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(kv[i])
		if err != nil {
			return nil, err
		}
		v, err := json.Marshal(kv[i+1])
		if err != nil {
			return nil, err
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// servePfPkg liefert das Paket fuer die pfSense zum Herunterladen.
func (m *Module) servePfPkg(w http.ResponseWriter, r *http.Request) {
	data, err := pfPkg()
	if err != nil {
		m.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+PfPkgFileName()+`"`)
	_, _ = w.Write(data)
}
