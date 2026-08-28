package vfs

import (
	"bytes"
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

// TestOpenImageReaderMatchesOpenImage: an image opened from a bytes.Reader
// (no file) yields the same tree as one opened from a path, since a later
// task fetches an image over HTTP and reads it lazily by byte-range rather
// than from a file.
func TestOpenImageReaderMatchesOpenImage(t *testing.T) {
	dir := t.TempDir()
	path := makeImage(t, dir, "tools.image", map[string]string{"dist/sa": "SA"})
	want, err := OpenImage(path)
	if err != nil {
		t.Fatal(err)
	}
	defer want.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenImageReader(bytes.NewReader(raw), io.NopCloser(nil), "tools.image")
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()

	f, err := got.FSys().Open("dist/sa")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(f)
	if string(b) != "SA" {
		t.Fatalf("dist/sa = %q, want SA", b)
	}
}
