package serve

import (
	"fmt"
	"testing"

	"github.com/jamesbraid/instigator/efs/efstest"
	"github.com/jamesbraid/instigator/internal/config"
)

// irix53Config serves one set laid out like the IRIX 5.3 disc, whose dist
// ships no .related_dists, followed by extra config text.
func irix53Config(t *testing.T, extra string) *config.Config {
	t.Helper()
	img := efstest.New()
	img.SetRoot(map[string]uint32{
		"stand": img.AddDir(map[string]uint32{
			"fx.ARCS": img.AddFile(0o644, []byte("fake fx.ARCS")),
			"fx.IP12": img.AddFile(0o644, []byte("fake fx.IP12")),
		}),
		"dist": img.AddDir(map[string]uint32{"sa": img.AddFile(0o644, []byte("SA"))}),
	})
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
server_ip: 127.0.0.1
clients:
  - {name: indigo, mac: "08:00:69:0e:af:12", ip: 127.0.0.1}
install_sets:
  - name: "5.3"
    layers:
      - {name: irix53, source: %q, boot: true}
%s`, writeImage(t, t.TempDir(), "irix53.image", img), extra)))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Ports = config.Ports{}
	return cfg
}

// The 5.3 disc boots ARCS PROMs and the R3000 Indigo, and its inst has no
// admin source, so the operator types the selection.
func TestStartupServesIRIX53(t *testing.T) {
	s, c := captureStart(t, irix53Config(t, ""))
	for _, want := range []string{
		"  PROM: boot -f bootp():/5.3/stand/fx.ARCS  (32-bit ARCS PROMs)",
		"  PROM: boot -f bootp():/5.3/stand/fx.IP12  (R3000 Indigo)",
		"  Inst>: from 127.0.0.1:/5.3/dist",
		"  Inst>: install default",
		"  Inst>: go",
	} {
		if !hasLine(c.instructions, want) {
			t.Errorf("instructions missing %q:\n%v", want, c.instructions)
		}
	}
	if hasSubstring(c.instructions, "admin source") {
		t.Errorf("instructions offer admin source to a 5.3 inst:\n%v", c.instructions)
	}
	if hasSubstring(c.log, "WARN") {
		t.Errorf("warning with no install scripts:\n%v", c.log)
	}
	if got := tftpGet(t, s, "/5.3/stand/fx.IP12"); string(got) != "fake fx.IP12" {
		t.Errorf("fx.IP12 = %q", got)
	}
}

// Install scripts need admin source, so configuring one for 5.3 is a mistake
// startup names.
func TestStartupWarnsOfIRIX53InstallScripts(t *testing.T) {
	_, c := captureStart(t, irix53Config(t, "install_scripts:\n  - {name: irix}\n"))
	if !hasSubstring(c.log, "older than IRIX 6.5") {
		t.Errorf("no warning about unusable install scripts:\n%v", c.log)
	}
	if !hasSubstring(c.log, "/5.3/dist has no .related_dists") {
		t.Errorf("the warning does not name its evidence:\n%v", c.log)
	}
}
