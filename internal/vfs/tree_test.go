package vfs

import (
	"errors"
	"io"
	"io/fs"
	"testing"
	"testing/fstest"
)

// standardTree is the shape the rest of these tests read against: one set
// merged from a whole-root image layer and a dist-only directory layer,
// plus a generated command file.
func standardTree(t *testing.T) *Tree {
	t.Helper()
	dir := t.TempDir()
	img := makeImage(t, dir, "tools.image", map[string]string{
		"stand/fx.64": "FX",
		"dist/sa":     "SA",
	})
	extracted := makeDir(t, dir+"/found", map[string]string{"dist/foundation.sw": "F"})

	tree := build(t, []SetSpec{{
		Name: "6.5.30",
		Layers: []LayerSpec{
			wholeRoot("tools", img),
			{Name: "foundations", Dir: extracted, SourceDir: "dist", TargetDir: "dist"},
		},
	}})
	if err := tree.AddGenerated("install.cmds", "admin-source", []byte("open server:/foundations/dist\n")); err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestTreeSatisfiesFSTest(t *testing.T) {
	tree := standardTree(t)
	if err := fstest.TestFS(tree,
		"6.5.30/stand/fx.64",
		"6.5.30/dist/sa",
		"6.5.30/dist/foundation.sw",
		"install.cmds",
	); err != nil {
		t.Fatal(err)
	}
}

func TestTreeImplementsTheIOFSInterfaces(t *testing.T) {
	tree := standardTree(t)
	var _ fs.FS = tree
	var _ fs.ReadDirFS = tree
	var _ fs.StatFS = tree

	f, err := tree.Open("6.5.30/dist/sa")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rf, ok := f.(File)
	if !ok {
		t.Fatalf("Open returned %T, want a File with random access", f)
	}
	if rf.Size() != 2 {
		t.Errorf("Size = %d, want 2", rf.Size())
	}
	p := make([]byte, 1)
	if _, err := rf.ReadAt(p, 1); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if p[0] != 'A' {
		t.Errorf("ReadAt(1) = %q, want A", p)
	}

	root, err := tree.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, ok := root.(fs.ReadDirFile); !ok {
		t.Fatalf("Open(.) returned %T, want an fs.ReadDirFile", root)
	}
}

func TestTreeStatReportsMetadata(t *testing.T) {
	tree := standardTree(t)

	info, err := tree.Stat("6.5.30/dist/sa")
	if err != nil {
		t.Fatal(err)
	}
	if info.Name() != "sa" || info.IsDir() || info.Size() != 2 {
		t.Fatalf("Stat = %+v", info)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want the source perm 0644", info.Mode())
	}
	meta, ok := info.Sys().(*Metadata)
	if !ok {
		t.Fatalf("Sys() = %T, want *Metadata", info.Sys())
	}
	if meta.Ino == 0 {
		t.Error("Metadata.Ino is unset")
	}
	if meta.Origin.Kind != OriginImage || meta.Origin.Source != "tools" {
		t.Errorf("Metadata.Origin = %+v", meta.Origin)
	}

	dirInfo, err := tree.Stat("6.5.30/dist")
	if err != nil {
		t.Fatal(err)
	}
	if !dirInfo.IsDir() || dirInfo.Mode() != fs.ModeDir|0o755 {
		t.Fatalf("directory Stat = %v mode %v", dirInfo.IsDir(), dirInfo.Mode())
	}
}

// TestTreeRejectsInvalidAndMissingNames pins the io/fs error contract the
// protocol adapters rely on, and with it containment. The tree is an index
// of paths the build materialized, so ".." is not resolved but refused:
// every name goes through fs.ValidPath, which rejects a leading slash, a
// trailing one, and any ".." component outright. A client cannot address a
// host path, or another set, no matter how it spells the climb.
func TestTreeRejectsInvalidAndMissingNames(t *testing.T) {
	tree := standardTree(t)

	invalid := []string{"/6.5.30/dist/sa", "6.5.30/../6.5.30/dist/sa", "../etc/passwd", "6.5.30/dist/", ""}
	for _, name := range invalid {
		_, err := tree.Open(name)
		var pe *fs.PathError
		if !errors.As(err, &pe) {
			t.Fatalf("Open(%q) error = %v, want a *fs.PathError", name, err)
		}
		if !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("Open(%q) error = %v, want fs.ErrInvalid", name, err)
		}
	}

	_, err := tree.Open("6.5.30/dist/absent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Open error = %v, want ErrNotFound", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Open error = %v, want fs.ErrNotExist too", err)
	}
	if _, err := tree.Stat("absent"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat error = %v, want fs.ErrNotExist", err)
	}
	if _, err := tree.ReadDir("6.5.30/absent"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadDir error = %v, want fs.ErrNotExist", err)
	}
	if _, err := tree.Resolve("6.5.30/dist/absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Resolve error = %v, want ErrNotFound", err)
	}
	if _, err := tree.ReadDir("6.5.30/dist/sa"); err == nil {
		t.Error("ReadDir on a regular file succeeded")
	}
}

// TestTreeReadDirIsLexicalAndInodesAreStable: listings are sorted so an
// operator's ls and inst's own walk see one order, and two builds of the
// same spec hand the same path the same inode, so identity survives a
// restart.
func TestTreeReadDirIsLexicalAndInodesAreStable(t *testing.T) {
	dir := t.TempDir()
	img := makeImage(t, dir, "tools.image", map[string]string{
		"dist/sa":       "SA",
		"dist/.iscd":    "",
		"dist/prod.sw":  "P",
		"dist/apple.sw": "A",
	})
	spec := []SetSpec{{Name: "6.5.30", Layers: []LayerSpec{wholeRoot("tools", img)}}}

	first := build(t, spec)
	want := []string{".iscd", "apple.sw", "prod.sw", "sa"}
	if got := dirNames(t, first, "6.5.30/dist"); !equalStrings(got, want) {
		t.Fatalf("ReadDir = %v, want %v", got, want)
	}

	second := build(t, spec)
	for _, p := range treePaths(t, first) {
		a, err := first.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		b, err := second.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		ai := a.Sys().(*Metadata).Ino
		bi := b.Sys().(*Metadata).Ino
		if ai != bi {
			t.Errorf("%s: inode %d on the first build, %d on the second", p, ai, bi)
		}
	}
	rootInfo, err := first.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	if got := rootInfo.Sys().(*Metadata).Ino; got != 2 {
		t.Errorf("root inode = %d, want 2", got)
	}
}
