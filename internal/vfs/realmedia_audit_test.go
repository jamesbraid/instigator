package vfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tests skip when the local IRIX media is unavailable.
const realMediaDir = "/storage/software/os/irix/Irix 6.5.30_cdimages"

// These are the two differing paths present on the local applications media.
const (
	realAppsInstREADME = "applications/dist/inst.README"
	realAppsSwmgr      = "applications/dist/swmgr.README.html"
)

// applicationsSpec builds the SetSpec for the two-image applications
// profile: both discs contribute their ordinary dist, and neither boots -
// nothing netboots the applications set.
func applicationsSpec(dir string, collisions map[string]string) SetSpec {
	return SetSpec{
		Name: "applications",
		Layers: []LayerSpec{
			{Name: "applications", Source: filepath.Join(dir, "Applications.image")},
			{Name: "complementary", Source: filepath.Join(dir, "Complementary_Applications.image")},
		},
		Collisions: collisions,
	}
}

// TestRealMediaApplicationsCollisionFailsWithoutOverride: Applications and
// Complementary Applications both carry inst.README and swmgr.README.html
// with different bytes, so Build must refuse to guess a winner rather than
// silently pick one.
func TestRealMediaApplicationsCollisionFailsWithoutOverride(t *testing.T) {
	if _, err := os.Stat(realMediaDir); err != nil {
		t.Skip("real IRIX 6.5.30 media not present")
	}
	_, err := Build([]SetSpec{applicationsSpec(realMediaDir, nil)}, localResolver{})
	if err == nil {
		t.Fatal("Build succeeded over colliding real media; want a differing-collision error")
	}
	if !strings.Contains(err.Error(), realAppsInstREADME) && !strings.Contains(err.Error(), realAppsSwmgr) {
		t.Fatalf("error %q mentions neither %s nor %s", err, realAppsInstREADME, realAppsSwmgr)
	}
	t.Logf("Build correctly refused: %v", err)
}

// TestRealMediaApplicationsCollisionOverrideResolves: with the
// Applications layer named as the winner for both differing paths, Build
// succeeds and both resolve to it. It also times the Build, so a slow
// byte-compare on real overlapping media shows up here rather than as a
// surprise startup delay in the field.
func TestRealMediaApplicationsCollisionOverrideResolves(t *testing.T) {
	if _, err := os.Stat(realMediaDir); err != nil {
		t.Skip("real IRIX 6.5.30 media not present")
	}
	spec := applicationsSpec(realMediaDir, map[string]string{
		realAppsInstREADME: "applications",
		realAppsSwmgr:      "applications",
	})

	start := time.Now()
	tree, err := Build([]SetSpec{spec}, localResolver{})
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

func fullProfileSets() []SetSpec {
	return []SetSpec{
		{Name: "6.5.30", Layers: []LayerSpec{
			{Name: "overlays1", Source: filepath.Join(realMediaDir, "Instalation_Tools_and_Overlays1.image"), Boot: true},
			{Name: "overlays2", Source: filepath.Join(realMediaDir, "Overlays2.image")},
			{Name: "overlays3", Source: filepath.Join(realMediaDir, "Overlays3.image")},
		}},
		{Name: "foundations", Layers: []LayerSpec{
			{Name: "foundation1", Source: filepath.Join(realBaseMediaDir, "irix6.5_foundation1.iso")},
			{Name: "foundation2", Source: filepath.Join(realBaseMediaDir, "irix6.5_foundation2.iso")},
			{Name: "nfs", Source: filepath.Join(realBaseMediaDir, "irix6.5_nfs.iso"), Dist: "dist6.5"},
		}},
		applicationsSpec(realMediaDir, map[string]string{
			realAppsInstREADME: "applications",
			realAppsSwmgr:      "applications",
		}),
		{Name: "development", Layers: []LayerSpec{
			{Name: "devlibs", Source: filepath.Join(realBaseMediaDir, "irix6.5_devlibs.iso")},
			{Name: "devfoundation", Source: filepath.Join(realBaseMediaDir, "irix6.5_devfoundation.iso"), Dist: "dist/dist6.5"},
		}},
	}
}

// TestRealMediaFullProfileBuild builds the ten-image profile and checks
// representative origins.
func TestRealMediaFullProfileBuild(t *testing.T) {
	for _, dir := range []string{realMediaDir, realBaseMediaDir} {
		if _, err := os.Stat(dir); err != nil {
			t.Skipf("real IRIX media not present: %s", dir)
		}
	}

	start := time.Now()
	tree, err := Build(fullProfileSets(), localResolver{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Build of the four-set profile: %v", err)
	}
	defer tree.Close()
	t.Logf("Build of the four-set profile (10 real images) took %s, %d files", elapsed, len(treePaths(t, tree)))

	for _, tc := range []struct{ path, layer string }{
		// The one boot layer's stand, which is the only artifact the
		// PROM fetches, and the same layer's dist.
		{"6.5.30/stand/fx.64", "overlays1"},
		{"6.5.30/dist/sa", "overlays1"},
		// The overridden applications collision, both halves.
		{realAppsInstREADME, "applications"},
		{realAppsSwmgr, "applications"},
		// A foundation product, and one from the rebased ONC3/NFS disc
		// landing beside it in the same dist.
		{"foundations/dist/eoe.sw", "foundation1"},
		{"foundations/dist/nfs.sw", "nfs"},
		// A development product from each disc, both landing in the one
		// dist: the libraries from devlibs, the compilers from the
		// rebased development foundation disc.
		{"development/dist/ViewKit_dev.sw", "devlibs"},
		{"development/dist/WorkShop.sw", "devfoundation"},
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

	// Both rebased discs must leave their .redirect stub behind. inst
	// reading one at a set's own dist would chase the subdirectory and
	// stop seeing the products the other layers merged alongside, which
	// is the whole reason those two layers draw from dist6.5 rather than
	// dist.
	for _, p := range []string{
		"development/dist/.redirect",
		"development/dist/dist6.5",
		"foundations/dist/.redirect",
		// Only the 6.5.30 set boots, so no other set has a stand at all.
		"foundations/stand",
		"applications/stand",
		"development/stand",
	} {
		if origin, err := tree.Resolve(p); err == nil {
			t.Errorf("Resolve(%s) = %+v; the rebase should have left it behind", p, origin)
		}
	}
}

func TestRealMediaOverlayBootPackagePair(t *testing.T) {
	if _, err := os.Stat(realMediaDir); err != nil {
		t.Skip("real IRIX 6.5.30 media not present")
	}
	tree, err := Build(fullProfileSets(), localResolver{})
	if err != nil {
		t.Fatalf("Build of the four-set profile: %v", err)
	}
	defer tree.Close()

	for _, name := range []string{"eoe_6530m.idb", "eoe_6530m.sw", "eoe_6530m.sw64"} {
		path := "6.5.30/dist/" + name
		origin, err := tree.Resolve(path)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", path, err)
		}
		if origin.Source != "overlays1" {
			t.Errorf("Resolve(%s).Source = %q, want overlays1", path, origin.Source)
		}
	}
}
