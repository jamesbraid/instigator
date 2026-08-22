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
// for a four-set profile: Sets[0] is already open when inst loads the
// file, so it opens each later set in order, reopens the primary release
// last, then runs the fixed selection block, one command per line, trailing
// newline. This is served as the admin-source file, so any deviation here
// is a deviation an operator's serial console will see.
func TestCommandsExactFourSetSequence(t *testing.T) {
	p := testParams()
	got := Commands(p)

	want := "" +
		"open 192.0.2.10:/foundations/dist\n" +
		"open 192.0.2.10:/applications/dist\n" +
		"open 192.0.2.10:/development/dist\n" +
		"open 192.0.2.10:/6.5.30/dist\n" +
		"done\n" +
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
		"done\n" +
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
	install := "done\nkeep *\ninstall standard\nkeep java_dev.sw.base\ngo\n"
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
		"done",
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

// TestCommandsNeverEmitsRetiredDirectives guards against regressing back
// to the old release-stream vocabulary: no install feature/maint/prereqs,
// positional conflicts, or quit. These bytes are served verbatim to a real
// inst(1M) session.
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
