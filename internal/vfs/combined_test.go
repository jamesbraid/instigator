package vfs

import (
	"os"
	"path/filepath"
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
	got, err := tree.AddCombined("full", []string{found, primary, nfs})
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
	if _, err := tree.AddCombined("full", []string{found, primary, nfs}); err != nil {
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
	tree.AddCombined("full", []string{found, primary, nfs})

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

func TestCombinedRequiresExactlyOnePrimary(t *testing.T) {
	dir := t.TempDir()
	a := combImage(t, dir, "a.iso", foundationDist) // .iscd only, no primary marker
	b := combImage(t, dir, "b.iso", redirectDist)

	tree, _ := BuildTree(nil)
	defer tree.Close()
	if _, err := tree.AddCombined("x", []string{a, b}); err == nil {
		t.Fatal("combined with no .related_dists disc should fail, got nil")
	}

	// two primaries is equally ambiguous
	p1 := combImage(t, dir, "p1.image", primaryDist)
	p2 := combImage(t, dir, "p2.image", primaryDist)
	if _, err := tree.AddCombined("y", []string{p1, p2}); err == nil {
		t.Fatal("combined with two .related_dists discs should fail, got nil")
	}
}
