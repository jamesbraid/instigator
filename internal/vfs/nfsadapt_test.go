package vfs

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jamesbraid/instigator/efs/efstest"
)

func singleDiscTree(t *testing.T) *Tree {
	t.Helper()
	dir := t.TempDir()
	img := efstest.New()
	sa := img.AddFile(0o644, []byte("distribution"))
	dist := img.AddDir(map[string]uint32{"sa": sa})
	img.SetRoot(map[string]uint32{"dist": dist})
	if err := os.WriteFile(filepath.Join(dir, "disc1.iso"), img.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	tree, err := BuildTree([]MediaSet{{Name: "6.5.30", Dir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tree.Close() })
	return tree
}

func TestNFSExportRootLookupRead(t *testing.T) {
	tree := singleDiscTree(t)
	exp := tree.NFSExport()

	root, err := exp.MountRoot("6.5.30/disc1")
	if err != nil {
		t.Fatal(err)
	}
	dist, err := exp.Lookup(root, "dist")
	if err != nil {
		t.Fatal(err)
	}
	sa, err := exp.Lookup(dist, "sa")
	if err != nil {
		t.Fatal(err)
	}
	if sa.Size() != 12 {
		t.Fatalf("size = %d", sa.Size())
	}
	// filehandle round-trip: a node reachable by its stable id
	again, err := exp.NodeByID(sa.ID())
	if err != nil {
		t.Fatal(err)
	}
	if again.ID() != sa.ID() {
		t.Fatal("NodeByID returned a different node")
	}
	p := make([]byte, 12)
	n, err := exp.ReadAt(sa, p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(p[:n]) != "distribution" {
		t.Fatalf("read = %q", p[:n])
	}
}

// Describe backs NFS's per-open log line, so it must name the real
// image and build up the in-image path across Lookups, not just the
// leaf name of whatever was last looked up.
func TestNFSExportDescribe(t *testing.T) {
	tree := singleDiscTree(t)
	exp := tree.NFSExport()

	root, err := exp.MountRoot("6.5.30/disc1")
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := root.(interface{ Describe() string }); !ok || d.Describe() != "disc1.iso:/" {
		t.Fatalf("root Describe = %v", root)
	}
	dist, err := exp.Lookup(root, "dist")
	if err != nil {
		t.Fatal(err)
	}
	sa, err := exp.Lookup(dist, "sa")
	if err != nil {
		t.Fatal(err)
	}
	d, ok := sa.(interface{ Describe() string })
	if !ok {
		t.Fatal("sa does not implement Describe")
	}
	if got, want := d.Describe(), "disc1.iso:/dist/sa"; got != want {
		t.Fatalf("Describe() = %q, want %q", got, want)
	}
}

func TestNFSExportUnknownDisc(t *testing.T) {
	tree := singleDiscTree(t)
	if _, err := tree.NFSExport().MountRoot("6.5.30/absent"); err == nil {
		t.Fatal("mount of unknown disc succeeded")
	}
}

func TestNFSExportFilehandlesDistinctAcrossDiscs(t *testing.T) {
	dir := t.TempDir()
	makeDisc(t, dir, "IRIX 6.5.30 Overlay 1of3.iso", "one")
	makeDisc(t, dir, "IRIX 6.5.30 Overlay 2of3.iso", "two")
	tree, err := BuildTree([]MediaSet{{Name: "6.5.30", Dir: dir}})
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	exp := tree.NFSExport()

	r1, err := exp.MountRoot("6.5.30/irix-6-5-30-overlay-1of3")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := exp.MountRoot("6.5.30/irix-6-5-30-overlay-2of3")
	if err != nil {
		t.Fatal(err)
	}
	// both roots are EFS inode 2, but the filehandles must differ
	if r1.ID() == r2.ID() {
		t.Fatalf("filehandles collide across discs: %#x", r1.ID())
	}
	// and each resolves back to its own disc's content
	fx1, _ := exp.Lookup(r1, "stand")
	fx2, _ := exp.Lookup(r2, "stand")
	if fx1.ID() == fx2.ID() {
		t.Fatal("child filehandles collide across discs")
	}
	// NodeByID round-trips to the correct disc
	back, err := exp.NodeByID(r2.ID())
	if err != nil || back.ID() != r2.ID() {
		t.Fatalf("NodeByID(r2) = %v %v", back, err)
	}
}
