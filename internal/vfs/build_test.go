package vfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamesbraid/instigator/efs/efstest"
)

func TestBuildRootListsOnlySetNames(t *testing.T) {
	dir := t.TempDir()
	img := makeImage(t, dir, "media.iso", map[string]string{"dist/prod.sw": "P"})

	tree := build(t, []SetSpec{
		{Name: "6.5.30", Layers: []LayerSpec{distLayer("overlays", img)}},
		{Name: "applications", Layers: []LayerSpec{distLayer("apps", img)}},
		{Name: "development", Layers: []LayerSpec{distLayer("dev", img)}},
		{Name: "foundations", Layers: []LayerSpec{distLayer("found", img)}},
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

// TestBuildMergesDisjointLayers merges an image boot layer (its dist plus
// its stand) with a directory layer contributing only its dist, and checks
// both origins report the source and the source-relative path they came from.
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
			bootLayer("tools", img),
			{Name: "foundations", Source: extracted},
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

// TestBuildDifferingCollisionFails: two layers disagreeing about a file's
// bytes with no configured winner must stop the build, naming the path
// and both layers, rather than serving whichever the merge happened to
// reach first.
func TestBuildDifferingCollisionFails(t *testing.T) {
	dir := t.TempDir()
	first := makeImage(t, dir, "a.iso", map[string]string{"dist/inst.README": "old text"})
	second := makeImage(t, dir, "b.iso", map[string]string{"dist/inst.README": "new text"})

	_, err := Build([]SetSpec{{
		Name:   "applications",
		Layers: []LayerSpec{distLayer("base", first), distLayer("apps", second)},
	}}, localResolver{})
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
	}}, localResolver{})
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
	}}, localResolver{})
	if err == nil {
		t.Fatal("Build accepted a file/directory collision")
	}
	if !strings.Contains(err.Error(), "6.5.30/dist/sa") {
		t.Errorf("error %q does not name the path", err)
	}
}

// TestBuildServesStandOnlyForTheBootLayer: the PROM fetches
// /<set>/stand/fx.64 before anything else exists, so exactly one layer
// per set has its stand served. Every other set is dist and nothing else
// - inst runs from the miniroot and never reads stand, so serving a
// second copy of it would only be another path to keep merged.
func TestBuildServesStandOnlyForTheBootLayer(t *testing.T) {
	dir := t.TempDir()
	tools := makeImage(t, dir, "tools.image", map[string]string{
		"stand/fx.64": "FX",
		"dist/sa":     "SA",
	})
	apps := makeImage(t, dir, "apps.image", map[string]string{
		"stand/fx.64":  "OTHER FX",
		"dist/apps.sw": "A",
	})

	tree := build(t, []SetSpec{
		{Name: "6.5.30", Layers: []LayerSpec{bootLayer("tools", tools)}},
		{Name: "applications", Layers: []LayerSpec{distLayer("apps", apps)}},
	})

	if got := readTree(t, tree, "6.5.30/stand/fx.64"); got != "FX" {
		t.Fatalf("boot artifact = %q, want FX", got)
	}
	if got := dirNames(t, tree, "6.5.30"); !equalStrings(got, []string{"dist", "stand"}) {
		t.Fatalf("ReadDir(6.5.30) = %v, want [dist stand]", got)
	}
	// The applications media carry a stand too; without the boot flag it
	// stays on the disc.
	if got := dirNames(t, tree, "applications"); !equalStrings(got, []string{"dist"}) {
		t.Fatalf("ReadDir(applications) = %v, want [dist]: only a boot layer serves stand", got)
	}
	if _, err := tree.Open("applications/stand/fx.64"); err == nil {
		t.Fatal("a non-boot layer served its stand")
	}
}

// TestBuildRejectsABootLayerWithNoStand: a set configured to boot from
// media that carry no stand would serve a dist and quietly no boot
// artifact, and the operator would find out at the PROM. Name the layer
// instead.
func TestBuildRejectsABootLayerWithNoStand(t *testing.T) {
	dir := t.TempDir()
	img := makeImage(t, dir, "nostand.image", map[string]string{"dist/sa": "SA"})
	extracted := makeDir(t, filepath.Join(dir, "found"), map[string]string{"dist/foundation.sw": "F"})

	for _, tc := range []struct {
		name  string
		layer LayerSpec
	}{
		{"image", LayerSpec{Name: "tools", Source: img, Boot: true}},
		{"directory", LayerSpec{Name: "found", Source: extracted, Boot: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Build([]SetSpec{{Name: "6.5.30", Layers: []LayerSpec{tc.layer}}}, localResolver{})
			if err == nil {
				t.Fatal("Build accepted a boot layer whose source has no stand")
			}
			for _, want := range []string{"6.5.30", tc.layer.Name, "stand"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

// TestBuildRebasesDist65WithoutLeakingRedirect: inst reads dist/.redirect
// first when it opens a distribution, so a version-stub disc's root-level
// .redirect stub leaking into the set's dist would send inst into an
// unresolvable reopen loop. Mapping only the dist6.5 subtree onto dist
// excludes the stub structurally.
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
			{Name: "nfs", Source: stub, Dist: "dist6.5"},
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

// TestBuildRebasesLayerUnderBase: a source whose install tree sits below a
// subdirectory names that subdirectory as its Base, and the layer's dist is
// read from <base>/dist. Here a directory layer laid out as
// extracted/foundations/dist/foundation.sw with Base "foundations" lands on
// the set's dist exactly as a root-level layer would, so an archive that
// unpacks under a top directory needs no repacking. The resolved origin path
// is the real in-source path, so it carries the base prefix - which is also
// what would break if base were ignored: the dist would resolve at the source
// root, find nothing, and the read would fail before ever reaching here.
func TestBuildRebasesLayerUnderBase(t *testing.T) {
	dir := t.TempDir()
	root := makeDir(t, filepath.Join(dir, "extracted"), map[string]string{
		"foundations/dist/foundation.sw": "F",
	})

	tree := build(t, []SetSpec{{
		Name:   "foundations",
		Layers: []LayerSpec{{Name: "found", Source: root, Base: "foundations"}},
	}})

	if got := readTree(t, tree, "foundations/dist/foundation.sw"); got != "F" {
		t.Fatalf("read = %q, want F (dist resolved under base foundations/dist)", got)
	}
	got, err := tree.Resolve("foundations/dist/foundation.sw")
	if err != nil {
		t.Fatal(err)
	}
	want := Origin{Kind: OriginDirectory, Source: "found", Path: "foundations/dist/foundation.sw"}
	if got != want {
		t.Fatalf("Resolve = %+v, want %+v", got, want)
	}
}

// TestBuildFollowsSymlinksWithinTheSource: an EFS symlink to a regular
// file in its own image materializes as that file, which is the only
// symlink shape the tree serves.
func TestBuildFollowsSymlinksWithinTheSource(t *testing.T) {
	dir := t.TempDir()

	img := efstest.New()
	sa := img.AddFile(0o644, []byte("SA"))
	inside := img.AddSymlink("sa")
	dist := img.AddDir(map[string]uint32{"sa": sa, "inside": inside})
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
		t.Fatalf("ReadDir = %v, want [inside sa]", got)
	}
}

// TestBuildRejectsUnresolvableLinksInImageLayers: a link aimed outside
// its own image resolves to nothing - Lookup never leaves the image's
// inode graph - so no host path can be served through it. Dropping the
// entry silently would still hand the operator an install set missing a
// file the media carries, so the build fails and names the link.
func TestBuildRejectsUnresolvableLinksInImageLayers(t *testing.T) {
	dir := t.TempDir()

	img := efstest.New()
	sa := img.AddFile(0o644, []byte("SA"))
	outside := img.AddSymlink("../../../../etc/passwd")
	dist := img.AddDir(map[string]uint32{"sa": sa, "outside": outside})
	img.SetRoot(map[string]uint32{"dist": dist})
	image := writeCD(t, dir, "escape.iso", img)

	_, err := Build([]SetSpec{{
		Name:   "6.5.30",
		Layers: []LayerSpec{distLayer("links", image)},
	}}, localResolver{})
	if err == nil {
		t.Fatal("Build accepted a layer whose symlink resolves to nothing")
	}
	for _, want := range []string{"links", "dist/outside"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestBuildRejectsDirectoryLinksInImageLayers: a link to a directory
// would duplicate a subtree and could cycle, so the tree does not follow
// one. Traversing it is deferred work, not a silent drop: the build stops
// and says so.
func TestBuildRejectsDirectoryLinksInImageLayers(t *testing.T) {
	dir := t.TempDir()

	img := efstest.New()
	sa := img.AddFile(0o644, []byte("SA"))
	sub := img.AddDir(map[string]uint32{"sa": sa})
	dirlink := img.AddSymlink("sub")
	dist := img.AddDir(map[string]uint32{"sub": sub, "dirlink": dirlink})
	img.SetRoot(map[string]uint32{"dist": dist})
	image := writeCD(t, dir, "dirlink.iso", img)

	_, err := Build([]SetSpec{{
		Name:   "6.5.30",
		Layers: []LayerSpec{distLayer("links", image)},
	}}, localResolver{})
	if err == nil {
		t.Fatal("Build accepted a layer whose symlink resolves to a directory")
	}
	for _, want := range []string{"links", "dist/dirlink", "directory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestBuildRejectsSpecialFileLinksInImageLayers: a link is followed only
// to a regular file, so one aimed at a device node or a named pipe stops
// the build like a directory link does. Nothing but a directory and a
// regular file is materializable, and dropping the entry would be the
// same quiet lie.
func TestBuildRejectsSpecialFileLinksInImageLayers(t *testing.T) {
	dir := t.TempDir()

	img := efstest.New()
	sa := img.AddFile(0o644, []byte("SA"))
	fifo := img.AddFIFO()
	fifolink := img.AddSymlink("pipe")
	dist := img.AddDir(map[string]uint32{"sa": sa, "pipe": fifo, "fifolink": fifolink})
	img.SetRoot(map[string]uint32{"dist": dist})
	image := writeCD(t, dir, "fifolink.iso", img)

	_, err := Build([]SetSpec{{
		Name:   "6.5.30",
		Layers: []LayerSpec{distLayer("links", image)},
	}}, localResolver{})
	if err == nil {
		t.Fatal("Build accepted a layer whose symlink resolves to a named pipe")
	}
	for _, want := range []string{"links", "dist/fifolink"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}

	// The pipe reached directly, with no link aimed at it, is skipped as
	// it always was. Refusing to start over a device node a disc happens
	// to carry is not this rule's job.
	plain := efstest.New()
	psa := plain.AddFile(0o644, []byte("SA"))
	pdist := plain.AddDir(map[string]uint32{"sa": psa, "pipe": plain.AddFIFO()})
	plain.SetRoot(map[string]uint32{"dist": pdist})

	tree := build(t, []SetSpec{{
		Name:   "6.5.30",
		Layers: []LayerSpec{distLayer("links", writeCD(t, dir, "fifo.iso", plain))},
	}})
	if got := dirNames(t, tree, "6.5.30/dist"); !equalStrings(got, []string{"sa"}) {
		t.Fatalf("ReadDir = %v, want [sa]: a bare special file is skipped, not served", got)
	}
}

// TestBuildFollowsLinksWithinDirectoryLayers is the directory-layer half
// of containment. A pre-extracted layer sits on the host filesystem, so
// it is opened under os.OpenRoot: a link that stays inside and lands on a
// regular file resolves and is served.
func TestBuildFollowsLinksWithinDirectoryLayers(t *testing.T) {
	dir := t.TempDir()
	layer := makeDir(t, filepath.Join(dir, "found"), map[string]string{"dist/foundation.sw": "F"})
	if err := os.Symlink("foundation.sw", filepath.Join(layer, "dist", "inside")); err != nil {
		t.Fatal(err)
	}

	tree := build(t, []SetSpec{{
		Name:   "foundations",
		Layers: []LayerSpec{{Name: "found", Source: layer}},
	}})

	want := []string{"foundation.sw", "inside"}
	if got := dirNames(t, tree, "foundations/dist"); !equalStrings(got, want) {
		t.Fatalf("ReadDir = %v, want %v", got, want)
	}
	if got := readTree(t, tree, "foundations/dist/inside"); got != "F" {
		t.Fatalf("in-layer symlink = %q, want F", got)
	}
}

// TestBuildDirectoryLayerSymlinkOriginIsTheLink guards a subtle invariant:
// a directory layer's symlinked file keeps the link's own path as its
// origin, not the target's, because os.Root.FS().Stat follows a trailing
// symlink itself - resolveLink's first fs.Stat already returns the
// target's info without renaming the path being resolved. An EFS image's
// fs view leaves symlinks unfollowed, so its loop does rename the path.
func TestBuildDirectoryLayerSymlinkOriginIsTheLink(t *testing.T) {
	dir := t.TempDir()
	layer := makeDir(t, filepath.Join(dir, "found"), map[string]string{"dist/foundation.sw": "F"})
	if err := os.Symlink("foundation.sw", filepath.Join(layer, "dist", "inside")); err != nil {
		t.Fatal(err)
	}

	tree := build(t, []SetSpec{{
		Name:   "foundations",
		Layers: []LayerSpec{{Name: "found", Source: layer}},
	}})

	got, err := tree.Resolve("foundations/dist/inside")
	if err != nil {
		t.Fatal(err)
	}
	want := Origin{Kind: OriginDirectory, Source: "found", Path: "dist/inside"}
	if got != want {
		t.Fatalf("Resolve = %+v, want %+v (the link's own path, not dist/foundation.sw)", got, want)
	}
}

// TestBuildRejectsEscapingLinksInDirectoryLayers: os.Root refuses a link
// that leaves the layer, whether relative or absolute, so no host path is
// ever served. The build then fails rather than serve a set quietly
// missing those entries.
func TestBuildRejectsEscapingLinksInDirectoryLayers(t *testing.T) {
	for _, tc := range []struct{ name, target string }{
		{"relative", "../../../../etc/passwd"},
		{"absolute", "/etc/passwd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			layer := makeDir(t, filepath.Join(dir, "found"), map[string]string{"dist/foundation.sw": "F"})
			if err := os.Symlink(tc.target, filepath.Join(layer, "dist", "escape")); err != nil {
				t.Fatal(err)
			}

			_, err := Build([]SetSpec{{
				Name:   "foundations",
				Layers: []LayerSpec{{Name: "found", Source: layer}},
			}}, localResolver{})
			if err == nil {
				t.Fatalf("Build accepted a layer whose symlink escapes to %s", tc.target)
			}
			for _, want := range []string{"found", "dist/escape"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

// TestBuildRejectsDirectoryLinksInDirectoryLayers is the directory-layer
// half of the deferred directory-link traversal: an in-bounds link to a
// directory stops the build too.
func TestBuildRejectsDirectoryLinksInDirectoryLayers(t *testing.T) {
	dir := t.TempDir()
	layer := makeDir(t, filepath.Join(dir, "found"), map[string]string{"dist/sub/foundation.sw": "F"})
	if err := os.Symlink("sub", filepath.Join(layer, "dist", "dirlink")); err != nil {
		t.Fatal(err)
	}

	_, err := Build([]SetSpec{{
		Name:   "foundations",
		Layers: []LayerSpec{{Name: "found", Source: layer}},
	}}, localResolver{})
	if err == nil {
		t.Fatal("Build accepted a layer whose symlink resolves to a directory")
	}
	for _, want := range []string{"found", "dist/dirlink", "directory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
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
	}, localResolver{}); err == nil {
		t.Error("Build accepted two sets with the same name")
	}
	if _, err := Build([]SetSpec{{
		Name:   "6.5.30",
		Layers: []LayerSpec{{Name: "a", Source: img, Dist: "absent"}},
	}}, localResolver{}); err == nil {
		t.Error("Build accepted a layer whose dist directory is not in the source")
	}
	if _, err := Build([]SetSpec{{
		Name:   "6.5.30",
		Layers: []LayerSpec{{Name: "a", Source: filepath.Join(dir, "missing.iso")}},
	}}, localResolver{}); err == nil {
		t.Error("Build accepted a layer whose image does not exist")
	}
}
