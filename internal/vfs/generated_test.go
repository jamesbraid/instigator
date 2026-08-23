package vfs

import (
	"errors"
	"io/fs"
	"testing"
)

// TestAddGeneratedNestsAndResolves: a generated file lands at a nested
// logical path, creating its parents, and then reads, lists, stats and
// resolves through exactly the same calls a backed file does - there is no
// side map for callers to miss.
func TestAddGeneratedNestsAndResolves(t *testing.T) {
	dir := t.TempDir()
	img := makeImage(t, dir, "tools.image", map[string]string{"dist/sa": "SA"})
	tree := build(t, []SetSpec{{Name: "6.5.30", Layers: []LayerSpec{distLayer("tools", img)}}})

	cmds := []byte("open server:/foundations/dist\n")
	if err := tree.AddGenerated("6.5.30/dist/generated.commands", "test-commands", cmds); err != nil {
		t.Fatal(err)
	}
	if err := tree.AddGenerated("6.5.30/dist/.related_dists", "related_dists", []byte("../../applications/dist\n")); err != nil {
		t.Fatal(err)
	}

	if got := readTree(t, tree, "6.5.30/dist/generated.commands"); got != string(cmds) {
		t.Fatalf("read = %q, want %q", got, cmds)
	}
	want := []string{".related_dists", "generated.commands", "sa"}
	if got := dirNames(t, tree, "6.5.30/dist"); !equalStrings(got, want) {
		t.Fatalf("ReadDir = %v, want %v", got, want)
	}
	got, err := tree.Resolve("6.5.30/dist/generated.commands")
	if err != nil {
		t.Fatal(err)
	}
	if (got != Origin{Kind: OriginGenerated, Source: "test-commands"}) {
		t.Fatalf("Resolve = %+v, want a generated origin named test-commands", got)
	}
	if got := OriginGenerated.String(); got != "generated" {
		t.Errorf("OriginGenerated.String() = %q", got)
	}
	info, err := tree.Stat("6.5.30/dist/generated.commands")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(cmds)) || info.Mode().Perm() != 0o444 {
		t.Fatalf("Stat = size %d mode %v", info.Size(), info.Mode())
	}
}

// TestAddGeneratedCreatesMissingParents: a generated file at the root or
// under directories no layer contributed still lands, with generated
// parents.
func TestAddGeneratedCreatesMissingParents(t *testing.T) {
	tree := build(t, nil)

	if err := tree.AddGenerated("generated/guide.md", "test-guide", []byte("guide\n")); err != nil {
		t.Fatal(err)
	}
	if err := tree.AddGenerated("admin/install.cmds", "admin-source", []byte("cmds\n")); err != nil {
		t.Fatal(err)
	}
	if got := dirNames(t, tree, "."); !equalStrings(got, []string{"admin", "generated"}) {
		t.Fatalf("ReadDir(.) = %v, want [admin generated]", got)
	}
	if got := readTree(t, tree, "admin/install.cmds"); got != "cmds\n" {
		t.Fatalf("read = %q", got)
	}
	got, err := tree.Resolve("admin")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != OriginGenerated {
		t.Fatalf("generated parent origin = %+v", got)
	}
}

// TestAddGeneratedShadowsARegularFile is how the generated .related_dists
// menu aid takes over the stock copy the primary layer ships: the bytes
// and the origin are both replaced, with no trace of the media version
// left to serve.
func TestAddGeneratedShadowsARegularFile(t *testing.T) {
	dir := t.TempDir()
	img := makeImage(t, dir, "tools.image", map[string]string{"dist/.related_dists": "CD\n"})
	tree := build(t, []SetSpec{{Name: "6.5.30", Layers: []LayerSpec{distLayer("tools", img)}}})

	if got := readTree(t, tree, "6.5.30/dist/.related_dists"); got != "CD\n" {
		t.Fatalf("stock content = %q, want the media copy", got)
	}
	generated := []byte("../../applications/dist\n")
	if err := tree.AddGenerated("6.5.30/dist/.related_dists", "related_dists", generated); err != nil {
		t.Fatal(err)
	}
	if got := readTree(t, tree, "6.5.30/dist/.related_dists"); got != string(generated) {
		t.Fatalf("read = %q, want the generated copy %q", got, generated)
	}
	got, err := tree.Resolve("6.5.30/dist/.related_dists")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != OriginGenerated || got.Source != "related_dists" {
		t.Fatalf("Resolve = %+v, want a generated origin", got)
	}
	if got := dirNames(t, tree, "6.5.30/dist"); !equalStrings(got, []string{".related_dists"}) {
		t.Fatalf("ReadDir = %v: shadowing must not add an entry", got)
	}
}

// TestAddGeneratedRejectsDirectories: shadowing a file is deliberate,
// burying a whole directory of served media under one generated file is
// not.
func TestAddGeneratedRejectsDirectories(t *testing.T) {
	dir := t.TempDir()
	img := makeImage(t, dir, "tools.image", map[string]string{"dist/sa": "SA"})
	tree := build(t, []SetSpec{{Name: "6.5.30", Layers: []LayerSpec{distLayer("tools", img)}}})

	if err := tree.AddGenerated("6.5.30/dist", "test", []byte("x")); err == nil {
		t.Fatal("AddGenerated replaced a directory")
	}
	if err := tree.AddGenerated(".", "test", []byte("x")); err == nil {
		t.Fatal("AddGenerated replaced the tree root")
	}
	if err := tree.AddGenerated("/6.5.30/x", "test", []byte("x")); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("AddGenerated on an invalid name: %v, want fs.ErrInvalid", err)
	}
	// A file under a path whose parent is a regular file has nowhere to go.
	if err := tree.AddGenerated("6.5.30/dist/sa/deeper", "test", []byte("x")); err == nil {
		t.Fatal("AddGenerated nested a file under a regular file")
	}
}
