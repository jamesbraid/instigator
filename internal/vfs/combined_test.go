package vfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamesbraid/instigator/efs/efstest"
)

// combImage writes a CD image whose /dist is built by dist, which returns
// the dist directory's entries (letting a test add subdirs like dist6.5).
func combImage(t *testing.T, dir, filename string, dist func(img *efstest.Builder) map[string]uint32) string {
	t.Helper()
	img := efstest.New()
	d := img.AddDir(dist(img))
	img.SetRoot(map[string]uint32{"dist": d})
	p := filepath.Join(dir, filename)
	if err := os.WriteFile(p, img.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// primaryDist is the Installation-Tools/Overlays1 shape: a real
// .related_dists marker (stock content "CD\n"), the miniroot sa, a
// product, no .redirect.
func primaryDist(img *efstest.Builder) map[string]uint32 {
	return map[string]uint32{
		"sa":             img.AddFile(0o644, []byte("SA")),
		"prod.sw":        img.AddFile(0o644, []byte("P")),
		".related_dists": img.AddFile(0o555, []byte("CD\n")),
		".prereq_hints":  img.AddFile(0o555, []byte("H")),
	}
}

// foundationDist is a plain base disc: a product plus the .iscd sentinel,
// no .redirect and no .related_dists.
func foundationDist(img *efstest.Builder) map[string]uint32 {
	return map[string]uint32{
		"foundation.sw": img.AddFile(0o644, []byte("F")),
		".iscd":         img.AddFile(0o644, []byte{}),
	}
}

// redirectDist is a version-stub disc (devfoundation/nfs shape): a
// .redirect in dist/, the real catalog under dist/dist6.5.
func redirectDist(img *efstest.Builder) map[string]uint32 {
	d65 := img.AddDir(map[string]uint32{"nfs.sw": img.AddFile(0o444, []byte("NFS"))})
	return map[string]uint32{
		".redirect": img.AddFile(0o644, []byte("dist6.5 (TARGOS)\n")),
		".iscd":     img.AddFile(0o644, []byte{}),
		"dist6.5":   d65,
	}
}

func contains(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func TestCombinedSynthesizesRelatedDists(t *testing.T) {
	dir := t.TempDir()
	found := combImage(t, dir, "foundation1.iso", foundationDist)
	primary := combImage(t, dir, "tools.image", primaryDist)
	nfs := combImage(t, dir, "nfs.iso", redirectDist)

	tree, err := BuildTree(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()

	// config order is [foundation1, tools, nfs]; tools is auto-detected
	// primary because it carries .related_dists.
	got, err := tree.AddCombined("full", []string{found, primary, nfs}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/full/tools/dist" {
		t.Fatalf("primary dist path = %q, want /full/tools/dist", got)
	}

	// the synthesized .related_dists names the OTHER two discs in config
	// order, relative to the primary disc's own directory; the redirect
	// disc is pointed PAST its stub at dist6.5.
	f, err := tree.Open("full/tools/dist/.related_dists")
	if err != nil {
		t.Fatal(err)
	}
	want := "../foundation1/dist\n../nfs/dist/dist6.5\n"
	if c := read(t, f); c != want {
		t.Fatalf(".related_dists = %q, want %q", c, want)
	}
}

func TestCombinedServesEachDiscWhole(t *testing.T) {
	dir := t.TempDir()
	found := combImage(t, dir, "foundation1.iso", foundationDist)
	primary := combImage(t, dir, "tools.image", primaryDist)
	nfs := combImage(t, dir, "nfs.iso", redirectDist)

	tree, _ := BuildTree(nil)
	defer tree.Close()
	if _, err := tree.AddCombined("full", []string{found, primary, nfs}, nil); err != nil {
		t.Fatal(err)
	}

	cases := map[string]string{
		"full/tools/dist/prod.sw":             "P",
		"full/foundation1/dist/foundation.sw": "F",
		"full/nfs/dist/dist6.5/nfs.sw":        "NFS",
	}
	for path, wantContent := range cases {
		f, err := tree.Open(path)
		if err != nil {
			t.Fatalf("Open(%s): %v", path, err)
		}
		if c := read(t, f); c != wantContent {
			t.Fatalf("Open(%s) = %q, want %q", path, c, wantContent)
		}
	}

	// the redirect disc's own stub .redirect stays readable - we never
	// hide it, we just never point inst there.
	if _, err := tree.Open("full/nfs/dist/.redirect"); err != nil {
		t.Fatalf("nfs stub .redirect should be readable: %v", err)
	}
}

func TestCombinedListsAndStats(t *testing.T) {
	dir := t.TempDir()
	found := combImage(t, dir, "foundation1.iso", foundationDist)
	primary := combImage(t, dir, "tools.image", primaryDist)
	nfs := combImage(t, dir, "nfs.iso", redirectDist)

	tree, _ := BuildTree(nil)
	defer tree.Close()
	tree.AddCombined("full", []string{found, primary, nfs}, nil)

	// the combined root lists the disc slugs
	roots, err := tree.ReadDir("full")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"tools", "foundation1", "nfs"} {
		if !contains(roots, s) {
			t.Fatalf("combined root %v missing %q", roots, s)
		}
	}

	// the primary dist still lists .related_dists among its real entries
	names, err := tree.ReadDir("full/tools/dist")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(names, ".related_dists") || !contains(names, "sa") {
		t.Fatalf("primary dist listing = %v", names)
	}

	// stat of the synthesized .related_dists reports the generated size,
	// not the stock 3-byte "CD\n"
	info, err := tree.Stat("full/tools/dist/.related_dists")
	if err != nil {
		t.Fatal(err)
	}
	wantSize := int64(len("../foundation1/dist\n../nfs/dist/dist6.5\n"))
	if info.IsDir || info.Size != wantSize {
		t.Fatalf("stat .related_dists = %+v, want size %d file", info, wantSize)
	}
}

func TestCombinedDiscNamesOverrideSlugs(t *testing.T) {
	dir := t.TempDir()
	found := combImage(t, dir, "irix6.5_foundation1.iso", foundationDist)
	primary := combImage(t, dir, "Instalation_Tools.image", primaryDist)
	nfs := combImage(t, dir, "irix6.5_nfs.iso", redirectDist)

	tree, _ := BuildTree(nil)
	defer tree.Close()
	names := map[string]string{
		"irix6.5_foundation1.iso": "foundation1",
		"Instalation_Tools.image": "tools",
		"irix6.5_nfs.iso":         "nfs",
	}
	got, err := tree.AddCombined("full", []string{found, primary, nfs}, names)
	if err != nil {
		t.Fatal(err)
	}
	// the primary path and every mount use the override slug, not the
	// raw-filename slug (irix6-5-foundation1)
	if got != "/full/tools/dist" {
		t.Fatalf("primary dist = %q, want /full/tools/dist", got)
	}
	if _, err := tree.Open("full/foundation1/dist/foundation.sw"); err != nil {
		t.Fatalf("override-slug mount not reachable: %v", err)
	}
	// and the synthesized .related_dists names them by the override slug
	f, err := tree.Open("full/tools/dist/.related_dists")
	if err != nil {
		t.Fatal(err)
	}
	want := "../foundation1/dist\n../nfs/dist/dist6.5\n"
	if c := read(t, f); c != want {
		t.Fatalf(".related_dists = %q, want %q", c, want)
	}
}

// TestCombinedNeverLeaksRedirectIntoPrimary is a regression guard for the
// infinite-reopen bug this package's split from a flat union fixed: the
// old flat union merged every disc's dist/ into one shared directory,
// which pulled a redirect disc's dist/.redirect stub into the primary's
// dist alongside it. inst checks dist/.redirect first when it opens a
// distribution, so it followed that leaked redirect, failed to resolve
// it, and reopened the rsh session forever. This fails under either way
// that bug comes back: flattening leaking .redirect into the primary, or
// a redirect disc ever being referenced at its bare dist - which is only
// the stub - instead of past it at dist/dist6.5.
func TestCombinedNeverLeaksRedirectIntoPrimary(t *testing.T) {
	dir := t.TempDir()
	found := combImage(t, dir, "foundation1.iso", foundationDist)
	primary := combImage(t, dir, "tools.image", primaryDist)
	nfs := combImage(t, dir, "nfs.iso", redirectDist)

	tree, err := BuildTree(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if _, err := tree.AddCombined("full", []string{found, primary, nfs}, nil); err != nil {
		t.Fatal(err)
	}

	// (1) the path inst actually opens - the primary's own dist - must
	// never carry a .redirect, from a flattened merge or any other means.
	if _, err := tree.Open("full/tools/dist/.redirect"); err == nil {
		t.Fatal("full/tools/dist/.redirect opened: a redirect leaked into the path inst opens")
	}
	primaryNames, err := tree.ReadDir("full/tools/dist")
	if err != nil {
		t.Fatal(err)
	}
	if contains(primaryNames, ".redirect") {
		t.Fatalf("primary dist listing carries .redirect: %v", primaryNames)
	}

	// (2) the redirect disc is reachable past its stub, at dist/dist6.5.
	if _, err := tree.Open("full/nfs/dist/dist6.5/nfs.sw"); err != nil {
		t.Fatalf("redirect disc not reachable past its stub: %v", err)
	}

	// and the synthesized .related_dists - what inst actually follows to
	// find it - must reference the redirect disc past its stub, never at
	// its bare dist, which is only the .redirect file again.
	f, err := tree.Open("full/tools/dist/.related_dists")
	if err != nil {
		t.Fatal(err)
	}
	related := read(t, f)
	var nfsLine string
	for _, line := range strings.Split(strings.TrimRight(related, "\n"), "\n") {
		if strings.HasPrefix(line, "../nfs/") {
			nfsLine = line
		}
	}
	if nfsLine == "" {
		t.Fatalf(".related_dists has no entry for the redirect disc: %q", related)
	}
	if nfsLine == "../nfs/dist" {
		t.Fatal(".related_dists points the redirect disc at its bare dist - that's only the .redirect stub")
	}
	if !strings.HasSuffix(nfsLine, "/dist/dist6.5") {
		t.Fatalf(".related_dists points the redirect disc at %q, want it to end in /dist/dist6.5", nfsLine)
	}
}

func TestCombinedRequiresExactlyOnePrimary(t *testing.T) {
	dir := t.TempDir()
	a := combImage(t, dir, "a.iso", foundationDist) // .iscd only, no primary marker
	b := combImage(t, dir, "b.iso", redirectDist)

	tree, _ := BuildTree(nil)
	defer tree.Close()
	if _, err := tree.AddCombined("x", []string{a, b}, nil); err == nil {
		t.Fatal("combined with no .related_dists disc should fail, got nil")
	}

	// two primaries is equally ambiguous
	p1 := combImage(t, dir, "p1.image", primaryDist)
	p2 := combImage(t, dir, "p2.image", primaryDist)
	if _, err := tree.AddCombined("y", []string{p1, p2}, nil); err == nil {
		t.Fatal("combined with two .related_dists discs should fail, got nil")
	}
}
