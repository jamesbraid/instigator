package nfsexport_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jamesbraid/instigator/efs/efstest"
	"github.com/jamesbraid/instigator/internal/nfsexport"
	"github.com/jamesbraid/instigator/internal/source"
	"github.com/jamesbraid/instigator/internal/vfs"
)

// TestNodeModeCarriesSpecialBits: an EFS file that carries setuid,
// setgid, or sticky must report it through nfs.Node.Mode alongside the
// type bits - a miniroot that mounts the distribution over NFS applies
// what the attributes say, and a mode masked to the low nine bits would
// quietly serve /usr/bin/su without its setuid.
func TestNodeModeCarriesSpecialBits(t *testing.T) {
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

	fsys := nfsexport.Export(tree)
	root, err := fsys.MountRoot("6.5.30")
	if err != nil {
		t.Fatalf("MountRoot: %v", err)
	}
	distNode := lookup(t, fsys, root, "dist")
	suNode := lookup(t, fsys, distNode, "su")

	if got := suNode.Mode() & 0o170000; got != 0o100000 {
		t.Fatalf("su.Mode() type bits = %#o, want regular (%#o)", got, uint32(0o100000))
	}
	if got := suNode.Mode() & 0o7777; got != 0o4711 {
		t.Errorf("su.Mode()&0o7777 = %#o, want %#o", got, uint32(0o4711))
	}
}
