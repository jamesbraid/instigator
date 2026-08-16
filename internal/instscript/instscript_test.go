package instscript

import (
	"strings"
	"testing"
)

func testParams() Params {
	return Params{
		ServerIP: "192.0.2.10",
		DistPath: "/irix6.5.30/dist",
		Release:  "6.5.30",
		Stream:   "feature",
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

func TestGenerateContainsRealParamsInOrder(t *testing.T) {
	p := testParams()
	got := Generate(p)

	if got == "" {
		t.Fatal("Generate returned empty string")
	}

	// The server IP and dist path must appear literally (not as
	// placeholders), the release-stream choice must be named, and the
	// canonical inst Main Menu commands must appear in the documented
	// order: from, install, conflicts, go, quit.
	inOrder(t, got,
		p.Release,
		p.ServerIP,
		p.DistPath,
		"from "+p.ServerIP+":"+p.DistPath,
		"feature",
		"install standard",
		"go",
		"conflicts",
		"go",
		"quit",
	)
}

func TestGenerateMaintenanceStream(t *testing.T) {
	p := testParams()
	p.Stream = "maintenance"
	got := Generate(p)

	if !strings.Contains(got, "maintenance") {
		t.Errorf("Generate with Stream=maintenance should mention \"maintenance\", got:\n%s", got)
	}
	// The maintenance-stream menu entry ("1") must be the one called
	// out, not the feature entry.
	inOrder(t, got, "1. Place me on the maintenance stream", "Enter 1")
}

func TestGenerateFeatureStream(t *testing.T) {
	p := testParams()
	p.Stream = "feature"
	got := Generate(p)

	inOrder(t, got, "2. Place me on the feature stream", "Enter 2")
}

func TestGeneratePROMBootLine(t *testing.T) {
	p := testParams()
	got := Generate(p)

	// A PROM boot-monitor line must be present for reference, following
	// instigator's own established bootp() convention
	// (internal/serve/serve.go logStartup / README.md).
	if !strings.Contains(got, "boot -f bootp():") {
		t.Errorf("Generate output missing a PROM boot line, got:\n%s", got)
	}
	if !strings.Contains(got, "stand/fx.64") {
		t.Errorf("Generate output missing the fx.64 partitioner reference, got:\n%s", got)
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	p := testParams()
	a := Generate(p)
	b := Generate(p)
	if a != b {
		t.Error("Generate is not deterministic for identical Params")
	}
}

func TestGenerateDifferentParamsProduceDifferentText(t *testing.T) {
	p1 := testParams()
	p2 := testParams()
	p2.ServerIP = "192.0.2.99"
	p2.DistPath = "/irix6.5/dist"
	p2.Release = "6.5"
	p2.Stream = "maintenance"

	if Generate(p1) == Generate(p2) {
		t.Error("Generate should produce different output for different Params")
	}
}

func TestCommandsContainsSelectionDirectivesInOrder(t *testing.T) {
	p := testParams()
	got := Commands(p)

	if got == "" {
		t.Fatal("Commands returned empty string")
	}

	inOrder(t, got,
		"from "+p.ServerIP+":"+p.DistPath,
		"install standard",
		"install prereqs",
	)
}

// Commands feeds inst's -F selections-file, whose directive grammar
// (from / install / don't install / remove / don't remove / set) has no
// primitive for the release-stream prompt or for conflict resolution —
// see the package doc comment for the research this reflects. Commands
// must therefore NOT emit go/quit/conflicts: a caller that did would be
// promising non-interactive behavior inst does not have.
func TestCommandsOmitsInteractiveOnlySteps(t *testing.T) {
	p := testParams()
	got := Commands(p)

	for _, tok := range []string{"\ngo\n", "\nquit\n", "conflicts"} {
		if strings.Contains(got, tok) {
			t.Errorf("Commands output must not contain interactive-only step %q (inst's -F selections-file has no directive for it), got:\n%s", tok, got)
		}
	}
}

func TestCommandsOneCommandPerLine(t *testing.T) {
	p := testParams()
	got := Commands(p)

	got = strings.TrimRight(got, "\n")
	for _, line := range strings.Split(got, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
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
