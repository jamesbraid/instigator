package vfs

import (
	"io"
	"testing"
)

func TestSyntheticFileServed(t *testing.T) {
	tree, err := BuildTree(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	tree.AddSynthetic("install", []byte("do the install\nstep one\n"))

	names, err := tree.ReadDir("")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range names {
		if n == "install" {
			found = true
		}
	}
	if !found {
		t.Fatalf("synthetic file not listed at root: %v", names)
	}

	f, err := tree.Open("/install")
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, f.Size())
	if _, err := f.ReadAt(b, 0); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if string(b) != "do the install\nstep one\n" {
		t.Fatalf("content = %q", b)
	}
}
