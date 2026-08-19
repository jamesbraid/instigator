package serve

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/jamesbraid/instigator/efs/efstest"
	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/instscript"
	"github.com/jamesbraid/instigator/internal/logging"
)

// primaryImage writes the image the first set's first layer draws from.
// It carries the stock dist/.related_dists a real Installation Tools
// disc ships - which the generated menu aid has to shadow - and
// stand/fx.64 only when withStand, so a test can cover both the set that
// can boot the miniroot and the one that cannot.
func primaryImage(t *testing.T, dir, name string, withStand bool) string {
	t.Helper()
	img := efstest.New()
	dist := map[string]uint32{
		"sa":             img.AddFile(0o644, []byte("SA")),
		".related_dists": img.AddFile(0o555, []byte("stock media copy\n")),
	}
	root := map[string]uint32{"dist": img.AddDir(dist)}
	if withStand {
		fx := img.AddFile(0o755, []byte("fake fx binary"))
		root["stand"] = img.AddDir(map[string]uint32{"fx.64": fx})
	}
	img.SetRoot(root)
	return writeImage(t, dir, name, img)
}

// startupCapture separates Start's two output audiences: log is the
// leveled server log (the install-set inventory, client config - what
// this server is doing), instructions is the operator's PROM/Inst>
// commands (what a human does next). They used to be the same stream;
// James's directive was that they never should have been - see
// logStartup's own comment.
type startupCapture struct {
	log          []string
	instructions []string
}

// captureStart runs Start over the given config and returns the running
// servers - so a test can read what they serve - alongside both output
// streams as lines.
func captureStart(t *testing.T, cfg *config.Config) (*Servers, startupCapture) {
	t.Helper()
	var logbuf, instrbuf syncBuffer
	logger := logging.New(&logbuf, logging.LevelDebug)
	s, err := Start(cfg, logger, WithRSHHighPorts(), WithInstructions(&instrbuf))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	t.Logf("server log:\n%s", logbuf.String())
	t.Logf("operator instructions:\n%s", instrbuf.String())
	return s, startupCapture{
		log:          splitNonEmpty(logbuf.String()),
		instructions: splitNonEmpty(instrbuf.String()),
	}
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

func hasLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func hasSubstring(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

// served returns what the running server serves at a tree path.
func served(t *testing.T, s *Servers, name string) string {
	t.Helper()
	f, err := s.tree.Open(name)
	if err != nil {
		t.Fatalf("open %q: %v", name, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %q: %v", name, err)
	}
	return string(b)
}

// fourSetConfig is the served profile in miniature: a primary set from
// one image, then three sets merging only their dist, exactly the
// four-set shape the generated commands open. The primary layer boots
// only when its media carry a stand - a boot layer whose source has none
// fails the build, which is the config mistake, not the scenario this
// wants.
func fourSetConfig(t *testing.T, withStand bool) *config.Config {
	t.Helper()
	dir := t.TempDir()
	boot := ""
	if withStand {
		boot = ", boot: true"
	}
	yaml := fmt.Sprintf(`
server_ip: 192.0.2.10
clients:
  - {name: octane, mac: "08:00:69:0e:af:12", ip: 127.0.0.1}
install_sets:
  - name: "6.5.30"
    layers:
      - {name: base, image: %q%s}
  - name: foundations
    layers:
      - {name: foundations1, image: %q}
  - name: applications
    layers:
      - {name: apps, image: %q}
  - name: development
    layers:
      - {name: dev, image: %q}
`,
		primaryImage(t, dir, "base.image", withStand), boot,
		distImage(t, dir, "foundations.image", "eoe.sw"),
		distImage(t, dir, "apps.image", "apps.sw"),
		distImage(t, dir, "dev.image", "dev.sw"))
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Ports = config.Ports{}
	return cfg
}

// distPaths is the served dist path of every set the instructions open,
// in config order - what instscript is called with.
var fourSetDists = []string{"/6.5.30/dist", "/foundations/dist", "/applications/dist", "/development/dist"}

// install.cmds owns distribution opening and selection, so startup gives
// the operator one admin-source command instead of duplicating its lines.
func TestStartupInstructionsUseAdminSource(t *testing.T) {
	_, c := captureStart(t, fourSetConfig(t, true))

	for _, want := range []string{
		"  PROM: setenv netaddr 127.0.0.1",
		"  PROM: boot -f bootp():/6.5.30/stand/fx.64",
		"  PROM: Remote Directory: /6.5.30/dist",
		"  Inst>: admin source 192.0.2.10:/install.cmds",
	} {
		if !hasLine(c.instructions, want) {
			t.Errorf("operator instructions missing %q, got:\n%s", want, strings.Join(c.instructions, "\n"))
		}
	}
	if hasSubstring(c.instructions, "  Inst>: open ") {
		t.Errorf("operator instructions duplicate install.cmds:\n%s", strings.Join(c.instructions, "\n"))
	}

	// these are operator console UX, not server log content: they must
	// never show up in the leveled log itself.
	for _, l := range c.log {
		if strings.Contains(l, "PROM:") || strings.Contains(l, "Inst>:") {
			t.Errorf("operator instruction line leaked into the server log: %q", l)
		}
	}
}

// A trailing slash makes the PROM request dist//sa(sash64), which it
// rejects as an invalid argument.
func TestStartupRemoteDirectoryOmitsTrailingSlash(t *testing.T) {
	_, c := captureStart(t, fourSetConfig(t, true))
	for _, l := range c.instructions {
		if !strings.Contains(l, "Remote Directory") {
			continue
		}
		if strings.HasSuffix(l, "/") {
			t.Errorf("Remote Directory line has a trailing slash: %q", l)
		}
		return
	}
	t.Errorf("no Remote Directory line, got:\n%s", strings.Join(c.instructions, "\n"))
}

// A set whose media carries no miniroot can still open its additional
// distributions after the operator boots fx from a CD in the drive, so
// the Inst> lines must survive rather than be suppressed along with a
// boot artifact this server cannot actually serve. The Remote Directory
// go with the boot line: the PROM only asks for it during the netboot
// this server just declined to offer, and startup drops it in exactly the
// same case.
func TestStartupOmitsPROMBootLineWithoutFx(t *testing.T) {
	_, c := captureStart(t, fourSetConfig(t, false))
	for _, l := range c.instructions {
		if strings.Contains(l, "boot -f bootp()") {
			t.Errorf("boot line printed for a set with no stand/fx.64: %q", l)
		}
		if strings.Contains(l, "Remote Directory") {
			t.Errorf("Remote Directory printed with no boot line to go with it: %q", l)
		}
	}
	if !hasLine(c.instructions, "  Inst>: admin source 192.0.2.10:/install.cmds") {
		t.Errorf("Inst> lines dropped along with the PROM boot line, got:\n%s", strings.Join(c.instructions, "\n"))
	}
}

// The startup report is what an operator checks when a set serves
// something unexpected: which layers went into it, in what order, which
// collisions were settled by configuration, and where a representative
// file's bytes actually came from.
func TestStartupLogsInstallSetInventory(t *testing.T) {
	dir := t.TempDir()
	base := primaryImage(t, dir, "base.image", true)
	// two layers writing the same path with different bytes: a collision
	// the config settles by naming the winning layer.
	one := readmeImage(t, dir, "apps1.image", "one\n")
	two := readmeImage(t, dir, "apps2.image", "two\n")
	yaml := fmt.Sprintf(`
server_ip: 192.0.2.10
clients:
  - {name: octane, mac: "08:00:69:0e:af:12", ip: 127.0.0.1}
install_sets:
  - name: "6.5.30"
    layers:
      - {name: base, image: %q, boot: true}
  - name: applications
    layers:
      - {name: apps1, image: %q}
      - {name: apps2, image: %q}
    collisions: {"applications/dist/inst.README": apps2}
`, base, one, two)
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Ports = config.Ports{}

	_, c := captureStart(t, cfg)
	for _, want := range []string{
		"set 6.5.30: enabled, 1 layer",
		fmt.Sprintf("set 6.5.30: layer base  <-  image %q  dist dist, boot", base),
		"set applications: enabled, 2 layers",
		fmt.Sprintf("set applications: layer apps2  <-  image %q  dist dist", two),
		"set applications: collision applications/dist/inst.README  <-  layer apps2",
		"set 6.5.30: /6.5.30/stand/fx.64  <-  image base:/stand/fx.64",
		"set 6.5.30: /6.5.30/dist/sa  <-  image base:/dist/sa",
	} {
		if !hasSubstring(c.log, want) {
			t.Errorf("server log missing %q, got:\n%s", want, strings.Join(c.log, "\n"))
		}
	}
	// the inventory is server log content, not console UX
	if hasSubstring(c.instructions, "layer base") {
		t.Errorf("inventory leaked into the operator instructions:\n%s", strings.Join(c.instructions, "\n"))
	}
}

// readmeImage writes a dist-only layer carrying one inst.README, so two
// of them collide on the same logical path with different bytes.
func readmeImage(t *testing.T, dir, name, readme string) string {
	t.Helper()
	img := efstest.New()
	entries := map[string]uint32{"inst.README": img.AddFile(0o644, []byte(readme))}
	img.SetRoot(map[string]uint32{"dist": img.AddDir(entries)})
	return writeImage(t, dir, name, img)
}

// install.cmds contains the generated command sequence at the stable path
// the operator loads with "admin source".
func TestGeneratedAdminCommandsMatchScript(t *testing.T) {
	s, _ := captureStart(t, fourSetConfig(t, true))
	want := instscript.Commands(instscript.Params{
		ServerIP: "192.0.2.10",
		Sets:     fourSetDists,
	})
	admin := served(t, s, "install.cmds")
	if admin != want {
		t.Errorf("install.cmds =\n%q\nwant\n%q", admin, want)
	}
}

// The primary layer's media ships its own .related_dists naming the
// discs of a CD set; ours has to win, or the Inst "Open Dist" menu
// offers paths this server does not serve.
func TestGeneratedRelatedDistsShadowsTheMediaCopy(t *testing.T) {
	s, _ := captureStart(t, fourSetConfig(t, true))
	want := instscript.RelatedDists(fourSetDists)
	if got := served(t, s, "6.5.30/dist/.related_dists"); got != want {
		t.Errorf(".related_dists = %q, want %q", got, want)
	}
}

// A disabled set stays browsable - that is the point of building every
// configured set - but nothing offers it: not the generated commands,
// not the Inst> lines.
func TestDisabledSetIsServedButNotOffered(t *testing.T) {
	cfg := fourSetConfig(t, true)
	cfg.InstallSets[2].Enabled = false // applications
	s, c := captureStart(t, cfg)

	if got := served(t, s, "applications/dist/apps.sw"); got != "apps.sw" {
		t.Errorf("disabled set is not served: %q", got)
	}
	for _, l := range c.instructions {
		if strings.Contains(l, "/applications/dist") {
			t.Errorf("disabled set offered to the operator: %q", l)
		}
	}
	if strings.Contains(served(t, s, "install.cmds"), "/applications/dist") {
		t.Error("disabled set opened by the generated commands")
	}
}

// With nothing enabled there is no first set to build a command file
// around: generation is skipped entirely rather than reaching for a set
// that isn't there, and the operator gets no command pointing at one.
func TestNoEnabledSetsGeneratesNothing(t *testing.T) {
	cfg := fourSetConfig(t, true)
	for i := range cfg.InstallSets {
		cfg.InstallSets[i].Enabled = false
	}
	s, c := captureStart(t, cfg)

	for _, name := range []string{"install.cmds"} {
		if _, err := s.tree.Open(name); err == nil {
			t.Errorf("%s generated with no enabled set", name)
		}
	}
	for _, l := range c.instructions {
		if strings.Contains(l, "Inst>:") || strings.Contains(l, "boot -f") {
			t.Errorf("operator instruction with no enabled set: %q", l)
		}
	}
	// every configured set is still built and browsable
	if got := served(t, s, "applications/dist/apps.sw"); got != "apps.sw" {
		t.Errorf("disabled set is not served: %q", got)
	}
}
