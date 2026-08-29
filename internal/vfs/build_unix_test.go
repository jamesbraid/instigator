//go:build unix

package vfs

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestBuildRejectsSpecialFileLinksInDirectoryLayers is the
// directory-layer half: a link to an in-layer named pipe stops the build
// too. The pipe itself, reached directly rather than through a link, is
// left alone - real media carries the odd device node, and refusing to
// start over one is not this rule's job.
//
// This lives in a unix-tagged file because it creates a named pipe with
// syscall.Mkfifo, which does not exist on Windows.
func TestBuildRejectsSpecialFileLinksInDirectoryLayers(t *testing.T) {
	dir := t.TempDir()
	layer := makeDir(t, filepath.Join(dir, "found"), map[string]string{"dist/foundation.sw": "F"})
	if err := syscall.Mkfifo(filepath.Join(layer, "dist", "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pipe", filepath.Join(layer, "dist", "fifolink")); err != nil {
		t.Fatal(err)
	}

	_, err := Build([]SetSpec{{
		Name:   "foundations",
		Layers: []LayerSpec{{Name: "found", Source: layer}},
	}}, localResolver{})
	if err == nil {
		t.Fatal("Build accepted a layer whose symlink resolves to a named pipe")
	}
	for _, want := range []string{"found", "dist/fifolink"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}

	// Same as the image half: the pipe on its own, with the link removed,
	// is skipped and the layer builds.
	if err := os.Remove(filepath.Join(layer, "dist", "fifolink")); err != nil {
		t.Fatal(err)
	}
	tree := build(t, []SetSpec{{
		Name:   "foundations",
		Layers: []LayerSpec{{Name: "found", Source: layer}},
	}})
	if got := dirNames(t, tree, "foundations/dist"); !equalStrings(got, []string{"foundation.sw"}) {
		t.Fatalf("ReadDir = %v, want [foundation.sw]: a bare special file is skipped, not served", got)
	}
}

// TestBuildPreservesSpecialModeBitsFromDirectoryLayer is the
// directory-layer half of the special-bits rule: a file in an extracted
// layer that carries setuid, setgid, or sticky must serve with that bit
// intact, exactly as the image case does. os reports these as the
// symbolic fs.ModeSetuid, ModeSetgid, and ModeSticky bits, which
// Mode().Perm() silently drops. Unix-tagged because chmod on Windows
// carries none of these bits.
func TestBuildPreservesSpecialModeBitsFromDirectoryLayer(t *testing.T) {
	layer := t.TempDir()
	dist := filepath.Join(layer, "dist")
	if err := os.Mkdir(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{
		{"su", os.ModeSetuid | 0o711},
		{"sg", os.ModeSetgid | 0o755},
		{"tmp", os.ModeSticky | 0o644},
	} {
		p := filepath.Join(dist, tc.name)
		if err := os.WriteFile(p, []byte(tc.name), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, tc.mode); err != nil {
			t.Fatal(err)
		}
	}

	tree := build(t, []SetSpec{{
		Name:   "6.5.30",
		Layers: []LayerSpec{distLayer("extracted", layer)},
	}})

	for _, tc := range []struct {
		path string
		want uint16
	}{
		{"6.5.30/dist/su", 0o4711},
		{"6.5.30/dist/sg", 0o2755},
		{"6.5.30/dist/tmp", 0o1644},
	} {
		info, err := tree.Stat(tc.path)
		if err != nil {
			t.Fatalf("Stat(%s): %v", tc.path, err)
		}
		if got := uint16(info.Mode()) & 0o7777; got != tc.want {
			t.Errorf("Stat(%s).Mode()&0o7777 = %#o, want %#o", tc.path, got, tc.want)
		}
	}
}
