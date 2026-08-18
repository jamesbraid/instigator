package vfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// realMediaDir holds the real IRIX 6.5.30 CD images used to build the
// applications install set for this audit: Applications.image and
// Complementary_Applications.image, August 2006. The tests below are
// skipped when it is absent, so CI (which has no SGI media) stays green
// while a developer with the media gets a check against the reviewed
// report in tmp/research/install-set-collision-audit-2026-08-16.md.
const realMediaDir = "/storage/software/os/irix/Irix 6.5.30_cdimages"

// The two paths the reviewed audit found differing between the
// Applications and Complementary Applications distributions. The audit
// also lists a third shared path, .iscd, as byte-identical (true of
// Foundation 1/2 in the same report) - but neither local image actually
// carries a .iscd anywhere, checked exhaustively with `instigator dump`
// over both discs' full trees. That claim does not hold for this local
// media, so this audit does not assert a merged .iscd; it only tests the
// two collisions that are actually present.
const (
	realAppsInstREADME = "applications/dist/inst.README"
	realAppsSwmgr      = "applications/dist/swmgr.README.html"
)

// applicationsSpec builds the SetSpec for the reviewed two-image
// applications profile: the primary Applications disc supplies the whole
// set root (stand, dist, everything), Complementary Applications
// contributes only its dist directory.
func applicationsSpec(dir string, collisions map[string]string) SetSpec {
	return SetSpec{
		Name: "applications",
		Layers: []LayerSpec{
			{Name: "applications", Image: filepath.Join(dir, "Applications.image"), SourceDir: ".", TargetDir: "."},
			{Name: "complementary", Image: filepath.Join(dir, "Complementary_Applications.image"), SourceDir: "dist", TargetDir: "dist"},
		},
		Collisions: collisions,
	}
}

// TestRealMediaApplicationsCollisionFailsWithoutOverride is the
// fail-closed half of the reviewed applications audit: Applications and
// Complementary Applications August 2006 both carry inst.README and
// swmgr.README.html, with different bytes, so Build must refuse to guess
// a winner rather than silently pick one.
func TestRealMediaApplicationsCollisionFailsWithoutOverride(t *testing.T) {
	if _, err := os.Stat(realMediaDir); err != nil {
		t.Skip("real IRIX 6.5.30 media not present")
	}
	_, err := Build([]SetSpec{applicationsSpec(realMediaDir, nil)})
	if err == nil {
		t.Fatal("Build succeeded over colliding real media; want a differing-collision error")
	}
	if !strings.Contains(err.Error(), realAppsInstREADME) && !strings.Contains(err.Error(), realAppsSwmgr) {
		t.Fatalf("error %q mentions neither %s nor %s", err, realAppsInstREADME, realAppsSwmgr)
	}
	t.Logf("Build correctly refused: %v", err)
}

// TestRealMediaApplicationsCollisionOverrideResolves is the override
// half: with the reviewed winner - the primary Applications layer - named
// for both differing paths, Build succeeds and both resolve to it. It
// also times the successful Build over the two real images, which do
// byte-for-byte comparison on every colliding regular file (here, just
// the two overridden ones - an override suppresses the compare at its
// own path), so a slow byte-compare on real overlapping media would show
// up here rather than as a surprise startup delay in the field.
func TestRealMediaApplicationsCollisionOverrideResolves(t *testing.T) {
	if _, err := os.Stat(realMediaDir); err != nil {
		t.Skip("real IRIX 6.5.30 media not present")
	}
	spec := applicationsSpec(realMediaDir, map[string]string{
		realAppsInstREADME: "applications",
		realAppsSwmgr:      "applications",
	})

	start := time.Now()
	tree, err := Build([]SetSpec{spec})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer tree.Close()
	t.Logf("Build of the applications set (2 real images, override on both differing paths) took %s", elapsed)

	for _, p := range []string{realAppsInstREADME, realAppsSwmgr} {
		origin, err := tree.Resolve(p)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", p, err)
		}
		if origin.Source != "applications" {
			t.Errorf("Resolve(%s).Source = %q, want %q", p, origin.Source, "applications")
		}
	}
}

// realBaseMediaDir holds the IRIX 6.5 base ISO set the foundations and
// development sets are assembled from - the 2004 pressing, alongside the
// 2006 6.5.30 overlays in realMediaDir. Like realMediaDir it is only
// present on a developer's machine with the media.
const realBaseMediaDir = "/storage/software/os/irix/irix_6.5base_iso"

// fullProfileSets is the whole four-set profile a Fuel install actually
// browses: the 6.5.30 overlays, the 6.5 foundations with ONC3/NFS folded
// in, the applications pair, and the development discs. Ten real images
// across two pressings, which is the scale the collision and merge policy
// has to hold at - the applications-only audit above exercises two.
//
// Two layer shapes repeat. A set's first layer maps the whole disc root
// onto the set root, so stand/ and the disc's top level come along; every
// later layer contributes only its dist. The ONC3/NFS disc is the
// exception the reviewed profile already names: its 6.5 products live in
// dist6.5 behind a dist/.redirect, so it is rebased dist6.5 -> dist and
// its .redirect never reaches the set.
func fullProfileSets() []SetSpec {
	return []SetSpec{
		{Name: "6.5.30", Layers: []LayerSpec{
			{Name: "overlays1", Image: filepath.Join(realMediaDir, "Instalation_Tools_and_Overlays1.image"), SourceDir: ".", TargetDir: "."},
			{Name: "overlays2", Image: filepath.Join(realMediaDir, "Overlays2.image"), SourceDir: "dist", TargetDir: "dist"},
			{Name: "overlays3", Image: filepath.Join(realMediaDir, "Overlays3.image"), SourceDir: "dist", TargetDir: "dist"},
		}},
		{Name: "foundations", Layers: []LayerSpec{
			{Name: "foundation1", Image: filepath.Join(realBaseMediaDir, "irix6.5_foundation1.iso"), SourceDir: ".", TargetDir: "."},
			{Name: "foundation2", Image: filepath.Join(realBaseMediaDir, "irix6.5_foundation2.iso"), SourceDir: "dist", TargetDir: "dist"},
			{Name: "nfs", Image: filepath.Join(realBaseMediaDir, "irix6.5_nfs.iso"), SourceDir: "dist6.5", TargetDir: "dist"},
		}},
		applicationsSpec(realMediaDir, map[string]string{
			realAppsInstREADME: "applications",
			realAppsSwmgr:      "applications",
		}),
		{Name: "development", Layers: []LayerSpec{
			{Name: "devfoundation", Image: filepath.Join(realBaseMediaDir, "irix6.5_devfoundation.iso"), SourceDir: ".", TargetDir: "."},
			{Name: "devlibs", Image: filepath.Join(realBaseMediaDir, "irix6.5_devlibs.iso"), SourceDir: "dist", TargetDir: "dist"},
		}},
	}
}

// TestRealMediaFullProfileBuild asks the question the applications audit
// could not: does the policy hold across the whole profile, or only over
// the one pair of discs it was written from? It builds all four sets from
// both pressings at once and checks a file from each layer shape resolves
// to the layer that really delivered it.
//
// Two results are worth the run on their own. The only override the whole
// profile needs is the reviewed applications pair - the other eight images
// merge with no configured winner, so every path two of them share is
// byte-identical. And no symlink anywhere on the ten discs fails the
// fail-closed rule vfs now enforces, so the media carry no directory link
// and no link out of their own image.
//
// The logged time is the server's cold-start cost: Build walks every
// directory on all ten images before it serves anything, which on cold
// media here is over twenty seconds and on warm media a few milliseconds.
func TestRealMediaFullProfileBuild(t *testing.T) {
	for _, dir := range []string{realMediaDir, realBaseMediaDir} {
		if _, err := os.Stat(dir); err != nil {
			t.Skipf("real IRIX media not present: %s", dir)
		}
	}

	start := time.Now()
	tree, err := Build(fullProfileSets())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Build of the four-set profile: %v", err)
	}
	defer tree.Close()
	t.Logf("Build of the four-set profile (10 real images) took %s, %d files", elapsed, len(treePaths(t, tree)))

	for _, tc := range []struct{ path, layer string }{
		// Set root from the first layer, and the same layer's dist.
		{"6.5.30/stand/fx.64", "overlays1"},
		{"6.5.30/dist/sa", "overlays1"},
		// The overridden applications collision, both halves.
		{realAppsInstREADME, "applications"},
		{realAppsSwmgr, "applications"},
		// A foundation product, and one from the rebased ONC3/NFS disc
		// landing beside it in the same dist.
		{"foundations/dist/eoe.sw", "foundation1"},
		{"foundations/dist/nfs.sw", "nfs"},
		// A development product from the later layer.
		{"development/dist/ViewKit_dev.sw", "devlibs"},
	} {
		origin, err := tree.Resolve(tc.path)
		if err != nil {
			t.Errorf("Resolve(%s): %v", tc.path, err)
			continue
		}
		if origin.Source != tc.layer {
			t.Errorf("Resolve(%s).Source = %q, want %q", tc.path, origin.Source, tc.layer)
		}
	}

	// Finding for the hardware run, asserted rather than left in prose.
	// The development foundation disc has the same shape as the ONC3/NFS
	// one - products in dist/dist6.5 behind a dist/.redirect - but the
	// reviewed profile maps it whole-root, so the compiler products land
	// a directory deeper than inst expects and the .redirect is served at
	// the set's own dist. Serving it there is what the nfs rebase exists
	// to avoid: inst reading it jumps to dist6.5 and stops seeing the
	// devlibs products merged alongside.
	for _, tc := range []struct{ path, layer string }{
		{"development/dist/dist6.5/WorkShop.sw", "devfoundation"},
		{"development/dist/.redirect", "devfoundation"},
	} {
		origin, err := tree.Resolve(tc.path)
		if err != nil {
			t.Errorf("Resolve(%s): %v", tc.path, err)
			continue
		}
		if origin.Source != tc.layer {
			t.Errorf("Resolve(%s).Source = %q, want %q", tc.path, origin.Source, tc.layer)
		}
	}
	t.Log("development: devfoundation needs the dist6.5 -> dist rebase the nfs disc gets; " +
		"as mapped whole-root its products sit at development/dist/dist6.5 and its .redirect " +
		"is served at development/dist")
}
