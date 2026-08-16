package instscript

import (
	"strings"
	"testing"
)

func testParams() Params {
	return Params{
		ServerIP: "192.0.2.10",
		// the primary disc's own dist, the path AddCombined returns and
		// serve passes in - not a union across the set
		DistPath: "/irix6.5.30/tools/dist",
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
		"install feature",
		"go",
		"conflicts",
		"go",
		"quit",
	)
}

// The stream choice is scriptable: "install maint"/"install feature" sets
// it without waiting for Inst's interactive first-install prompt (SGI
// IRIX Admin: Software Installation and Licensing, 007-1364-140, ch.7
// "Maintenance Tips" > "Switching Streams" - "use the Inst commands
// install feature or install maintenance when the Inst prompt first
// appears"). The runbook must instruct that command, not "enter 1/2" at
// a menu it never has to wait for.
func TestGenerateMaintenanceStream(t *testing.T) {
	p := testParams()
	p.Stream = "maintenance"
	got := Generate(p)

	if !strings.Contains(got, "maintenance") {
		t.Errorf("Generate with Stream=maintenance should mention \"maintenance\", got:\n%s", got)
	}
	// The interactive menu is still shown for orientation (what the
	// command answers), the maintenance entry named correctly, but the
	// instructed action is the scripted command.
	inOrder(t, got, "1. Place me on the maintenance stream", "install maint")
}

func TestGenerateFeatureStream(t *testing.T) {
	p := testParams()
	p.Stream = "feature"
	got := Generate(p)

	inOrder(t, got, "2. Place me on the feature stream", "install feature")
}

// One "from" opens the whole set: instigator serves each disc whole and
// synthesizes a .related_dists on the primary that names the rest, which
// inst opens itself. The runbook has to say so. An operator who sees
// eleven discs served under one name and no explanation reaches for a
// per-disc "open" for each of the other ten - the very thing the
// synthesized .related_dists exists to avoid.
func TestGenerateSaysOneFromOpensTheWholeSet(t *testing.T) {
	p := testParams()
	got := Generate(p)

	inOrder(t, got, "from "+p.ServerIP+":"+p.DistPath, ".related_dists")

	if n := strings.Count(got, "from "+p.ServerIP+":"); n != 1 {
		t.Errorf("runbook has %d \"from\" commands, want exactly 1 for a combined set, got:\n%s", n, got)
	}
	if strings.Contains(got, "open "+p.ServerIP+":") {
		t.Errorf("runbook tells the operator to \"open\" a disc by hand; inst opens the rest itself, got:\n%s", got)
	}
}

// The runbook's PROM line and the one serve prints at startup are the
// same command derived from the same path; if they ever disagree the
// operator has two answers and no way to pick.
func TestGeneratePROMBootLineMatchesStartupLog(t *testing.T) {
	p := testParams()
	const want = "boot -f bootp():/irix6.5.30/tools/stand/fx.64"
	if got := promBootLine(p.DistPath); got != want {
		t.Errorf("promBootLine(%q) = %q, want %q", p.DistPath, got, want)
	}
	if got := Generate(p); !strings.Contains(got, want) {
		t.Errorf("runbook missing the primary disc's PROM boot line, got:\n%s", got)
	}
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

	// admin source replays these lines in the plain Inst> vocabulary
	// (from/install), not the -F selections-file directive grammar
	// (don't install/don't remove) — see the package doc comment,
	// confirmed against the field-tested walkthrough. "install feature"
	// both sets the stream and does the "keep * ; install standard"
	// product selection in one command (007-1364-140 ch.7).
	inOrder(t, got,
		"from "+p.ServerIP+":"+p.DistPath,
		"install feature",
		"install prereqs",
		"keep incompleteoverlays",
	)
}

// Commands must NOT emit go/quit/conflicts: neither the "admin source"
// vocabulary nor inst's -F selections-file grammar has a way to trigger
// the install or resolve a conflict from a file, so a caller that
// included them would be promising non-interactive behavior inst does
// not have. The release-stream prompt used to be excluded here too,
// wrongly - see TestCommandsSetsStreamNonInteractively.
func TestCommandsOmitsInteractiveOnlySteps(t *testing.T) {
	p := testParams()
	got := Commands(p)

	for _, tok := range []string{"\ngo\n", "\nquit\n", "conflicts"} {
		if strings.Contains(got, tok) {
			t.Errorf("Commands output must not contain interactive-only step %q (inst's -F selections-file has no directive for it), got:\n%s", tok, got)
		}
	}
}

// TestCommandsSetsStreamNonInteractively is the fix: the release-stream
// prompt DOES have a scripted equivalent, "install maint"/"install
// feature" (007-1364-140 ch.7, "Switching Streams" - "use the Inst
// commands install feature or install maintenance when the Inst prompt
// first appears"), so Commands must include it rather than leave stream
// selection as a step only Generate's human runbook covers.
func TestCommandsSetsStreamNonInteractively(t *testing.T) {
	feature := testParams()
	feature.Stream = "feature"
	if got := Commands(feature); !strings.Contains(got, "install feature") {
		t.Errorf("Commands with Stream=feature should contain \"install feature\", got:\n%s", got)
	}

	maint := testParams()
	maint.Stream = "maintenance"
	got := Commands(maint)
	if !strings.Contains(got, "install maint") {
		t.Errorf("Commands with Stream=maintenance should contain \"install maint\", got:\n%s", got)
	}
	if strings.Contains(got, "install feature") {
		t.Errorf("Commands with Stream=maintenance should not also contain \"install feature\", got:\n%s", got)
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

func TestGenerateMentionsAdminSource(t *testing.T) {
	p := testParams()
	got := Generate(p)

	// admin source <host>:<path> is the confirmed, field-tested way to
	// load Commands()'s output into a running Inst session; the
	// runbook should point operators at it rather than leave them to
	// discover it separately.
	if !strings.Contains(got, "admin source") {
		t.Errorf("Generate output should mention the \"admin source\" file-loading mechanism, got:\n%s", got)
	}
}

// TestGenerateDoesNotClaimStreamIsUnscriptable is the other half of the
// fix: the runbook used to tell operators the release-stream prompt
// needed a human present. That was wrong - "install maint"/"install
// feature" answers it - and the wrong claim must not still be in the
// text next to the right command.
func TestGenerateDoesNotClaimStreamIsUnscriptable(t *testing.T) {
	p := testParams()
	got := Generate(p)

	if !strings.Contains(got, "install feature") {
		t.Errorf("Generate output should instruct \"install feature\" to set the stream, got:\n%s", got)
	}
	for _, wrong := range []string{
		"no way to answer the release-stream",
		"release-stream prompt or resolve",
	} {
		if strings.Contains(got, wrong) {
			t.Errorf("Generate output still contains the retired claim %q that the stream choice can't be scripted, got:\n%s", wrong, got)
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
