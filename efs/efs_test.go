package efs_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/jamesbraid/instigator/efs"
	"github.com/jamesbraid/instigator/efs/efstest"
)

// fixture builds an image with a small tree:
//
//	/hello.txt   "hello, octane\n"
//	/sub/nested.txt  "nested"
//	/link -> hello.txt
func fixture(t *testing.T) (*efs.FS, map[string]uint32) {
	t.Helper()
	img := efstest.New()
	inos := map[string]uint32{}
	inos["hello.txt"] = img.AddFile(0o644, []byte("hello, octane\n"))
	inos["nested.txt"] = img.AddFile(0o444, []byte("nested"))
	inos["sub"] = img.AddDir(map[string]uint32{"nested.txt": inos["nested.txt"]})
	inos["link"] = img.AddSymlink("hello.txt")
	img.SetRoot(map[string]uint32{
		"hello.txt": inos["hello.txt"],
		"sub":       inos["sub"],
		"link":      inos["link"],
	})
	fs, err := efs.Open(bytes.NewReader(img.Bytes()), 0)
	if err != nil {
		t.Fatal(err)
	}
	return fs, inos
}

func TestOpenRejectsBadMagic(t *testing.T) {
	if _, err := efs.Open(bytes.NewReader(make([]byte, 4096)), 0); err == nil {
		t.Fatal("Open accepted zeroed superblock; want magic error")
	}
}

func TestInodeFields(t *testing.T) {
	fs, inos := fixture(t)
	ino, err := fs.Inode(inos["hello.txt"])
	if err != nil {
		t.Fatal(err)
	}
	if ino.Mode != 0x8000|0o644 || ino.Size != 14 || ino.UID != 10 || ino.GID != 20 {
		t.Fatalf("inode = %+v", ino)
	}
	if ino.Mtime != 1000+inos["hello.txt"] {
		t.Fatalf("mtime = %d, want %d", ino.Mtime, 1000+inos["hello.txt"])
	}
	if !ino.IsRegular() || ino.IsDir() {
		t.Fatal("type predicates wrong for regular file")
	}
}

func TestReadDirRoot(t *testing.T) {
	fs, _ := fixture(t)
	root, err := fs.Inode(efs.RootIno)
	if err != nil {
		t.Fatal(err)
	}
	ents, err := fs.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range ents {
		got[e.Name] = true
	}
	for _, want := range []string{"hello.txt", "sub", "link"} {
		if !got[want] {
			t.Fatalf("missing %q in %v", want, ents)
		}
	}
}

func TestLookup(t *testing.T) {
	fs, inos := fixture(t)
	ino, err := fs.Lookup("/sub/nested.txt")
	if err != nil {
		t.Fatal(err)
	}
	if ino.Num != inos["nested.txt"] {
		t.Fatalf("resolved ino %d, want %d", ino.Num, inos["nested.txt"])
	}
	if _, err := fs.Lookup("/sub/absent"); err == nil {
		t.Fatal("Lookup found a file that does not exist")
	}
}

func TestReadAt(t *testing.T) {
	fs, inos := fixture(t)
	ino, _ := fs.Inode(inos["hello.txt"])
	p := make([]byte, 5)
	n, err := fs.ReadAt(ino, p, 7)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 || string(p) != "octan" {
		t.Fatalf("ReadAt(7) = %d %q", n, p)
	}
	// read spanning EOF returns the short count and io.EOF
	n, err = fs.ReadAt(ino, p, 12)
	if n != 2 || string(p[:n]) != "e\n" || err != io.EOF {
		t.Fatalf("ReadAt(12) = %d %q %v, want 2 \"e\\n\" EOF", n, p[:n], err)
	}
}

func TestReadAtSparseHole(t *testing.T) {
	img := efstest.New()
	bn, _ := img.AddData([]byte("tail"))
	// one extent at logical block 2; blocks 0-1 are a hole
	ino := img.AddFileExtents(2*512+4, [][3]uint32{{uint32(bn), 1, 2}})
	img.SetRoot(map[string]uint32{"sparse": ino})
	fs, err := efs.Open(bytes.NewReader(img.Bytes()), 0)
	if err != nil {
		t.Fatal(err)
	}
	node, _ := fs.Inode(ino)
	p := make([]byte, 6)
	if _, err := fs.ReadAt(node, p, 510); err != nil {
		t.Fatal(err)
	}
	if string(p) != "\x00\x00\x00\x00\x00\x00" {
		t.Fatalf("hole read = %q, want zeros", p)
	}
	if _, err := fs.ReadAt(node, p[:4], 1024); err != nil {
		t.Fatal(err)
	}
	if string(p[:4]) != "tail" {
		t.Fatalf("tail read = %q", p[:4])
	}
}

func TestMultiExtent(t *testing.T) {
	img := efstest.New()
	b1, _ := img.AddData(bytes.Repeat([]byte("A"), 512))
	b2, _ := img.AddData(bytes.Repeat([]byte("B"), 512))
	ino := img.AddFileExtents(1024, [][3]uint32{{uint32(b1), 1, 0}, {uint32(b2), 1, 1}})
	img.SetRoot(map[string]uint32{"two": ino})
	fs, err := efs.Open(bytes.NewReader(img.Bytes()), 0)
	if err != nil {
		t.Fatal(err)
	}
	node, _ := fs.Inode(ino)
	p := make([]byte, 4)
	if _, err := fs.ReadAt(node, p, 510); err != nil {
		t.Fatal(err)
	}
	if string(p) != "AABB" {
		t.Fatalf("extent boundary read = %q, want AABB", p)
	}
}

func TestIndirectExtents(t *testing.T) {
	img := efstest.New()
	var extents [][3]uint32
	var want []byte
	for i := 0; i < 14; i++ {
		c := byte('a' + i)
		bn, _ := img.AddData(bytes.Repeat([]byte{c}, 512))
		extents = append(extents, [3]uint32{uint32(bn), 1, uint32(i)})
		want = append(want, c)
	}
	ino := img.AddFileIndirect(14*512, extents)
	img.SetRoot(map[string]uint32{"big": ino})
	fs, err := efs.Open(bytes.NewReader(img.Bytes()), 0)
	if err != nil {
		t.Fatal(err)
	}
	node, _ := fs.Inode(ino)
	if len(node.Extents) != 14 {
		t.Fatalf("got %d extents, want 14", len(node.Extents))
	}
	p := make([]byte, 1)
	for i, c := range want {
		if _, err := fs.ReadAt(node, p, int64(i)*512); err != nil {
			t.Fatal(err)
		}
		if p[0] != c {
			t.Fatalf("block %d starts with %q, want %q", i, p[0], c)
		}
	}
}

func TestReadlink(t *testing.T) {
	fs, inos := fixture(t)
	node, _ := fs.Inode(inos["link"])
	if !node.IsSymlink() {
		t.Fatal("link inode not a symlink")
	}
	target, err := fs.Readlink(node)
	if err != nil {
		t.Fatal(err)
	}
	if target != "hello.txt" {
		t.Fatalf("readlink = %q", target)
	}
}
