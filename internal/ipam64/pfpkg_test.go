package ipam64

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// Paket fuer pfSense: Aufbau wie pkg-create (Manifeste voran, absolute Pfade, root/wheel), Summen stimmen, der Sammler
// ist das Skript der Node, die Version steht ueberall.
func TestPfPkg(t *testing.T) {
	data, err := pfPkg()
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := buildPfPkg(); !bytes.Equal(data, again) {
		t.Fatal("package is not reproducible")
	}
	zr, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	var names []string
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Uname != "root" || h.Gname != "wheel" || h.Mode != 0o644 || h.Typeflag != tar.TypeReg {
			t.Fatalf("%s: %+v", h.Name, h)
		}
		b, _ := io.ReadAll(tr)
		names = append(names, h.Name)
		files[h.Name] = b
	}
	if len(names) != len(pfPkgFiles)+2 || names[0] != "+COMPACT_MANIFEST" || names[1] != "+MANIFEST" {
		t.Fatalf("members: %v", names)
	}
	var man struct {
		Name, Version, Abi, Prefix string
		Flatsize                   int
		Files                      map[string]pfPkgFile
		Scripts                    map[string]string
	}
	if err := json.Unmarshal(files["+MANIFEST"], &man); err != nil {
		t.Fatal(err)
	}
	if man.Name != pfPkgName || man.Version != pfPkgVersion || man.Prefix != "/usr/local" || len(man.Files) != len(pfPkgFiles) ||
		!strings.Contains(man.Scripts["install"], "rc.packages "+pfPkgName) || !strings.Contains(man.Scripts["deinstall"], "rc.packages "+pfPkgName) {
		t.Fatalf("manifest: %+v", man)
	}
	flat := 0
	for name, meta := range man.Files {
		b, ok := files[name]
		sum := sha256.Sum256(b)
		if !ok || !strings.HasPrefix(name, "/") || meta.Sum != "1$"+hex.EncodeToString(sum[:]) {
			t.Fatalf("file %s: in archive %v, sum %s", name, ok, meta.Sum)
		}
		flat += len(b)
	}
	if flat != man.Flatsize {
		t.Fatalf("flatsize %d, files %d", man.Flatsize, flat)
	}
	var compact map[string]any
	if err := json.Unmarshal(files["+COMPACT_MANIFEST"], &compact); err != nil || compact["version"] != pfPkgVersion || compact["files"] != nil {
		t.Fatalf("compact manifest: %v %v", compact, err)
	}
	if !bytes.Equal(files["/usr/local/pkg/node64ipam-collector.php"], collectorPfsense) {
		t.Fatal("collector differs from collectors/pfsense.php")
	}
	for _, f := range []string{"/usr/local/pkg/node64ipam.xml", "/usr/local/share/" + pfPkgName + "/info.xml"} {
		if s := string(files[f]); !strings.Contains(s, "<version>"+pfPkgVersion+"</version>") || strings.Contains(s, "@VERSION@") {
			t.Fatalf("%s: version missing", f)
		}
	}
	// die Seite des Pakets wertet diese Meldungen des Sammlers aus (node64ipam.inc)
	inc, col := string(files["/usr/local/pkg/node64ipam.inc"]), string(collectorPfsense)
	for _, want := range []string{"Node64 antwortet mit", "Lease-Datei", "Netze, %d feste Adressen, %d Vergaben", "Eintraege uebersprungen"} {
		if !strings.Contains(col, want) {
			t.Fatalf("collector lacks %q", want)
		}
	}
	if !strings.Contains(inc, "/usr/local/pkg/node64ipam-collector.php") || !strings.Contains(inc, "Node64 antwortet mit") {
		t.Fatal("wrapper does not match")
	}
}
