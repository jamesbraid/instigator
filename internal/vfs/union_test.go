package vfs

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/jamesbraid/instigator/efs/efstest"
)

// distDisc writes a CD image whose /dist directory holds the named files,
// each with content "<discTag>:<name>" so we can tell which layer a read
// came from.
func distDisc(t *testing.T, dir, filename, discTag string, files []string) string {
	t.Helper()
	img := efstest.New()
	entries := map[string]uint32{}
	for _, name := range files {
		entries[name] = img.AddFile(0o644, []byte(discTag+":"+name))
	}
	dist := img.AddDir(entries)
	img.SetRoot(map[string]uint32{"dist": dist})
	p := filepath.Join(dir, filename)
	if err := os.WriteFile(p, img.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func read(t *testing.T, f File) string {
	t.Helper()
	b := make([]byte, f.Size())
	if _, err := f.ReadAt(b, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return string(b)
}

func TestUnionMergesDistWithPrecedence(t *testing.T) {
	dir := t.TempDir()
	base := distDisc(t, dir, "base.iso", "base", []string{"eoe.sw", "foundation.sw"})
	over := distDisc(t, dir, "over.iso", "over", []string{"eoe.sw", "overlay.sw"})

	tree, err := BuildTree(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	// layers low -> high: base first, overlay overrides
	if err := tree.AddUnion("full", []string{base, over}); err != nil {
		t.Fatal(err)
	}

	// ReadDir at the union root shows only "dist"
	top, err := tree.ReadDir("full")
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 || top[0] != "dist" {
		t.Fatalf("union root = %v, want [dist]", top)
	}

	// the merged dist lists every product once
	got, err := tree.ReadDir("full/dist")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"eoe.sw": true, "foundation.sw": true, "overlay.sw": true}
	if len(got) != 3 {
		t.Fatalf("merged dist = %v, want 3 entries", got)
	}
	for _, n := range got {
		if !want[n] {
			t.Fatalf("unexpected entry %q in %v", n, got)
		}
	}

	// overlay wins for the shared product; base-only and overlay-only resolve
	// to their own disc
	cases := map[string]string{
		"full/dist/eoe.sw":        "over:eoe.sw",
		"full/dist/foundation.sw": "base:foundation.sw",
		"full/dist/overlay.sw":    "over:overlay.sw",
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

	if _, err := tree.Open("full/dist/absent.sw"); err == nil {
		t.Fatal("opened a product no layer has")
	}
}
