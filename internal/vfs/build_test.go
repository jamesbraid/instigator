package vfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamesbraid/instigator/efs/efstest"
)

// TestBuildRootListsOnlySetNames is Global Constraint 3: a set's contents
// are its merged layers, so nothing disc-named exists in the served
// namespace and a pristine tree's root is exactly the configured names.
func TestBuildRootListsOnlySetNames(t *testing.T) {
	dir := t.TempDir()
	img := makeImage(t, dir, "media.iso", map[string]string{"dist/prod.sw": "P"})

	tree := build(t, []SetSpec{
		{Name: "6.5.30", Layers: []LayerSpec{wholeRoot("overlays", img)}},
		{Name: "applications", Layers: []LayerSpec{wholeRoot("apps", img)}},
		{Name: "development", Layers: []LayerSpec{wholeRoot("dev", img)}},
		{Name: "foundations", Layers: []LayerSpec{wholeRoot("found", img)}},
	})

	want := []string{"6.5.30", "applications", "development", "foundations"}
	if got := dirNames(t, tree, "."); !equalStrings(got, want) {
		t.Fatalf("ReadDir(.) = %v, want %v", got, want)
	}
	// Below a set there is only merged content: no image name, no slug.
	if got := dirNames(t, tree, "6.5.30"); !equalStrings(got, []string{"dist"}) {
		t.Fatalf("ReadDir(6.5.30) = %v, want [dist]", got)
	}
	for _, p := range treePaths(t, tree) {
		if strings.Contains(p, "media") {
			t.Fatalf("served path %q carries a disc name", p)
		}
	}
}

// TestBuildMergesDisjointLayers merges an image layer mapped whole-root
// with a directory layer contributing only dist, and checks both origins
// report the source and the source-relative path they came from.
func TestBuildMergesDisjointLayers(t *testing.T) {
	dir := t.TempDir()
	img := makeImage(t, dir, "tools.image", map[string]string{
		"stand/fx.64": "FX",
		"dist/sa":     "SA",
	})
	extracted := makeDir(t, dir+"/foundations", map[string]string{"dist/foundation.sw": "F"})

	tree := build(t, []SetSpec{{
		Name: "6.5.30",
		Layers: []LayerSpec{
			wholeRoot("tools", img),
			{Name: "foundations", Dir: extracted, SourceDir: "dist", TargetDir: "dist"},
		},
	}})

	content := map[string]string{
		"6.5.30/stand/fx.64":        "FX",
		"6.5.30/dist/sa":            "SA",
		"6.5.30/dist/foundation.sw": "F",
	}
	for p, want := range content {
		if got := readTree(t, tree, p); got != want {
			t.Errorf("read %q = %q, want %q", p, got, want)
		}
	}

	origins := map[string]Origin{
		"6.5.30/dist/sa":            {Kind: OriginImage, Source: "tools", Path: "dist/sa"},
		"6.5.30/dist/foundation.sw": {Kind: OriginDirectory, Source: "foundations", Path: "dist/foundation.sw"},
	}
	for p, want := range origins {
		got, err := tree.Resolve(p)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", p, err)
		}
		if got != want {
			t.Errorf("Resolve(%q) = %+v, want %+v", p, got, want)
		}
	}
	if got := OriginImage.String(); got != "image" {
		t.Errorf("OriginImage.String() = %q", got)
	}
	if got := OriginDirectory.String(); got != "directory" {
		t.Errorf("OriginDirectory.String() = %q", got)
	}
}

// TestBuildIdenticalDuplicateKeepsEarliestLayer: the same bytes in two
// layers merge to one node whose origin is deterministically the earlier
// layer, so a manifest line never flips between restarts.
func TestBuildIdenticalDuplicateKeepsEarliestLayer(t *testing.T) {
	dir := t.TempDir()
	first := makeImage(t, dir, "a.iso", map[string]string{"dist/sa": "SA"})
	second := makeImage(t, dir, "b.iso", map[string]string{"dist/sa": "SA"})

	tree := build(t, []SetSpec{{
		Name:   "6.5.30",
		Layers: []LayerSpec{distLayer("first", first), distLayer("second", second)},
	}})

	if got := dirNames(t, tree, "6.5.30/dist"); !equalStrings(got, []string{"sa"}) {
		t.Fatalf("ReadDir(6.5.30/dist) = %v, want [sa]", got)
	}
	if got := readTree(t, tree, "6.5.30/dist/sa"); got != "SA" {
		t.Fatalf("read = %q, want SA", got)
	}
	got, err := tree.Resolve("6.5.30/dist/sa")
	if err != nil {
		t.Fatal(err)
	}
	want := Origin{Kind: OriginImage, Source: "first", Path: "dist/sa"}
	if got != want {
		t.Fatalf("Resolve = %+v, want %+v", got, want)
	}
}

// TestBuildDifferingCollisionFails is the fail-closed half of Global
// Constraint 4: two layers disagreeing about a file's bytes with no
// configured winner must stop the build, naming the path and both layers,
// rather than serving whichever the merge happened to reach first.
func TestBuildDifferingCollisionFails(t *testing.T) {
	dir := t.TempDir()
	first := makeImage(t, dir, "a.iso", map[string]string{"dist/inst.README": "old text"})
	second := makeImage(t, dir, "b.iso", map[string]string{"dist/inst.README": "new text"})

	_, err := Build([]SetSpec{{
		Name:   "applications",
		Layers: []LayerSpec{distLayer("base", first), distLayer("apps", second)},
	}})
	if err == nil {
		t.Fatal("Build accepted a differing collision with no configured winner")
	}
	for _, want := range []string{"applications/dist/inst.README", "base", "apps"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestBuildCollisionOverrideChoosesConfiguredLayer: the reviewed override
// names the exact logical path and the winning layer, and that layer's
// bytes are what serves.
func TestBuildCollisionOverrideChoosesConfiguredLayer(t *testing.T) {
	dir := t.TempDir()
	first := makeImage(t, dir, "a.iso", map[string]string{"dist/inst.README": "old text"})
	second := makeImage(t, dir, "b.iso", map[string]string{"dist/inst.README": "new text"})

	tree := build(t, []SetSpec{{
		Name:       "applications",
		Layers:     []LayerSpec{distLayer("base", first), distLayer("apps", second)},
		Collisions: map[string]string{"applications/dist/inst.README": "apps"},
	}})

	if got := readTree(t, tree, "applications/dist/inst.README"); got != "new text" {
		t.Fatalf("read = %q, want the apps layer's %q", got, "new text")
	}
	got, err := tree.Resolve("applications/dist/inst.README")
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "apps" {
		t.Fatalf("Resolve source = %q, want apps", got.Source)
	}
}

// TestBuildCollisionOverrideWinsFromEitherOrder proves the winner is the
// configured layer and not simply the last one merged: naming the earlier
// layer keeps its bytes even though a later layer differs.
func TestBuildCollisionOverrideWinsFromEitherOrder(t *testing.T) {
	dir := t.TempDir()
	first := makeImage(t, dir, "a.iso", map[string]string{"dist/inst.README": "old text"})
	second := makeImage(t, dir, "b.iso", map[string]string{"dist/inst.README": "new text"})

	tree := build(t, []SetSpec{{
		Name:       "applications",
		Layers:     []LayerSpec{distLayer("base", first), distLayer("apps", second)},
		Collisions: map[string]string{"applications/dist/inst.README": "base"},
	}})

	if got := readTree(t, tree, "applications/dist/inst.README"); got != "old text" {
		t.Fatalf("read = %q, want the base layer's %q", got, "old text")
	}
}

// TestBuildRejectsUnmatchedCollision closes the other half of the
// fail-closed rule: a configured winner skips the byte comparison, so a
// winner that never contributes that path would leave un-compared bytes
// serving under an override that did nothing.
func TestBuildRejectsUnmatchedCollision(t *testing.T) {
	dir := t.TempDir()
	first := makeImage(t, dir, "a.iso", map[string]string{"dist/inst.README": "old text"})
	second := makeImage(t, dir, "b.iso", map[string]string{"dist/inst.README": "new text"})

	_, err := Build([]SetSpec{{
		Name:       "applications",
		Layers:     []LayerSpec{distLayer("base", first), distLayer("apps", second)},
		Collisions: map[string]string{"applications/dist/inst.README": "absent"},
	}})
	if err == nil {
		t.Fatal("Build accepted a collision winner that contributes nothing")
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Errorf("error %q does not name the configured winner", err)
	}
}

// TestBuildRejectsFileDirectoryCollision: one layer's regular file landing
// where another layer has a directory is structural, not a byte
// disagreement, and no collision winner can resolve it.
func TestBuildRejectsFileDirectoryCollision(t *testing.T) {
	dir := t.TempDir()
	asFile := makeImage(t, dir, "a.iso", map[string]string{"dist/sa": "SA"})
	asDir := makeImage(t, dir, "b.iso", map[string]string{"dist/sa/inner": "I"})

	_, err := Build([]SetSpec{{
		Name:   "6.5.30",
		Layers: []LayerSpec{distLayer("file", asFile), distLayer("dir", asDir)},
	}})
	if err == nil {
		t.Fatal("Build accepted a file/directory collision")
	}
	if !strings.Contains(err.Error(), "6.5.30/dist/sa") {
		t.Errorf("error %q does not name the path", err)
	}
}

// TestBuildRebasesDist65WithoutLeakingRedirect is the redirect-loop
// regression. A version-stub disc (the onc3/nfs shape) carries a
// root-level .redirect stub beside the real catalog in dist6.5. inst reads
// dist/.redirect first when it opens a distribution, so a stub leaking
// into the set's dist sends it chasing a redirect it cannot resolve and it
// reopens forever. Mapping only the dist6.5 subtree onto the logical dist
// excludes the stub structurally - there is no filtering to get wrong.
func TestBuildRebasesDist65WithoutLeakingRedirect(t *testing.T) {
	dir := t.TempDir()
	primary := makeImage(t, dir, "tools.image", map[string]string{"dist/sa": "SA"})
	stub := makeImage(t, dir, "nfs.iso", map[string]string{
		".redirect":      "dist6.5 (TARGOS)\n",
		"dist6.5/nfs.sw": "NFS",
	})

	tree := build(t, []SetSpec{{
		Name: "foundations",
		Layers: []LayerSpec{
			distLayer("tools", primary),
			{Name: "nfs", Image: stub, SourceDir: "dist6.5", TargetDir: "dist"},
		},
	}})

	if got := readTree(t, tree, "foundations/dist/nfs.sw"); got != "NFS" {
		t.Fatalf("rebased catalog = %q, want NFS", got)
	}
	if _, err := tree.Open("foundations/dist/.redirect"); err == nil {
		t.Fatal("the stub's .redirect resolved at the set's dist: inst would loop on it")
	}
	if got := dirNames(t, tree, "foundations/dist"); !equalStrings(got, []string{"nfs.sw", "sa"}) {
		t.Fatalf("ReadDir(foundations/dist) = %v, want [nfs.sw sa]", got)
	}
	// The stub layer contributes at its real in-image path, which is what
	// a manifest line has to say.
	got, err := tree.Resolve("foundations/dist/nfs.sw")
	if err != nil {
		t.Fatal(err)
	}
	want := Origin{Kind: OriginImage, Source: "nfs", Path: "dist6.5/nfs.sw"}
	if got != want {
		t.Fatalf("Resolve = %+v, want %+v", got, want)
	}
}

// TestBuildFollowsSymlinksWithinTheSource: an EFS symlink to a file in its
// own image materializes as that file, while one aimed outside the image
// resolves to nothing and is skipped rather than serving a host path.
func TestBuildFollowsSymlinksWithinTheSource(t *testing.T) {
	dir := t.TempDir()

	img := efstest.New()
	sa := img.AddFile(0o644, []byte("SA"))
	inside := img.AddSymlink("sa")
	outside := img.AddSymlink("../../../../etc/passwd")
	dist := img.AddDir(map[string]uint32{"sa": sa, "inside": inside, "outside": outside})
	img.SetRoot(map[string]uint32{"dist": dist})
	image := writeCD(t, dir, "links.iso", img)

	tree := build(t, []SetSpec{{
		Name:   "6.5.30",
		Layers: []LayerSpec{distLayer("links", image)},
	}})

	if got := readTree(t, tree, "6.5.30/dist/inside"); got != "SA" {
		t.Fatalf("in-source symlink = %q, want SA", got)
	}
	if got := dirNames(t, tree, "6.5.30/dist"); !equalStrings(got, []string{"inside", "sa"}) {
		t.Fatalf("ReadDir = %v, want [inside sa]: the escaping link must be dropped", got)
	}
	if _, err := tree.Open("6.5.30/dist/outside"); err == nil {
		t.Fatal("a symlink pointing out of the image resolved")
	}
}

// TestBuildDropsEscapingLinksInDirectoryLayers is the directory-layer half
// of containment. A pre-extracted layer sits on the host filesystem, where
// a symlink really can reach /etc/passwd, so the layer is opened under
// os.OpenRoot: a link that stays inside resolves, one that leaves is
// refused and its entry never enters the tree.
func TestBuildDropsEscapingLinksInDirectoryLayers(t *testing.T) {
	dir := t.TempDir()
	layer := makeDir(t, filepath.Join(dir, "found"), map[string]string{"dist/foundation.sw": "F"})
	if err := os.Symlink("foundation.sw", filepath.Join(layer, "dist", "inside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../../etc/passwd", filepath.Join(layer, "dist", "outside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(layer, "dist", "absolute")); err != nil {
		t.Fatal(err)
	}

	tree := build(t, []SetSpec{{
		Name:   "foundations",
		Layers: []LayerSpec{{Name: "found", Dir: layer, SourceDir: "dist", TargetDir: "dist"}},
	}})

	want := []string{"foundation.sw", "inside"}
	if got := dirNames(t, tree, "foundations/dist"); !equalStrings(got, want) {
		t.Fatalf("ReadDir = %v, want %v: the escaping links must be dropped", got, want)
	}
	if got := readTree(t, tree, "foundations/dist/inside"); got != "F" {
		t.Fatalf("in-layer symlink = %q, want F", got)
	}
	for _, name := range []string{"foundations/dist/outside", "foundations/dist/absolute"} {
		if _, err := tree.Open(name); err == nil {
			t.Fatalf("Open(%q) served a host path through a symlink", name)
		}
	}
}

// TestBuildRejectsDuplicateAndMissingSources fails the build on the two
// configuration mistakes that would otherwise serve a half-assembled tree.
func TestBuildRejectsDuplicateAndMissingSources(t *testing.T) {
	dir := t.TempDir()
	img := makeImage(t, dir, "a.iso", map[string]string{"dist/sa": "SA"})

	if _, err := Build([]SetSpec{
		{Name: "6.5.30", Layers: []LayerSpec{distLayer("a", img)}},
		{Name: "6.5.30", Layers: []LayerSpec{distLayer("b", img)}},
	}); err == nil {
		t.Error("Build accepted two sets with the same name")
	}
	if _, err := Build([]SetSpec{{
		Name:   "6.5.30",
		Layers: []LayerSpec{{Name: "a", Image: img, SourceDir: "absent", TargetDir: "dist"}},
	}}); err == nil {
		t.Error("Build accepted a layer whose SourceDir is not in the source")
	}
	if _, err := Build([]SetSpec{{
		Name:   "6.5.30",
		Layers: []LayerSpec{{Name: "a", Image: filepath.Join(dir, "missing.iso")}},
	}}); err == nil {
		t.Error("Build accepted a layer whose image does not exist")
	}
}
