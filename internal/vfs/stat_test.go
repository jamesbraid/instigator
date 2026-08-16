package vfs

import (
	"testing"

	"github.com/jamesbraid/instigator/efs"
)

func TestTreeStatLevels(t *testing.T) {
	tree := buildTestTree(t)

	root, err := tree.Stat("")
	if err != nil {
		t.Fatal(err)
	}
	if !root.IsDir || root.Ino != uint64(efs.RootIno) {
		t.Fatalf("root stat = %+v, want a directory with ino %d", root, efs.RootIno)
	}

	media, err := tree.Stat("6.5.30")
	if err != nil {
		t.Fatal(err)
	}
	if !media.IsDir {
		t.Fatalf("media-level stat = %+v, want a directory", media)
	}

	disc, err := tree.Stat("6.5.30/irix-6-5-30-overlay-1of3")
	if err != nil {
		t.Fatal(err)
	}
	if !disc.IsDir {
		t.Fatalf("disc-level stat = %+v, want a directory", disc)
	}

	file, err := tree.Stat("6.5.30/irix-6-5-30-overlay-1of3/stand/fx.64")
	if err != nil {
		t.Fatal(err)
	}
	if file.IsDir {
		t.Fatal("fx.64 reported as a directory")
	}
	if want := int64(len("overlay one fx")); file.Size != want {
		t.Fatalf("fx.64 size = %d, want %d", file.Size, want)
	}
	if file.Ino == 0 {
		t.Fatal("fx.64 inode is zero")
	}
	if file.Ino == root.Ino {
		t.Fatal("fx.64 shares the tree root's synthetic inode")
	}
	if file.Nlink == 0 {
		t.Fatal("fx.64 nlink is zero")
	}
}

func TestTreeStatMissingPath(t *testing.T) {
	tree := buildTestTree(t)
	if _, err := tree.Stat("6.5.30/no-such-disc"); err == nil {
		t.Fatal("stat succeeded on a missing disc")
	}
	if _, err := tree.Stat("6.5.30/irix-6-5-30-overlay-1of3/absent"); err == nil {
		t.Fatal("stat succeeded on a missing file")
	}
}

func TestCombinedStat(t *testing.T) {
	dir := t.TempDir()
	primary := combImage(t, dir, "tools.image", primaryDist)
	found := combImage(t, dir, "foundation1.iso", foundationDist)
	tree, err := BuildTree(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if _, err := tree.AddCombined("full", []string{primary, found}, nil); err != nil {
		t.Fatal(err)
	}

	// a disc's dist directory stats as a directory
	distInfo, err := tree.Stat("full/foundation1/dist")
	if err != nil {
		t.Fatal(err)
	}
	if !distInfo.IsDir {
		t.Fatal("combined dist not reported as a directory")
	}

	// a real product file stats as a non-directory with a real size
	fileInfo, err := tree.Stat("full/foundation1/dist/foundation.sw")
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.IsDir {
		t.Fatal("combined product file reported as a directory")
	}
	if fileInfo.Size == 0 {
		t.Fatal("combined product file size is zero")
	}
}
