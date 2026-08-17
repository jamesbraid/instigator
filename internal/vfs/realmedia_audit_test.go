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
