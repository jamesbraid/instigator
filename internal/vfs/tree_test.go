package vfs

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/jamesbraid/instigator/efs/efstest"
)

// makeDisc writes one synthetic CD image holding stand/fx.64 with the
// given content.
func makeDisc(t *testing.T, dir, filename, content string) {
	t.Helper()
	img := efstest.New()
	fx := img.AddFile(0o755, []byte(content))
	stand := img.AddDir(map[string]uint32{"fx.64": fx})
	img.SetRoot(map[string]uint32{"stand": stand})
	if err := os.WriteFile(filepath.Join(dir, filename), img.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}
}

func buildTestTree(t *testing.T) *Tree {
	t.Helper()
	dir := t.TempDir()
	makeDisc(t, dir, "IRIX 6.5.30 Overlay 1of3.iso", "overlay one fx")
	makeDisc(t, dir, "IRIX 6.5.30 Overlay 2of3.iso", "overlay two fx")
	tree, err := BuildTree([]MediaSet{{Name: "6.5.30", Dir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tree.Close() })
	return tree
}

func TestTreeOpenAcrossDiscs(t *testing.T) {
	tree := buildTestTree(t)
	f, err := tree.Open("6.5.30/irix-6-5-30-overlay-1of3/stand/fx.64")
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, f.Size())
	if _, err := f.ReadAt(b, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if string(b) != "overlay one fx" {
		t.Fatalf("content = %q", b)
	}
}

func TestTreeReadDirLevels(t *testing.T) {
	tree := buildTestTree(t)
	medias, err := tree.ReadDir("")
	if err != nil {
		t.Fatal(err)
	}
	if len(medias) != 1 || medias[0] != "6.5.30" {
		t.Fatalf("media level = %v", medias)
	}
	discs, err := tree.ReadDir("6.5.30")
	if err != nil {
		t.Fatal(err)
	}
	if len(discs) != 2 {
		t.Fatalf("disc level = %v", discs)
	}
	files, err := tree.ReadDir("6.5.30/irix-6-5-30-overlay-2of3/stand")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "fx.64" {
		t.Fatalf("efs level = %v", files)
	}
}

func TestTreeDiscNameOverride(t *testing.T) {
	dir := t.TempDir()
	makeDisc(t, dir, "IRIX 6.5.30 Overlay 1of3.iso", "fx")
	tree, err := BuildTree([]MediaSet{{
		Name:      "6.5.30",
		Dir:       dir,
		DiscNames: map[string]string{"IRIX 6.5.30 Overlay 1of3.iso": "overlay1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if _, err := tree.Open("6.5.30/overlay1/stand/fx.64"); err != nil {
		t.Fatal(err)
	}
}

func TestTreeMissingPath(t *testing.T) {
	tree := buildTestTree(t)
	if _, err := tree.Open("6.5.30/no-such-disc/stand/fx.64"); err == nil {
		t.Fatal("opened a missing disc")
	}
	if _, err := tree.Open("6.5.30/irix-6-5-30-overlay-1of3/absent"); err == nil {
		t.Fatal("opened a missing file")
	}
}

func TestTreeDiscMap(t *testing.T) {
	tree := buildTestTree(t)
	m := tree.DiscMap()
	if len(m["6.5.30"]) != 2 {
		t.Fatalf("DiscMap = %v", m)
	}
	if m["6.5.30"]["irix-6-5-30-overlay-1of3"] == "" {
		t.Fatalf("DiscMap missing slug: %v", m)
	}
}
