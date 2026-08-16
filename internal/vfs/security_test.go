package vfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jamesbraid/instigator/efs"
	"github.com/jamesbraid/instigator/efs/efstest"
)

// escapeDisc writes a synthetic CD image whose directory tree carries
// real "." and ".." entries at every level, exactly like genuine EFS
// media (Unix-derived, always self- and parent-linked). That matters:
// fs.Lookup (efs/inode.go) resolves ".." by literal directory-entry
// lookup, with no special-casing at all. If synthetic test images never
// had a working ".." entry, a path-traversal attempt would fail simply
// because the entry doesn't exist - true here, but not proof of
// anything, since a hostile client can't control what's in the served
// image either way. Building working "." / ".." entries makes this
// test prove the real invariant: even when ".." resolves for real, the
// walk can only ever reach other inodes in the *same* image, because
// Lookup never leaves the image's own inode graph to consult a host
// path. Root's own ".." points to itself, so any run of ".." bottoms
// out there and goes no further.
//
// Layout:
//
//	/               (root, ino 2)
//	/dist/          product.sw, sub/
//	/dist/product.sw
//	/dist/sub/      (one level deeper, for a multi-hop ".." case)
func escapeDisc(t *testing.T, dir, filename string) string {
	t.Helper()
	img := efstest.New()

	product := img.AddFile(0o644, []byte("product-content"))
	if product != 3 {
		t.Fatalf("efstest inode allocation changed (product=%d, want 3); update the hardcoded . / .. entries below", product)
	}
	// .related_dists makes this disc a valid combined primary (AddCombined
	// requires exactly one); its content is irrelevant to a traversal test.
	related := img.AddFile(0o555, []byte("CD\n"))
	if related != 4 {
		t.Fatalf("efstest inode allocation changed (related=%d, want 4); update the hardcoded . / .. entries below", related)
	}
	// A directory's own inode number has to be known before it's built,
	// to write its "." (and its children's "..") entries. efstest
	// allocates inode numbers sequentially as each Add* call is made, so
	// with the call order fixed here, sub's own inode (this call) and
	// dist's (the next AddDir call) are both predictable; the two
	// Fatalf checks below catch it if that ever stops being true.
	sub := img.AddDir(map[string]uint32{".": 5, "..": 6}) // sub=5 (self), dist=6 (next)
	if sub != 5 {
		t.Fatalf("efstest inode allocation changed (sub=%d, want 5); update the hardcoded . / .. entries below", sub)
	}
	dist := img.AddDir(map[string]uint32{
		".":              6,
		"..":             efs.RootIno,
		"product.sw":     product,
		"sub":            sub,
		".related_dists": related,
	})
	if dist != 6 {
		t.Fatalf("efstest inode allocation changed (dist=%d, want 6); update the hardcoded . / .. entries below", dist)
	}
	img.SetRoot(map[string]uint32{".": efs.RootIno, "..": efs.RootIno, "dist": dist})

	p := filepath.Join(dir, filename)
	if err := os.WriteFile(p, img.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// buildEscapeTree serves escapeDisc's image two ways at once: as an
// ordinary media/disc (media="media", disc="disc") and as a combined set
// (name="combined", disc slug "escape"), so both addressing paths through
// Tree can be exercised against the same synthetic content.
func buildEscapeTree(t *testing.T) *Tree {
	t.Helper()
	dir := t.TempDir()
	img := escapeDisc(t, dir, "escape.iso")

	tree, err := BuildTree([]MediaSet{{
		Name:      "media",
		Dir:       dir,
		DiscNames: map[string]string{"escape.iso": "disc"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tree.Close() })

	if _, err := tree.AddCombined("combined", []string{img}, nil); err != nil {
		t.Fatal(err)
	}
	return tree
}

// TestOpenCannotEscapeMediaOrCombined is the traversal-bounding
// regression test: every one of these paths tries to climb out of the
// served image to a host path that is never actually present anywhere in
// the tree (there is no "etc" or "shadow" entry in any synthetic image
// built by this file). Tree.Open must refuse every one - never fall
// through to a host filesystem read, and never resolve to any file at all.
func TestOpenCannotEscapeMediaOrCombined(t *testing.T) {
	tree := buildEscapeTree(t)

	cases := []struct {
		name string
		path string
	}{
		{"climb from disc level", "media/disc/../../../../etc/passwd"},
		{"climb from inside dist", "media/disc/dist/../../../../etc/passwd"},
		{"climb through a combined set", "combined/escape/dist/../../../../etc/passwd"},
		{"doubled leading slash", "//etc/passwd"},
		{"leading .. after slash-trim", "/../etc/shadow"},
		{"NUL byte in a component", "media/disc/dist/product.sw\x00/../../../../etc/passwd"},
		{"interleaved valid and escaping ..", "media/disc/dist/sub/../../dist/../../../../etc/passwd"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if f, err := tree.Open(c.path); err == nil {
				t.Fatalf("Open(%q) succeeded (size %d), want an error", c.path, f.Size())
			}
		})
	}
}

// TestReadDirCannotEscapeMediaOrCombined is ReadDir's half of the same
// invariant.
func TestReadDirCannotEscapeMediaOrCombined(t *testing.T) {
	tree := buildEscapeTree(t)

	cases := []struct {
		name string
		path string
	}{
		{"climb from disc level", "media/disc/../../../../etc"},
		{"climb through a combined set", "combined/escape/dist/../../../../etc"},
		{"doubled leading slash", "//etc"},
		{"leading .. after slash-trim", "/../etc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if names, err := tree.ReadDir(c.path); err == nil {
				t.Fatalf("ReadDir(%q) succeeded (%v), want an error", c.path, names)
			}
		})
	}
}

// TestBoundedPathsStillResolve proves the guard is a real boundary, not
// a blanket rejection of anything unusual: a plain in-image path, and a
// ".." that stays inside the image (sub/.. back to dist), both still
// resolve normally through media and combined addressing alike.
func TestBoundedPathsStillResolve(t *testing.T) {
	tree := buildEscapeTree(t)

	cases := []struct {
		name string
		path string
	}{
		{"plain media path", "media/disc/dist/product.sw"},
		{"plain combined path", "combined/escape/dist/product.sw"},
		{"in-bounds .. stays inside the image", "media/disc/dist/sub/../product.sw"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, err := tree.Open(c.path)
			if err != nil {
				t.Fatalf("Open(%q): %v", c.path, err)
			}
			if got := read(t, f); got != "product-content" {
				t.Fatalf("Open(%q) content = %q, want %q", c.path, got, "product-content")
			}
		})
	}

	names, err := tree.ReadDir("media/disc/dist")
	if err != nil {
		t.Fatalf("ReadDir(media/disc/dist): %v", err)
	}
	for _, want := range []string{"product.sw", "sub"} {
		if !containsString(names, want) {
			t.Fatalf("ReadDir(media/disc/dist) = %v, want it to contain %q", names, want)
		}
	}
}

func containsString(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}
