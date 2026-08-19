package vfs

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/jamesbraid/instigator/efs/efstest"
)

// makeImage writes a CD image at dir/name whose EFS holds files, keyed by
// slash-separated path with intermediate directories created. efstest's
// geometry leaves five inodes beyond the root, and every file and every
// intermediate directory costs one, so fixtures stay deliberately small.
func makeImage(t *testing.T, dir, name string, files map[string]string) string {
	t.Helper()
	tree := map[string]any{}
	for p, content := range files {
		insertFixture(t, tree, p, content)
	}
	img := efstest.New()
	img.SetRoot(addFixtureDir(img, tree))
	return writeCD(t, dir, name, img)
}

// writeCD lays a built image out as CD media on disk.
func writeCD(t *testing.T, dir, name string, img *efstest.Builder) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, img.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// insertFixture adds one slash-separated path to a nested fixture tree,
// where a string leaf is file content and a map is a directory.
func insertFixture(t *testing.T, dir map[string]any, p, content string) {
	t.Helper()
	base, rest, nested := splitFixturePath(p)
	if !nested {
		dir[base] = content
		return
	}
	sub, ok := dir[base].(map[string]any)
	if !ok {
		if _, taken := dir[base]; taken {
			t.Fatalf("fixture %q: %q is already a file", p, base)
		}
		sub = map[string]any{}
		dir[base] = sub
	}
	insertFixture(t, sub, rest, content)
}

func splitFixturePath(p string) (base, rest string, nested bool) {
	for i := 0; i < len(p); i++ {
		if p[i] == '/' {
			return p[:i], p[i+1:], true
		}
	}
	return p, "", false
}

// addFixtureDir writes a fixture directory bottom-up and returns its
// entries, ready for AddDir or SetRoot.
func addFixtureDir(img *efstest.Builder, dir map[string]any) map[string]uint32 {
	entries := map[string]uint32{}
	for name, v := range dir {
		switch v := v.(type) {
		case string:
			entries[name] = img.AddFile(0o644, []byte(v))
		case map[string]any:
			entries[name] = img.AddDir(addFixtureDir(img, v))
		}
	}
	return entries
}

// makeDir writes a pre-extracted directory layer holding files, keyed the
// same way makeImage keys its images.
func makeDir(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// build assembles a tree and closes it when the test ends.
func build(t *testing.T, sets []SetSpec) *Tree {
	t.Helper()
	tree, err := Build(sets)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { tree.Close() })
	return tree
}

// readTree returns a tree path's full contents, read through fs.File.
func readTree(t *testing.T, tree *Tree, name string) string {
	t.Helper()
	f, err := tree.Open(name)
	if err != nil {
		t.Fatalf("Open(%q): %v", name, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %q: %v", name, err)
	}
	return string(b)
}

// dirNames returns the entry names of a tree directory, in the order
// ReadDir reports them.
func dirNames(t *testing.T, tree *Tree, name string) []string {
	t.Helper()
	ents, err := tree.ReadDir(name)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", name, err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// treePaths lists every regular file in the tree, for fstest and for
// whole-tree assertions.
func treePaths(t *testing.T, tree *Tree) []string {
	t.Helper()
	var out []string
	err := fs.WalkDir(tree, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	return out
}

// bootLayer is the shape a bootable set's one boot layer uses: its dist
// merged like any other, and its stand served at the set's stand so the
// PROM can fetch fx.64.
func bootLayer(name, image string) LayerSpec {
	return LayerSpec{Name: name, Image: image, Boot: true}
}

// distLayer is the shape every other layer uses: its dist contributed to
// the set's own dist, nothing else.
func distLayer(name, image string) LayerSpec {
	return LayerSpec{Name: name, Image: image}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
