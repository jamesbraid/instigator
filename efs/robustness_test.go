package efs_test

import (
	"bytes"
	"testing"

	"github.com/jamesbraid/instigator/efs"
	"github.com/jamesbraid/instigator/efs/efstest"
)

// minFS opens a minimal valid image so tests can call methods on a real FS.
func minFS(t *testing.T) *efs.FS {
	t.Helper()
	img := efstest.New()
	img.SetRoot(nil)
	fs, err := efs.Open(bytes.NewReader(img.Bytes()), 0)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

// A negative on-disk size is corrupt; Inode must reject it rather than
// pass it downstream where make([]byte, size) panics. Reachable over the
// wire: an NFS filehandle carries an attacker-chosen inode number.
func TestInodeRejectsNegativeSize(t *testing.T) {
	img := efstest.New()
	img.PutInode(3, 0x4000|0o755, -1, 100, nil) // directory, negative size
	img.SetRoot(map[string]uint32{"bad": 3})
	fs, err := efs.Open(bytes.NewReader(img.Bytes()), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Inode(3); err == nil {
		t.Fatal("Inode accepted a negative-size inode; want error")
	}
}

// ReadDir and Readlink must not panic on a negative-size inode even when
// handed one directly (defense in depth behind Inode's check).
func TestReadDirNegativeSizeNoPanic(t *testing.T) {
	fs := minFS(t)
	if _, err := fs.ReadDir(&efs.Inode{Num: 3, Mode: 0x4000 | 0o755, Size: -1}); err == nil {
		t.Fatal("ReadDir on a negative-size inode returned no error; want error, not panic")
	}
}

func TestReadlinkNegativeSizeNoPanic(t *testing.T) {
	fs := minFS(t)
	if _, err := fs.Readlink(&efs.Inode{Num: 3, Mode: 0xA000 | 0o777, Size: -1}); err == nil {
		t.Fatal("Readlink on a negative-size inode returned no error; want error, not panic")
	}
}

// An inode number whose cylinder group lands at or beyond ncg must be
// rejected. The bug: int16(cg) truncates, so cg in [32768,65535] slips
// past the guard. With the small test geometry (ncg=1), inode 262144 has
// cg=32768; the full-width check must reject it.
func TestInodeCylinderGroupBoundsFullWidth(t *testing.T) {
	fs := minFS(t)
	if _, err := fs.Inode(262144); err == nil {
		t.Fatal("Inode(262144) accepted; cylinder-group bound bypassed by int16 truncation")
	}
}
