package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jamesbraid/instigator/internal/extract"
)

// Real IRIX media, never committed. The test skips unless every image is
// present, so CI stays green.
const (
	media6530 = "/storage/software/os/irix/Irix 6.5.30_cdimages"
	media65   = "/storage/software/os/irix/irix_6.5base_iso"
)

// TestExtractRealMedia extracts, with --unpack, the 6.5.30 discs installed
// after the 6.5 base discs. The base documentation disc is ISO 9660, not an
// inst distribution, and the base installation tools disc adds only
// installer products, so neither is part of the installed view. The 6.5.30
// discs ship two differing copies of two top-level files; the overlays
// disc's win.
func TestExtractRealMedia(t *testing.T) {
	layers := func(dir string, ls ...string) string {
		y := "    layers:\n"
		for i := 0; i < len(ls); i += 3 {
			p := filepath.Join(dir, ls[i+1])
			if _, err := os.Stat(p); err != nil {
				t.Skip("real IRIX media not present")
			}
			y += fmt.Sprintf("      - {name: %s, source: %q%s}\n", ls[i], p, ls[i+2])
		}
		return y
	}
	cfg := "install_sets:\n  - name: set\n    after: [\"6.5\"]\n" +
		"    collisions: {set/dist/inst.README: overlays1, set/dist/swmgr.README.html: overlays1}\n" +
		layers(media6530, "overlays1", "Instalation_Tools_and_Overlays1.image", "", "overlays2", "Overlays2.image", "",
			"overlays3", "Overlays3.image", "", "apps", "Applications.image", "", "capps", "Complementary_Applications.image", "") +
		"  - name: \"6.5\"\n" +
		layers(media65, "foundation1", "irix6.5_foundation1.iso", "", "foundation2", "irix6.5_foundation2.iso", "",
			"devfoundation", "irix6.5_devfoundation.iso", ", dist: dist/dist6.5", "devlibs", "irix6.5_devlibs.iso", "",
			"apps65", "irix6.5_apps.iso", "", "nfs", "irix6.5_nfs.iso", ", dist: dist/dist6.5")
	if testing.Short() {
		t.Skip("real-media extraction skipped in -short mode")
	}
	dir := t.TempDir()
	conf := writeConfig(t, dir, cfg)
	check := func(out string) map[string][]manifestLine {
		ms := readManifest(t, filepath.Join(out, "set.jsonl"))
		for _, recs := range ms {
			for _, m := range recs {
				if m.Status == "error" || m.Status == "unparsed" || m.Status == "superseded" && m.Set != "6.5" {
					t.Errorf("%s entry %s (%s, set %s): %s", m.Status, m.InstallPath, m.Product, m.Set, m.Error)
				}
			}
		}
		return ms
	}

	// A manifest-only run of every manual page reads no archive.
	out := filepath.Join(dir, "plan")
	if err := extractCommand(io.Discard, []string{conf, out}, []string{"set"}, "", "",
		extract.Options{ManifestOnly: true, Subsystems: []string{"*.man.*"}}); err != nil {
		t.Fatal(err)
	}
	for _, recs := range check(out) {
		if recs[0].Subsystem != "" && !strings.Contains(recs[0].Subsystem, ".man.") {
			t.Errorf("%s: subsystem %s", recs[0].InstallPath, recs[0].Subsystem)
		}
	}

	out = filepath.Join(dir, "out")
	if err := extractCommand(io.Discard, []string{conf, out}, []string{"set"}, "", "", extract.Options{Unpack: true,
		Paths: []string{"usr/include", "var/sysgen", "usr/share/catman", "usr/adm", "usr/spool", "usr/lib/cron",
			"var/adm", "var/spool", "var/tmp", "etc/cron.d"}}); err != nil {
		t.Fatal(err)
	}
	ms := check(out)
	root := filepath.Join(out, "set")

	// A link may dangle when its target lies outside the extracted paths
	// or is on none of these discs, never when the target was written,
	// unpacked or not.
	written := map[string]bool{}
	for _, recs := range ms {
		for _, m := range recs {
			written[m.Path] = true
		}
	}
	links := 0
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}
		links++
		if _, err := os.Stat(p); err != nil {
			rel, _ := filepath.Rel(root, p)
			target, _ := os.Readlink(p)
			if dest := path.Join(path.Dir(filepath.ToSlash(rel)), target); written[dest] || written[strings.TrimSuffix(strings.TrimSuffix(dest, ".z"), ".Z")] {
				t.Errorf("link %s does not resolve to %s, which was written", rel, dest)
			}
		}
		return nil
	})
	if links == 0 {
		t.Error("no links written")
	}

	// The digests were computed by an independent extractor. The base eoe
	// and the overlays' eoe_6530m both install acct with different bytes;
	// the installed view holds the overlay's.
	for p, want := range map[string]string{
		"usr/include/ckpt.h":                    "e873148954527c54c7b6a6e2a4c1c7b166a21019cfdd02f6a8ab71d39bd1b8a2",
		"usr/include/openssl/aes.h":             "c81c280b66ae428205020ff7aabb7fd4648947f13a2c9043ad3e16aed7ac195d",
		"usr/include/dmedia/audio.h":            "2f5d15ad56d82a9da4dd92cd22d80b2af40604e95254e4f3af9a2869fcf243e8",
		"usr/include/GL/fgl.h":                  "d3f30327ddcea094a95b79f723afe4fa18b03101b811debc063e402c364c0e84",
		"usr/include/geotiff/cpl_serv.h":        "c00f6b7f46a19da08309afb6dca7e0054ed089ae6fce188457bcf6748c3fa708",
		"var/sysgen/stune":                      "6ac40276a8c9a86ef8eb89ed4e9e6932865c7f00595a1352656de1a2be56f629",
		"usr/share/catman/u_man/man1/backend.1": "b3cf5803655377b76cd82de65d417583fe67dce54e9e5a533db33e3c29e49eb7",
		"usr/share/catman/a_man/cat1/acct":      "9e45fae40f77b8c2452f42b8d612ae582631cb07f12a996149fb5634b062b076",
	} {
		b, err := os.ReadFile(filepath.Join(root, p))
		if sum := sha256.Sum256(b); err != nil || hex.EncodeToString(sum[:]) != want {
			t.Errorf("%s: sha256 %x, %v; want %s", p, sum, err, want)
		}
	}

	// 6.5 moved these paths under var and left links behind. eoe lists the
	// old paths again as delhist entries, and appletalk installs under
	// usr/adm, which eoe makes a link but which sorts first.
	for _, p := range []string{"usr/adm", "usr/spool", "usr/lib/cron"} {
		if fi, err := os.Lstat(filepath.Join(root, p)); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			t.Errorf("%s is not a link: %v", p, err)
		}
	}
	for _, p := range []string{"var/adm/appletalk", "var/spool/cron/crontabs/root", "etc/cron.d/at.cfg"} {
		if _, err := os.Lstat(filepath.Join(root, p)); err != nil {
			t.Error(err)
		}
	}
	if fi, err := os.Lstat(filepath.Join(root, "var/tmp")); err != nil || !fi.IsDir() {
		t.Errorf("var/tmp is not a directory: %v", err)
	}
}
