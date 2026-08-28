package efs_test

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"sort"
	"testing"

	"github.com/jamesbraid/instigator/efs"
	"github.com/jamesbraid/instigator/efs/efstest"
)

func fsysFixture(t *testing.T) fs.FS {
	t.Helper()
	img := efstest.New()
	hello := img.AddFile(0o644, []byte("hello, octane\n"))
	nested := img.AddFile(0o444, []byte("nested"))
	sub := img.AddDir(map[string]uint32{"nested.txt": nested})
	link := img.AddSymlink("hello.txt")
	img.SetRoot(map[string]uint32{"hello.txt": hello, "sub": sub, "link": link})
	f, err := efs.Open(bytes.NewReader(img.Bytes()), 0)
	if err != nil {
		t.Fatal(err)
	}
	return f.FSys()
}

func TestFSysReadDirRoot(t *testing.T) {
	fsys := fsysFixture(t)
	ents, err := fs.ReadDir(fsys, ".")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]fs.FileMode{}
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		got[e.Name()] = info.Mode().Type()
	}
	if got["hello.txt"] != 0 {
		t.Errorf("hello.txt type = %v, want regular", got["hello.txt"])
	}
	if got["sub"]&fs.ModeDir == 0 {
		t.Errorf("sub type = %v, want dir", got["sub"])
	}
	if got["link"]&fs.ModeSymlink == 0 {
		t.Errorf("link type = %v, want symlink", got["link"])
	}
}

func TestFSysReadRegularAndReaderAt(t *testing.T) {
	fsys := fsysFixture(t)
	f, err := fsys.Open("sub/nested.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ra, ok := f.(io.ReaderAt)
	if !ok {
		t.Fatal("opened regular file does not implement io.ReaderAt")
	}
	p := make([]byte, 3)
	if _, err := ra.ReadAt(p, 3); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if string(p) != "ted" {
		t.Errorf("ReadAt(3) = %q, want %q", p, "ted")
	}
	all, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(all) != "nested" {
		t.Errorf("ReadAll = %q, want %q", all, "nested")
	}
}

func TestFSysStatOwnerViaSys(t *testing.T) {
	fsys := fsysFixture(t)
	info, err := fs.Stat(fsys, "hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*efs.Stat)
	if !ok {
		t.Fatalf("Sys() = %T, want *efs.Stat", info.Sys())
	}
	if st.UID != 10 || st.GID != 20 || st.Nlink != 1 {
		t.Errorf("Stat = %+v, want UID 10 GID 20 Nlink 1", st)
	}
	if info.Size() != 14 {
		t.Errorf("Size = %d, want 14", info.Size())
	}
}

func TestFSysReadLinkDoesNotFollow(t *testing.T) {
	fsys := fsysFixture(t)
	target, err := fs.ReadLink(fsys, "link")
	if err != nil {
		t.Fatal(err)
	}
	if target != "hello.txt" {
		t.Errorf("ReadLink = %q, want %q", target, "hello.txt")
	}
	// Stat does not follow: the link stats as a symlink, not the target.
	info, err := fs.Stat(fsys, "link")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("Stat(link) mode = %v, want symlink", info.Mode())
	}
}

func TestFSysInvalidAndMissing(t *testing.T) {
	fsys := fsysFixture(t)
	if _, err := fsys.Open("/abs"); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("Open(/abs) err = %v, want ErrInvalid", err)
	}
	if _, err := fsys.Open("nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open(nope) err = %v, want ErrNotExist", err)
	}
}

// TestLookupMissingIsErrNotFound pins the sentinel a genuine absence returns,
// distinct from an I/O or corruption error. The fs view maps only this to
// fs.ErrNotExist and surfaces every other error as itself, so a failed read
// (for a remote-backed image, a chunk re-fetch that fails) is never disguised
// as a missing file.
func TestLookupMissingIsErrNotFound(t *testing.T) {
	img := efstest.New()
	hello := img.AddFile(0o644, []byte("hi"))
	img.SetRoot(map[string]uint32{"hello.txt": hello})
	f, err := efs.Open(bytes.NewReader(img.Bytes()), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Lookup("nope"); !errors.Is(err, efs.ErrNotFound) {
		t.Errorf("Lookup(nope) err = %v, want efs.ErrNotFound", err)
	}
}

// TestFSysReadDirSorted: fs.ReadDirFS requires entries sorted by name, but EFS
// stores them in directory-slot order, so the view must sort them.
func TestFSysReadDirSorted(t *testing.T) {
	img := efstest.New()
	a := img.AddFile(0o644, []byte("a"))
	m := img.AddFile(0o644, []byte("m"))
	z := img.AddFile(0o644, []byte("z"))
	img.SetRoot(map[string]uint32{"zebra": z, "apple": a, "mango": m})
	f, err := efs.Open(bytes.NewReader(img.Bytes()), 0)
	if err != nil {
		t.Fatal(err)
	}
	ents, err := fs.ReadDir(f.FSys(), ".")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(ents))
	for i, e := range ents {
		names[i] = e.Name()
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("ReadDir entries not sorted by name: %v", names)
	}
}
