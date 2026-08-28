package nfsexport_test

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/jamesbraid/instigator/efs/efstest"
	"github.com/jamesbraid/instigator/internal/nfsexport"
	"github.com/jamesbraid/instigator/internal/source"
	"github.com/jamesbraid/instigator/internal/vfs"
	"github.com/jamesbraid/instigator/nfs"
)

// buildTestTree assembles a small synthetic install-set tree: one set
// ("6.5.30") whose single layer is an EFS image holding dist/sa and a
// nested dist/sub/nested.txt, its dist merged into the set's dist. That
// gives Export something with a merged directory (the set root, no single
// layer origin), an image-backed regular file, and a subdirectory to
// exercise nested Lookup/ReadDir.
func buildTestTree(t *testing.T) *vfs.Tree {
	t.Helper()
	dir := t.TempDir()

	img := efstest.New()
	sa := img.AddFile(0o644, []byte("distribution"))
	nested := img.AddFile(0o644, []byte("nested content"))
	sub := img.AddDir(map[string]uint32{"nested.txt": nested})
	dist := img.AddDir(map[string]uint32{"sa": sa, "sub": sub})
	img.SetRoot(map[string]uint32{"dist": dist})

	imgPath := filepath.Join(dir, "disc1.iso")
	if err := os.WriteFile(imgPath, img.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}

	tree, err := vfs.Build([]vfs.SetSpec{{
		Name: "6.5.30",
		Layers: []vfs.LayerSpec{
			{Name: "media", Source: imgPath},
		},
	}}, source.New(source.Options{CacheDir: t.TempDir()}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { tree.Close() })
	return tree
}

func lookup(t *testing.T, fsys nfs.FS, dir nfs.Node, name string) nfs.Node {
	t.Helper()
	n, err := fsys.Lookup(dir, name)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", name, err)
	}
	return n
}

func readAll(t *testing.T, fsys nfs.FS, n nfs.Node) []byte {
	t.Helper()
	p := make([]byte, n.Size())
	got, err := fsys.ReadAt(n, p, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	return p[:got]
}

func TestExportMountLookupReadDirReadAt(t *testing.T) {
	tree := buildTestTree(t)
	fsys := nfsexport.Export(tree)

	root, err := fsys.MountRoot("6.5.30")
	if err != nil {
		t.Fatalf("MountRoot: %v", err)
	}
	dist := lookup(t, fsys, root, "dist")
	sa := lookup(t, fsys, dist, "sa")

	// Mode() must carry the type bits nfs.Node documents (0o040000 for a
	// directory, 0o100000 for a regular file), not just the permission
	// bits fs.FileMode.Perm() reports - a regression here would silently
	// break the wire protocol's file-type field.
	if got := dist.Mode() & 0o170000; got != 0o040000 {
		t.Fatalf("dist.Mode() type bits = %#o, want dir (%#o)", got, uint32(0o040000))
	}
	if got := sa.Mode() & 0o170000; got != 0o100000 {
		t.Fatalf("sa.Mode() type bits = %#o, want regular (%#o)", got, uint32(0o100000))
	}

	if want := int64(len("distribution")); sa.Size() != want {
		t.Fatalf("sa.Size() = %d, want %d", sa.Size(), want)
	}
	if got := readAll(t, fsys, sa); string(got) != "distribution" {
		t.Fatalf("ReadAt(sa) = %q", got)
	}

	ents, err := fsys.ReadDir(dist)
	if err != nil {
		t.Fatalf("ReadDir(dist): %v", err)
	}
	names := map[string]bool{}
	for _, e := range ents {
		names[e.Name] = true
	}
	if !names["sa"] || !names["sub"] {
		t.Fatalf("ReadDir(dist) entries = %v, want sa and sub", ents)
	}

	// Nested directory traversal.
	sub := lookup(t, fsys, dist, "sub")
	nested := lookup(t, fsys, sub, "nested.txt")
	if got := readAll(t, fsys, nested); string(got) != "nested content" {
		t.Fatalf("ReadAt(nested) = %q", got)
	}
}

func TestExportFilehandleRoundTrip(t *testing.T) {
	tree := buildTestTree(t)
	fsys := nfsexport.Export(tree)

	root, err := fsys.MountRoot("6.5.30")
	if err != nil {
		t.Fatalf("MountRoot: %v", err)
	}
	dist := lookup(t, fsys, root, "dist")
	sa := lookup(t, fsys, dist, "sa")

	again, err := fsys.NodeByID(sa.ID())
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if again.ID() != sa.ID() {
		t.Fatalf("NodeByID round trip: id = %#x, want %#x", again.ID(), sa.ID())
	}
	if got := readAll(t, fsys, again); string(got) != "distribution" {
		t.Fatalf("ReadAt(NodeByID(sa.ID())) = %q", got)
	}
}

func TestExportFilehandlesDistinctAcrossPaths(t *testing.T) {
	tree := buildTestTree(t)
	fsys := nfsexport.Export(tree)

	root, err := fsys.MountRoot("6.5.30")
	if err != nil {
		t.Fatalf("MountRoot: %v", err)
	}
	dist := lookup(t, fsys, root, "dist")
	sa := lookup(t, fsys, dist, "sa")
	sub := lookup(t, fsys, dist, "sub")
	nested := lookup(t, fsys, sub, "nested.txt")

	seen := map[uint64]string{root.ID(): "root", dist.ID(): "dist"}
	for name, n := range map[string]nfs.Node{"sa": sa, "sub": sub, "nested": nested} {
		if other, dup := seen[n.ID()]; dup {
			t.Fatalf("filehandle %#x collides between %q and %q", n.ID(), other, name)
		}
		seen[n.ID()] = name
	}
}

func TestExportDescribe(t *testing.T) {
	tree := buildTestTree(t)
	fsys := nfsexport.Export(tree)

	root, err := fsys.MountRoot("6.5.30")
	if err != nil {
		t.Fatalf("MountRoot: %v", err)
	}
	dist := lookup(t, fsys, root, "dist")
	sa := lookup(t, fsys, dist, "sa")

	d, ok := sa.(interface{ Describe() string })
	if !ok {
		t.Fatal("regular-file node does not implement nfs.Describer")
	}
	if got, want := d.Describe(), "media:/dist/sa"; got != want {
		t.Fatalf("Describe() = %q, want %q", got, want)
	}
}

func TestExportUnknownMountPath(t *testing.T) {
	tree := buildTestTree(t)
	fsys := nfsexport.Export(tree)

	if _, err := fsys.MountRoot("nope"); err != nfs.ErrNotFound {
		t.Fatalf("MountRoot(nope) = %v, want ErrNotFound", err)
	}
}

func TestExportUnknownFilehandle(t *testing.T) {
	tree := buildTestTree(t)
	fsys := nfsexport.Export(tree)

	if _, err := fsys.NodeByID(0xdeadbeef); err != nfs.ErrStale {
		t.Fatalf("NodeByID(unknown) = %v, want ErrStale", err)
	}
}

// TestExportReadAtShortReadAtEOF forces the adapter's io.EOF-as-short-read
// branch: a buffer bigger than the remaining bytes makes the underlying
// efs.FS.ReadAt return (n, io.EOF), which ReadAt must swallow as a normal
// short read (err == nil) rather than translate to nfs.ErrIO.
func TestExportReadAtShortReadAtEOF(t *testing.T) {
	tree := buildTestTree(t)
	fsys := nfsexport.Export(tree)

	root, err := fsys.MountRoot("6.5.30")
	if err != nil {
		t.Fatalf("MountRoot: %v", err)
	}
	dist := lookup(t, fsys, root, "dist")
	sa := lookup(t, fsys, dist, "sa")

	p := make([]byte, sa.Size()+8) // bigger than the file, forces a short read
	got, err := fsys.ReadAt(sa, p, 0)
	if err != nil {
		t.Fatalf("ReadAt past EOF: err = %v, want nil (EOF swallowed)", err)
	}
	if int64(got) != sa.Size() {
		t.Fatalf("ReadAt past EOF: got %d bytes, want %d (short read)", got, sa.Size())
	}
	if string(p[:got]) != "distribution" {
		t.Fatalf("ReadAt past EOF content = %q", p[:got])
	}
}

func TestExportLookupMissingChild(t *testing.T) {
	tree := buildTestTree(t)
	fsys := nfsexport.Export(tree)

	root, err := fsys.MountRoot("6.5.30")
	if err != nil {
		t.Fatalf("MountRoot: %v", err)
	}
	dist := lookup(t, fsys, root, "dist")

	if _, err := fsys.Lookup(dist, "nonexistent"); err != nfs.ErrNotFound {
		t.Fatalf("Lookup(dist, nonexistent) = %v, want ErrNotFound", err)
	}
}

func TestExportLookupNotADirectory(t *testing.T) {
	tree := buildTestTree(t)
	fsys := nfsexport.Export(tree)

	root, err := fsys.MountRoot("6.5.30")
	if err != nil {
		t.Fatalf("MountRoot: %v", err)
	}
	dist := lookup(t, fsys, root, "dist")
	sa := lookup(t, fsys, dist, "sa")

	if _, err := fsys.Lookup(sa, "anything"); err != nfs.ErrNotDir {
		t.Fatalf("Lookup(file, name) = %v, want ErrNotDir", err)
	}
}

// TestExportOverMapFS exercises Export against a plain fstest.MapFS, whose
// Stat never reports a *vfs.Metadata Sys(): ids must fall back to a stable
// per-path assignment, and no Describer should be exposed since MapFS has
// no Resolve to build a manifest line from.
func TestExportOverMapFS(t *testing.T) {
	mapfs := fstest.MapFS{
		"a.txt":     &fstest.MapFile{Data: []byte("hello")},
		"dir/b.txt": &fstest.MapFile{Data: []byte("world")},
	}
	fsys := nfsexport.Export(mapfs)

	root, err := fsys.MountRoot("")
	if err != nil {
		t.Fatalf("MountRoot(\"\"): %v", err)
	}
	a := lookup(t, fsys, root, "a.txt")
	if got := readAll(t, fsys, a); string(got) != "hello" {
		t.Fatalf("ReadAt(a.txt) = %q", got)
	}
	if _, ok := a.(interface{ Describe() string }); ok {
		t.Fatal("MapFS-backed node should not implement nfs.Describer")
	}

	dir := lookup(t, fsys, root, "dir")
	b := lookup(t, fsys, dir, "b.txt")
	if got := readAll(t, fsys, b); string(got) != "world" {
		t.Fatalf("ReadAt(dir/b.txt) = %q", got)
	}
	if a.ID() == b.ID() {
		t.Fatalf("fallback ids collide: %#x", a.ID())
	}

	again, err := fsys.NodeByID(b.ID())
	if err != nil || again.ID() != b.ID() {
		t.Fatalf("NodeByID(b.ID()) = %v, %v", again, err)
	}
}

func TestExportReadlinkRefused(t *testing.T) {
	tree := buildTestTree(t)
	fsys := nfsexport.Export(tree)

	root, err := fsys.MountRoot("6.5.30")
	if err != nil {
		t.Fatalf("MountRoot: %v", err)
	}
	dist := lookup(t, fsys, root, "dist")
	sa := lookup(t, fsys, dist, "sa")

	if _, err := fsys.Readlink(sa); err != nfs.ErrIO {
		t.Fatalf("Readlink(sa) = %v, want ErrIO", err)
	}
}

// buildTwoSetTree assembles a tree with two sets, so a lookup that walks
// out of a mounted subtree lands somewhere observable: escaping "6.5.30"
// reaches the export root, from which the whole "applications" set is
// readable.
func buildTwoSetTree(t *testing.T) *vfs.Tree {
	t.Helper()
	dir := t.TempDir()

	img := efstest.New()
	sa := img.AddFile(0o644, []byte("distribution"))
	nested := img.AddFile(0o644, []byte("nested content"))
	sub := img.AddDir(map[string]uint32{"nested.txt": nested})
	dist := img.AddDir(map[string]uint32{"sa": sa, "sub": sub})
	img.SetRoot(map[string]uint32{"dist": dist})

	imgPath := filepath.Join(dir, "disc1.iso")
	if err := os.WriteFile(imgPath, img.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}

	tree, err := vfs.Build([]vfs.SetSpec{
		{Name: "6.5.30", Layers: []vfs.LayerSpec{
			{Name: "media", Source: imgPath},
		}},
		{Name: "applications", Layers: []vfs.LayerSpec{
			{Name: "apps", Source: imgPath},
		}},
	}, source.New(source.Options{CacheDir: t.TempDir()}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { tree.Close() })
	return tree
}

// TestExportLookupRejectsNonComponentNames holds the mount boundary. NFS
// LOOKUP takes one directory entry name, never a path: a client mounted
// at /6.5.30 that asks for ".." must not be handed the export root, from
// which every other install set is reachable. "." and any name carrying a
// slash are equally not entry names.
func TestExportLookupRejectsNonComponentNames(t *testing.T) {
	tree := buildTwoSetTree(t)
	fsys := nfsexport.Export(tree)

	root, err := fsys.MountRoot("6.5.30")
	if err != nil {
		t.Fatalf("MountRoot: %v", err)
	}
	dist := lookup(t, fsys, root, "dist")

	for _, tc := range []struct {
		dir  nfs.Node
		name string
	}{
		{root, ".."},
		{root, "."},
		{root, ""},
		{root, "dist/sa"},
		{dist, ".."},
		{dist, "sub/nested.txt"},
		{dist, "/sa"},
	} {
		n, err := fsys.Lookup(tc.dir, tc.name)
		if err != nfs.ErrNotFound {
			t.Errorf("Lookup(%q) = (%v, %v), want ErrNotFound", tc.name, n, err)
		}
		if n != nil {
			t.Errorf("Lookup(%q) returned node %#x; the mount boundary leaked", tc.name, n.ID())
		}
	}

	// The escape must not be reachable by id either: the export root is
	// simply never handed to a client that mounted a subtree.
	if _, err := fsys.Lookup(root, ".."); err == nil {
		t.Fatal("Lookup(\"..\") escaped the mounted subtree")
	}

	// A single ordinary component still resolves.
	sa := lookup(t, fsys, dist, "sa")
	if got := readAll(t, fsys, sa); string(got) != "distribution" {
		t.Fatalf("ReadAt(sa) = %q, want distribution", got)
	}
}
