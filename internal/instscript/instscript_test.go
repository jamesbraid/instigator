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
		BootPath:  "/6.5.30/stand/fx.64",
		RemoteDir: "6.5.30/dist/",
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
// for a four-set profile: "from" on Sets[0], "open" on each later set in
// order, then the fixed selection block, one command per line, trailing
// newline. This is the single source served as both inst.init and the
// admin-source file, so any deviation here is a deviation an operator's
// serial console will see.
func TestCommandsExactFourSetSequence(t *testing.T) {
	p := testParams()
	got := Commands(p)

	want := "" +
		"from 192.0.2.10:/6.5.30/dist\n" +
		"open 192.0.2.10:/foundations/dist\n" +
		"open 192.0.2.10:/applications/dist\n" +
		"open 192.0.2.10:/development/dist\n" +
		"return\n" +
		"keep *\n" +
		"install standard\n" +
		"keep incompleteoverlays\n" +
		"remove java_dev*\n" +
		"remove java2_plugin*\n" +
		"conflicts\n"

	if got != want {
		t.Errorf("Commands() = %q, want %q", got, want)
	}
}

// TestCommandsSingleSet checks the degenerate one-set case: just "from",
// no "open" lines, then the same fixed selection block.
func TestCommandsSingleSet(t *testing.T) {
	p := testParams()
	p.Sets = []string{"/6.5.30/dist"}
	got := Commands(p)

	want := "" +
		"from 192.0.2.10:/6.5.30/dist\n" +
		"return\n" +
		"keep *\n" +
		"install standard\n" +
		"keep incompleteoverlays\n" +
		"remove java_dev*\n" +
		"remove java2_plugin*\n" +
		"conflicts\n"

	if got != want {
		t.Errorf("Commands() = %q, want %q", got, want)
	}
}

// TestCommandsNeverEmitsRetiredDirectives guards against regressing back
// to the old release-stream/full-automation vocabulary: no
// install feature/maint/prereqs, no go, no quit. These bytes are served
// verbatim to a real inst(1M) session; any of these would either fail to
// parse under the new contract or promise automation inst does not do
// from a file.
func TestCommandsNeverEmitsRetiredDirectives(t *testing.T) {
	p := testParams()
	got := Commands(p)

	for _, retired := range []string{
		"install feature",
		"install maint",
		"install prereqs",
		"\ngo\n",
		"\nquit\n",
	} {
		if strings.Contains(got, retired) {
			t.Errorf("Commands output must not contain retired directive %q, got:\n%s", retired, got)
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

// TestGenerateBootLineOnlyWhenBootPathSet is GC11: the PROM boot line
// prints only when BootPath is non-empty, and never a synthesized ".64"
// path.
func TestGenerateBootLineOnlyWhenBootPathSet(t *testing.T) {
	p := testParams()
	got := Generate(p)
	if !strings.Contains(got, "boot -f bootp():"+p.BootPath) {
		t.Errorf("Generate output missing PROM boot line for BootPath %q, got:\n%s", p.BootPath, got)
	}

	p.BootPath = ""
	got = Generate(p)
	if strings.Contains(got, "boot -f bootp()") {
		t.Errorf("Generate output must omit the PROM boot line when BootPath is empty, got:\n%s", got)
	}
	if strings.Contains(got, ".64") {
		t.Errorf("Generate output must not synthesize a .64 path when BootPath is empty, got:\n%s", got)
	}
	// The rest of the runbook (the Inst> section) must still be present.
	if !strings.Contains(got, "Inst>") {
		t.Errorf("Generate output must still contain the Inst> section when BootPath is empty, got:\n%s", got)
	}
	inOrder(t, got, "from "+p.ServerIP+":"+p.Sets[0], "open "+p.ServerIP+":"+p.Sets[1])
}

// TestGenerateShowsRemoteDirWithTrailingSlash is GC11's other half: the
// PROM "Remote Directory" instruction prints RemoteDir exactly as given,
// trailing slash included — that slash is required by the PROM prompt,
// not decorative.
func TestGenerateShowsRemoteDirWithTrailingSlash(t *testing.T) {
	p := testParams()
	got := Generate(p)
	if !strings.Contains(got, p.RemoteDir) {
		t.Errorf("Generate output missing Remote Directory value %q, got:\n%s", p.RemoteDir, got)
	}
	if !strings.HasSuffix(p.RemoteDir, "/") {
		t.Fatal("test fixture bug: RemoteDir must carry a trailing slash")
	}
}

// TestGenerateOpensAllSetsExplicitly is GC9/GC10 from the human-runbook
// side: Generate must walk the operator through "from" on the first set
// and an explicit "open" for every other set, in order, and must run the
// same fixed selection block Commands emits.
func TestGenerateOpensAllSetsExplicitly(t *testing.T) {
	p := testParams()
	got := Generate(p)

	inOrder(t, got,
		"from "+p.ServerIP+":"+p.Sets[0],
		"open "+p.ServerIP+":"+p.Sets[1],
		"open "+p.ServerIP+":"+p.Sets[2],
		"open "+p.ServerIP+":"+p.Sets[3],
		"return",
		"keep *",
		"install standard",
		"keep incompleteoverlays",
		"remove java_dev*",
		"remove java2_plugin*",
		"conflicts",
	)
}

// TestGenerateTellsOperatorToReviewConflictsThenGo is the human step
// Commands deliberately can't cover: reviewing "conflicts" output is
// interactive, so the runbook must instruct the operator to check it and
// then type "go" themselves.
func TestGenerateTellsOperatorToReviewConflictsThenGo(t *testing.T) {
	p := testParams()
	got := Generate(p)

	inOrder(t, got, "conflicts", "go")
	if !strings.Contains(got, "go") {
		t.Errorf("Generate output should instruct the operator to type \"go\" once conflicts are resolved, got:\n%s", got)
	}
}

// TestGenerateDoesNotClaimRelatedDistsOpensDistributions is GC10: no text
// anywhere in this package's output may claim .related_dists opens
// distributions. The runbook opens every set with an explicit command
// instead.
func TestGenerateDoesNotClaimRelatedDistsOpensDistributions(t *testing.T) {
	p := testParams()
	got := Generate(p)

	for _, wrong := range []string{
		"related_dists opens",
		"related_dists lists the others, and inst opens",
		"inst opens each of them",
		"inst opens the rest",
	} {
		if strings.Contains(got, wrong) {
			t.Errorf("Generate output must not claim .related_dists opens distributions (%q found), got:\n%s", wrong, got)
		}
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
	p2.Sets = []string{"/6.5/dist"}
	p2.BootPath = ""
	p2.RemoteDir = "6.5/dist/"

	if Generate(p1) == Generate(p2) {
		t.Error("Generate should produce different output for different Params")
	}
}

func TestGenerateMentionsAdminSource(t *testing.T) {
	p := testParams()
	got := Generate(p)

	// admin source <host>:<path> is the confirmed, field-tested way to
	// load Commands()'s output into a running Inst session; the runbook
	// should point operators at it rather than leave them to discover it
	// separately.
	if !strings.Contains(got, "admin source") {
		t.Errorf("Generate output should mention the \"admin source\" file-loading mechanism, got:\n%s", got)
	}
}

// TestRelatedDistsListsOtherSets: one "../../<setname>/dist" line per set
// OTHER than sets[0], in order, trailing newline.
func TestRelatedDistsListsOtherSets(t *testing.T) {
	sets := []string{
		"/6.5.30/dist",
		"/foundations/dist",
		"/applications/dist",
		"/development/dist",
	}
	got := RelatedDists(sets)

	want := "../../foundations/dist\n" +
		"../../applications/dist\n" +
		"../../development/dist\n"

	if got != want {
		t.Errorf("RelatedDists(%v) = %q, want %q", sets, got, want)
	}
}

// TestRelatedDistsSingleSet: with only the primary set, there is nothing
// else to list.
func TestRelatedDistsSingleSet(t *testing.T) {
	got := RelatedDists([]string{"/6.5.30/dist"})
	if got != "" {
		t.Errorf("RelatedDists with a single set = %q, want empty string", got)
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
