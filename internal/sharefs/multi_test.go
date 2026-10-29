package sharefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jamesbraid/instigator/nfs"
)

func openTwo(t *testing.T) (*Multi, string, string) {
	t.Helper()
	a, b := t.TempDir(), t.TempDir()
	fa, err := Open("a", a, Options{})
	if err != nil {
		t.Fatal(err)
	}
	fb, err := Open("b", b, Options{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := OpenMulti(fa, fb)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m, a, b
}

func TestMultiRoutesByShare(t *testing.T) {
	m, a, b := openTwo(t)
	if got := m.Exports(); len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Fatalf("exports %v", got)
	}
	ra, err := m.MountRoot("/a")
	if err != nil {
		t.Fatal(err)
	}
	rb, err := m.MountRoot("/b/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.MountRoot("/c"); !errors.Is(err, nfs.ErrNotFound) {
		t.Errorf("unknown share: %v", err)
	}

	f, err := m.Create(rb, "f", nfs.Guarded, nfs.SetAttr{}, [8]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.WriteAt(f, []byte("hello"), 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(b, "f")); string(got) != "hello" {
		t.Errorf("file in b = %q", got)
	}
	if _, err := os.Stat(filepath.Join(a, "f")); err == nil {
		t.Error("file landed in a")
	}

	// A handle finds its file through the right share.
	back, err := m.NodeByHandle(m.Handle(f))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if n, err := m.ReadAt(back, buf, 0); err != nil || string(buf[:n]) != "hello" {
		t.Errorf("read %q, %v", buf[:n], err)
	}
	if string(m.Handle(ra)) == string(m.Handle(rb)) {
		t.Error("the two roots share a handle")
	}

	// A move between shares crosses filesystems.
	if err := m.Rename(rb, "f", ra, "f"); !errors.Is(err, nfs.ErrXDev) {
		t.Errorf("cross-share rename: %v", err)
	}
	if err := m.Rename(rb, "f", rb, "g"); err != nil {
		t.Errorf("rename within a share: %v", err)
	}
	if err := m.Remove(rb, "g"); err != nil {
		t.Error(err)
	}
}

func TestMultiHandles(t *testing.T) {
	m, _, _ := openTwo(t)
	ra, _ := m.MountRoot("/a")
	fh := m.Handle(ra)

	other := append([]byte(nil), fh...)
	other[7] ^= 0xff // an export id nobody serves
	if _, err := m.NodeByHandle(other); !errors.Is(err, nfs.ErrStale) {
		t.Errorf("handle of an unknown share: %v", err)
	}
	if _, err := m.NodeByHandle(fh[:16]); !errors.Is(err, nfs.ErrBadHandle) {
		t.Errorf("short handle: %v", err)
	}
	bad := append([]byte(nil), fh...)
	bad[1] = 1
	bad[7] ^= 0xff
	if _, err := m.NodeByHandle(bad); !errors.Is(err, nfs.ErrBadHandle) {
		t.Errorf("malformed handle: %v", err)
	}
}

func TestOpenMultiRefusesDuplicatesAndNone(t *testing.T) {
	d := t.TempDir()
	f1, _ := Open("x", d, Options{})
	f2, _ := Open("x", d, Options{})
	defer f1.Close()
	defer f2.Close()
	if _, err := OpenMulti(f1, f2); err == nil {
		t.Error("duplicate name accepted")
	}
	if _, err := OpenMulti(); err == nil {
		t.Error("no shares accepted")
	}
}
