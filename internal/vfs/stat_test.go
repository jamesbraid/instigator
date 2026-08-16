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

func TestUnionStat(t *testing.T) {
	dir := t.TempDir()
	base := distDisc(t, dir, "base.iso", "base", []string{"eoe.sw"})
	over := distDisc(t, dir, "over.iso", "over", []string{"overlay.sw"})
	tree, err := BuildTree(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	if err := tree.AddUnion("full", []string{base, over}); err != nil {
		t.Fatal(err)
	}

	distInfo, err := tree.Stat("full/dist")
	if err != nil {
		t.Fatal(err)
	}
	if !distInfo.IsDir {
		t.Fatal("union dist not reported as a directory")
	}

	fileInfo, err := tree.Stat("full/dist/eoe.sw")
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.IsDir {
		t.Fatal("union file reported as a directory")
	}
	if fileInfo.Size == 0 {
		t.Fatal("union file size is zero")
	}
}
