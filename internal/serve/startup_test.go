package serve

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamesbraid/instigator/efs/efstest"
	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/logging"
)

// combinedImage writes one CD image: dist/ holds the given entries, and
// stand/fx.64 is present only when withStand, so a test can cover both
// the disc that can boot the miniroot and the one that cannot.
func combinedImage(t *testing.T, dir, filename string, withStand bool, dist func(img *efstest.Builder) map[string]uint32) string {
	t.Helper()
	img := efstest.New()
	root := map[string]uint32{"dist": img.AddDir(dist(img))}
	if withStand {
		fx := img.AddFile(0o755, []byte("fake fx binary"))
		root["stand"] = img.AddDir(map[string]uint32{"fx.64": fx})
	}
	img.SetRoot(root)
	p := filepath.Join(dir, filename)
	if err := os.WriteFile(p, img.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// combinedSet writes the two-disc shape a real 6.5.30 set has in
// miniature: a plain foundation disc, and the Installation-Tools disc
// that carries both dist/.related_dists (which makes it the primary) and
// the miniroot's stand/fx.64. Returned in config order.
func combinedSet(t *testing.T) (foundation, primary string) {
	t.Helper()
	dir := t.TempDir()
	foundation = combinedImage(t, dir, "foundation1.iso", false, func(img *efstest.Builder) map[string]uint32 {
		return map[string]uint32{"foundation.sw": img.AddFile(0o644, []byte("F"))}
	})
	primary = combinedImage(t, dir, "tools.image", true, func(img *efstest.Builder) map[string]uint32 {
		return map[string]uint32{
			"sa":             img.AddFile(0o644, []byte("SA")),
			".related_dists": img.AddFile(0o555, []byte("CD\n")),
		}
	})
	return foundation, primary
}

// captureStart runs Start over the given config and returns every line it
// logged, with the servers closed again.
func captureStart(t *testing.T, cfg *config.Config) []string {
	t.Helper()
	var logbuf syncBuffer
	logger := logging.New(&logbuf, logging.LevelDebug)
	s, err := Start(cfg, logger, WithRSHHighPorts())
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	t.Logf("server log:\n%s", logbuf.String())
	return splitNonEmpty(logbuf.String())
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// hasLine reports whether any line ends with want: each line now carries
// a leading "<ISO8601> <LEVEL> " prefix from the leveled logger, so an
// exact match against the bare message never succeeds.
func hasLine(lines []string, want string) bool {
	for _, l := range lines {
		if strings.HasSuffix(l, want) {
			return true
		}
	}
	return false
}

// combinedConfig serves the media set the other serve tests use plus a
// combined set named irix6.5.30 whose primary disc is slugged "tools".
func combinedConfig(t *testing.T) *config.Config {
	t.Helper()
	foundation, primary := combinedSet(t)
	yaml := fmt.Sprintf(`
server_ip: 192.0.2.10
clients:
  - {name: octane, mac: "08:00:69:0e:af:12", ip: 127.0.0.1}
media:
  - {name: "6.5.30", discs: %q}
combined:
  - name: irix6.5.30
    layers: [%q, %q]
    disc_names: {"foundation1.iso": foundation1, "tools.image": tools}
`, mediaDir(t), foundation, primary)
	c, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	c.Ports = config.Ports{}
	return c
}

// The whole point of a combined set is that one "from" opens every disc,
// so the startup log has to hand the operator that exact command next to
// the PROM line that boots the same disc's miniroot - not leave them to
// assemble it from the "combined:" line and a media disc's boot hint.
func TestStartupLogsCombinedRecipe(t *testing.T) {
	lines := captureStart(t, combinedConfig(t))

	for _, want := range []string{
		"  PROM: boot -f bootp():/irix6.5.30/tools/stand/fx.64",
		"  Inst>: from 192.0.2.10:/irix6.5.30/tools/dist",
	} {
		if !hasLine(lines, want) {
			t.Errorf("startup log missing %q, got:\n%s", want, strings.Join(lines, "\n"))
		}
	}
}

// A combined set whose primary disc carries no miniroot still has a valid
// "from" - the operator boots fx from a media disc instead - so the Inst>
// line must survive on its own rather than be suppressed with the PROM
// line it normally accompanies.
func TestStartupOmitsCombinedPROMLineWithoutFx(t *testing.T) {
	dir := t.TempDir()
	foundation := combinedImage(t, dir, "foundation1.iso", false, func(img *efstest.Builder) map[string]uint32 {
		return map[string]uint32{"foundation.sw": img.AddFile(0o644, []byte("F"))}
	})
	primary := combinedImage(t, dir, "tools.image", false, func(img *efstest.Builder) map[string]uint32 {
		return map[string]uint32{".related_dists": img.AddFile(0o555, []byte("CD\n"))}
	})
	yaml := fmt.Sprintf(`
server_ip: 192.0.2.10
clients:
  - {name: octane, mac: "08:00:69:0e:af:12", ip: 127.0.0.1}
media:
  - {name: "6.5.30", discs: %q}
combined:
  - name: irix6.5.30
    layers: [%q, %q]
    disc_names: {"foundation1.iso": foundation1, "tools.image": tools}
`, mediaDir(t), foundation, primary)
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Ports = config.Ports{}

	lines := captureStart(t, cfg)
	if hasLine(lines, "  PROM: boot -f bootp():/irix6.5.30/tools/stand/fx.64") {
		t.Errorf("PROM line logged for a primary disc with no stand/fx.64, got:\n%s", strings.Join(lines, "\n"))
	}
	if !hasLine(lines, "  Inst>: from 192.0.2.10:/irix6.5.30/tools/dist") {
		t.Errorf("Inst> line dropped along with the PROM line, got:\n%s", strings.Join(lines, "\n"))
	}
}

// Without a combined set there is nothing to point inst at, so the log
// must not grow a bare "Inst>:" line with an empty path.
func TestStartupLogsNoInstLineWithoutCombined(t *testing.T) {
	lines := captureStart(t, testConfig(t))
	for _, l := range lines {
		if strings.Contains(l, "Inst>:") {
			t.Errorf("media-only config logged an Inst> line: %q", l)
		}
	}
}
