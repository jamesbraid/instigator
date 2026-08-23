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
		Layers: []LayerSpec{{Name: "found", Dir: layer}},
	}})
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
		Layers: []LayerSpec{{Name: "found", Dir: layer}},
	}})
	if got := dirNames(t, tree, "foundations/dist"); !equalStrings(got, []string{"foundation.sw"}) {
		t.Fatalf("ReadDir = %v, want [foundation.sw]: a bare special file is skipped, not served", got)
	}
}
