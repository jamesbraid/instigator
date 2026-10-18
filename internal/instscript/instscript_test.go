package instscript

import (
	"strings"
	"testing"
)

func testParams() Params {
	return Params{
		ServerIP: "192.0.2.10",
		Sets: []string{
			"/6.5.30/dist",
			"/foundations/dist",
			"/applications/dist",
			"/development/dist",
		},
	}
}

// inOrder asserts each of subs appears in s, and that each one's match
// starts no earlier than the previous one's — i.e. they appear in the
// given order (they need not be adjacent).
func inOrder(t *testing.T, s string, subs ...string) {
	t.Helper()
	pos := 0
	for _, sub := range subs {
		i := strings.Index(s[pos:], sub)
		if i < 0 {
			t.Fatalf("expected %q to appear after position %d (following %v), in:\n%s", sub, pos, subs, s)
		}
		pos += i + len(sub)
	}
}

// TestCommandsExactFourSetSequence pins Commands' output byte-for-byte
// for a four-set profile; this is served verbatim to admin source.
func TestCommandsExactFourSetSequence(t *testing.T) {
	p := testParams()
	got := Commands(p)

	want := "" +
		"open 192.0.2.10:/foundations/dist\n" +
		"open 192.0.2.10:/applications/dist\n" +
		"open 192.0.2.10:/development/dist\n" +
		"open 192.0.2.10:/6.5.30/dist\n" +
		"keep *\n" +
		"install standard\n" +
		"keep java_dev.sw.base\n" +
		"go\n"

	if got != want {
		t.Errorf("Commands() = %q, want %q", got, want)
	}
}

// TestCommandsSingleSet checks the degenerate one-set case: the primary
// distribution is reopened to make its metadata authoritative, then the
// fixed selection block runs.
func TestCommandsSingleSet(t *testing.T) {
	p := testParams()
	p.Sets = []string{"/6.5.30/dist"}
	got := Commands(p)

	want := "" +
		"open 192.0.2.10:/6.5.30/dist\n" +
		"keep *\n" +
		"install standard\n" +
		"keep java_dev.sw.base\n" +
		"go\n"

	if got != want {
		t.Errorf("Commands() = %q, want %q", got, want)
	}
}

func TestCommandsReopensPrimaryAfterAdditionalSets(t *testing.T) {
	got := Commands(testParams())
	primary := "open 192.0.2.10:/6.5.30/dist\n"
	install := "keep *\ninstall standard\nkeep java_dev.sw.base\ngo\n"
	if strings.Index(got, primary) < 0 {
		t.Fatalf("Commands omitted primary reopen:\n%s", got)
	}
	if strings.Index(got, primary) > strings.Index(got, install) {
		t.Fatalf("Commands must reopen primary before selection:\n%s", got)
	}
}

func TestCommandsPreservesProvenBaseProfileOrder(t *testing.T) {
	got := Commands(Params{
		ServerIP: "192.0.2.10",
		Sets: []string{
			"/6.5.30/dist",
			"/foundations/dist",
			"/development/dist",
			"/applications/dist",
			"/complementary/dist",
			"/freeware/dist",
		},
	})
	inOrder(t, got,
		"open 192.0.2.10:/foundations/dist",
		"open 192.0.2.10:/development/dist",
		"open 192.0.2.10:/applications/dist",
		"open 192.0.2.10:/complementary/dist",
		"open 192.0.2.10:/freeware/dist",
		"open 192.0.2.10:/6.5.30/dist",
		"keep *",
		"install standard",
		"keep java_dev.sw.base",
		"go",
	)
}

func TestCommandsKeepsInstalledSoftwareBeforeStandard(t *testing.T) {
	got := Commands(Params{
		ServerIP: "192.0.2.10",
		Sets:     []string{"/6.5.30/dist", "/foundations/dist"},
	})
	if !strings.Contains(got, "keep *\n") {
		t.Fatalf("Commands omitted the standard keep * selection:\n%s", got)
	}
	if !strings.Contains(got, "install standard\n") {
		t.Fatalf("Commands omitted install standard:\n%s", got)
	}
}

// TestCommandsNeverEmitsRetiredDirectives guards the baseline (a zero
// Selection) against the old release-stream vocabulary: no install
// feature/maint/prereqs, positional conflicts, or quit. A configured script
// may re-enable install maint/remove by intent; this pins that the default
// install.cmds does not.
func TestCommandsNeverEmitsRetiredDirectives(t *testing.T) {
	p := testParams()
	got := Commands(p)

	for _, retired := range []string{
		"from ",
		"return",
		"remove ",
		"keep incompleteoverlays",
		"install feature",
		"install maint",
		"install prereqs",
		"\nconflicts\n",
		"\nquit\n",
		"\ndone\n",
	} {
		if strings.Contains(got, retired) {
			t.Errorf("Commands output must not contain retired directive %q, got:\n%s", retired, got)
		}
	}
}

// TestCommandsInstallSelectionAppendsAfterBaseline checks a configured
// selection adds its install lines after the proven baseline and before go.
func TestCommandsInstallSelectionAppendsAfterBaseline(t *testing.T) {
	p := testParams()
	p.Selection = Selection{Install: []string{"dev.sw.dbx"}}
	got := Commands(p)
	if !strings.Contains(got, "install standard\nkeep java_dev.sw.base\ninstall dev.sw.dbx\ngo\n") {
		t.Fatalf("install selection not appended after baseline:\n%s", got)
	}
}

// TestCommandsMaintenanceStreamSwitchesFirst checks the maintenance stream
// emits "install maint" before the standard selection, so
// the standard set resolves to maintenance versions.
func TestCommandsMaintenanceStreamSwitchesFirst(t *testing.T) {
	p := testParams()
	p.Selection = Selection{Stream: "maintenance"}
	got := Commands(p)
	if !strings.Contains(got, "install maint\nkeep *\n") {
		t.Fatalf("maintenance stream not switched before selection:\n%s", got)
	}
}

// TestCommandsFeatureStreamEmitsNoDirective checks the default feature stream
// adds nothing, keeping the baseline output.
func TestCommandsFeatureStreamEmitsNoDirective(t *testing.T) {
	p := testParams()
	p.Selection = Selection{Stream: "feature"}
	if got := Commands(p); strings.Contains(got, "install maint") || strings.Contains(got, "install feature") {
		t.Fatalf("feature stream must emit no stream directive:\n%s", got)
	}
}

// TestCommandsKeepAndRemoveSelections checks extra keep and remove entries are
// emitted in install/keep/remove order before go.
func TestCommandsKeepAndRemoveSelections(t *testing.T) {
	p := testParams()
	p.Selection = Selection{Install: []string{"a"}, Keep: []string{"b"}, Remove: []string{"c"}}
	got := Commands(p)
	inOrder(t, got, "install a\n", "keep b\n", "remove c\n", "go\n")
}

// TestCommandsSelectionOneCommandPerLine keeps the one-bare-command-per-line
// invariant with a configured selection.
func TestCommandsSelectionOneCommandPerLine(t *testing.T) {
	p := testParams()
	p.Selection = Selection{Stream: "maintenance", Install: []string{"dev.sw.dbx"}, Remove: []string{"old.sw.thing"}}
	got := Commands(p)
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			t.Fatalf("selection produced a blank line:\n%s", got)
		}
	}
}

func TestCommandsOneCommandPerLine(t *testing.T) {
	p := testParams()
	got := Commands(p)

	if !strings.HasSuffix(got, "\n") {
		t.Errorf("Commands output must end with a trailing newline, got:\n%s", got)
	}

	trimmed := strings.TrimRight(got, "\n")
	for _, line := range strings.Split(trimmed, "\n") {
		if strings.TrimSpace(line) == "" {
			t.Error("Commands output has a blank line; expected one bare command per line")
		}
	}
}

func TestCommandsIsDeterministic(t *testing.T) {
	p := testParams()
	a := Commands(p)
	b := Commands(p)
	if a != b {
		t.Error("Commands is not deterministic for identical Params")
	}
}

// TestRelatedDistsUsesStockCDMarker prevents inst from opening an interactive
// related-distribution prompt while admin source reopens the primary overlay.
func TestRelatedDistsUsesStockCDMarker(t *testing.T) {
	sets := []string{
		"/6.5.30/dist",
		"/foundations/dist",
		"/applications/dist",
		"/development/dist",
	}
	got := RelatedDists(sets)

	want := "CD\n"

	if got != want {
		t.Errorf("RelatedDists(%v) = %q, want %q", sets, got, want)
	}
}

// TestRelatedDistsSingleSet still emits the stock marker for a primary set.
func TestRelatedDistsSingleSet(t *testing.T) {
	got := RelatedDists([]string{"/6.5.30/dist"})
	if got != "CD\n" {
		t.Errorf("RelatedDists with a single set = %q, want CD marker", got)
	}
}

func TestRelatedDistsIsDeterministic(t *testing.T) {
	sets := []string{"/6.5.30/dist", "/foundations/dist"}
	a := RelatedDists(sets)
	b := RelatedDists(sets)
	if a != b {
		t.Error("RelatedDists is not deterministic for identical input")
	}
}

// TestThenCommandsExactSequence pins the second pass byte for byte: open
// the add-on sets, keep what the first pass installed, select only the
// add-on products, then hand off to the timed go.
func TestThenCommandsExactSequence(t *testing.T) {
	p := ThenParams{
		ServerIP:  "192.0.2.10",
		Sets:      []string{"/addon-rad4/dist"},
		Install:   []string{"rad4x.sw.driver", "rad4x.man"},
		StartPath: "/__instigator/addon-rad4/then/start.cmds",
	}
	want := "" +
		"open 192.0.2.10:/addon-rad4/dist\n" +
		"keep *\n" +
		"install rad4x.sw.driver\n" +
		"install rad4x.man\n" +
		"admin source 192.0.2.10:/__instigator/addon-rad4/then/start.cmds\n"
	if got := ThenCommands(p); got != want {
		t.Errorf("ThenCommands() = %q, want %q", got, want)
	}
	p.StartPath = ""
	if got := ThenCommands(p); !strings.HasSuffix(got, "install rad4x.man\ngo\n") {
		t.Errorf("standalone second pass does not end with go: %q", got)
	}
}

// TestPostInstallCommandsExactSequence pins the post-install file: select
// a POSIX shell, run each command in the target, restore inst's default
// shell, then print the marker only reached when every command succeeded.
func TestPostInstallCommandsExactSequence(t *testing.T) {
	want := "" +
		"set shell /bin/sh\n" +
		"shroot chkconfig -f autoconfig_ipaddress on\n" +
		"shroot touch /etc/marker\n" +
		"set shell /bin/csh\n" +
		"sh echo instigator: post-install commands finished\n"
	got := PostInstallCommands([]string{"chkconfig -f autoconfig_ipaddress on", "touch /etc/marker"})
	if got != want {
		t.Errorf("PostInstallCommands() = %q, want %q", got, want)
	}
}
