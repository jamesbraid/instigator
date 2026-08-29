package serve

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jamesbraid/instigator/efs/efstest"
	"github.com/jamesbraid/instigator/internal/source"
	"github.com/jamesbraid/instigator/internal/vfs"
)

// TestCmdFSStatCarriesSpecialBits: the rsh shell's view of a file must
// keep the setuid, setgid, and sticky bits the media carries. cmdFS.Stat
// once masked the mode with Perm(), so ls over rsh showed /usr/bin/su
// stripped of the very bit the media put there.
func TestCmdFSStatCarriesSpecialBits(t *testing.T) {
	dir := t.TempDir()

	img := efstest.New()
	su := img.AddFile(0o4711, []byte("SU"))
	dist := img.AddDir(map[string]uint32{"su": su})
	img.SetRoot(map[string]uint32{"dist": dist})
	imgPath := filepath.Join(dir, "modes.iso")
	if err := os.WriteFile(imgPath, img.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}

	tree, err := vfs.Build([]vfs.SetSpec{{
		Name:   "6.5.30",
		Layers: []vfs.LayerSpec{{Name: "media", Source: imgPath}},
	}}, source.New(source.Options{CacheDir: t.TempDir()}))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { tree.Close() })

	info, err := cmdFS{tree}.Stat("/6.5.30/dist/su")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Perm & 0o7777; got != 0o4711 {
		t.Errorf("Stat(/6.5.30/dist/su).Perm = %#o, want %#o", got, uint32(0o4711))
	}
}
