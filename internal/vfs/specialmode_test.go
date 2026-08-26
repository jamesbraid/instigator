package vfs

import (
	"testing"

	"github.com/jamesbraid/instigator/efs/efstest"
)

// TestBuildPreservesSpecialModeBitsFromImage: an EFS regular file that
// carries a setuid, setgid, or sticky bit must serve with that bit
// intact, exactly as it sits on the media. io/fs.FileInfo.Mode() reports
// only the low nine permission bits, so the walker recovers the full
// mode from the source's Sys() (*efs.Stat) and the served node keeps all
// twelve bits. A directory layer has no Sys() owner and never carried
// these bits, so it is unaffected and stays at its ordinary perm.
func TestBuildPreservesSpecialModeBitsFromImage(t *testing.T) {
	dir := t.TempDir()

	img := efstest.New()
	setgid := img.AddFile(0o2755, []byte("SA"))  // setgid
	setuid := img.AddFile(0o4711, []byte("SU"))  // setuid
	sticky := img.AddFile(0o1644, []byte("TMP")) // sticky
	dist := img.AddDir(map[string]uint32{"sa": setgid, "su": setuid, "tmp": sticky})
	img.SetRoot(map[string]uint32{"dist": dist})
	image := writeCD(t, dir, "modes.iso", img)

	tree := build(t, []SetSpec{{
		Name:   "6.5.30",
		Layers: []LayerSpec{distLayer("tools", image)},
	}})

	for _, tc := range []struct {
		path string
		want uint16
	}{
		{"6.5.30/dist/sa", 0o2755},
		{"6.5.30/dist/su", 0o4711},
		{"6.5.30/dist/tmp", 0o1644},
	} {
		info, err := tree.Stat(tc.path)
		if err != nil {
			t.Fatalf("Stat(%s): %v", tc.path, err)
		}
		if got := uint16(info.Mode()) & 0o7777; got != tc.want {
			t.Errorf("Stat(%s).Mode()&0o7777 = %#o, want %#o", tc.path, got, tc.want)
		}
	}
}
