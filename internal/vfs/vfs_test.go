package vfs

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/jamesbraid/instigator/efs/efstest"
)

func writeImage(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// read returns a tree File's full contents.
func read(t *testing.T, f File) string {
	t.Helper()
	b := make([]byte, f.Size())
	if _, err := f.ReadAt(b, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	return string(b)
}

func TestOpenImage(t *testing.T) {
	img := efstest.New()
	ino := img.AddFile(0o644, []byte("fx dummy"))
	img.SetRoot(map[string]uint32{"fx.64": ino})
	path := writeImage(t, "disc1.iso", img.CDImage(64, nil))

	d, err := OpenImage(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	node, err := d.FS().Lookup("/fx.64")
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, node.Size)
	if _, err := d.FS().ReadAt(node, p, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if string(p) != "fx dummy" {
		t.Fatalf("content = %q", p)
	}
}

func TestOpenImageRejectsJunk(t *testing.T) {
	path := writeImage(t, "junk.iso", make([]byte, 4096))
	if _, err := OpenImage(path); err == nil {
		t.Fatal("OpenImage accepted junk")
	}
}
