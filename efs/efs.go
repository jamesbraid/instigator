// Package efs reads the SGI EFS filesystem: 512-byte basic blocks,
// 128-byte inodes grouped in cylinder groups, extent-mapped files. Read
// only — this package serves install media, it never writes.
package efs

import (
	"encoding/binary"
	"fmt"
	"io"
)

// SuperMagic is the classic EFS superblock magic. SuperMagicGrown marks a
// grown filesystem; both are accepted.
const (
	SuperMagic      = 0x00072959
	SuperMagicGrown = 0x0007295a
)

const (
	blockSize   = 512
	inodesPerBB = 4
)

// FS is an open EFS filesystem over a block image.
type FS struct {
	r    io.ReaderAt
	base int64 // partition start, in 512-byte blocks

	size    int32
	firstCG int32
	cgFSize int32
	cgISize int16
	ncg     int16
}

// Open reads and validates the superblock of the EFS filesystem starting at
// baseBlock (in 512-byte blocks) within r.
func Open(r io.ReaderAt, baseBlock int64) (*FS, error) {
	fs := &FS{r: r, base: baseBlock}
	sb, err := fs.readBlock(1)
	if err != nil {
		return nil, fmt.Errorf("efs: superblock: %w", err)
	}
	magic := binary.BigEndian.Uint32(sb[28:32])
	if magic != SuperMagic && magic != SuperMagicGrown {
		return nil, fmt.Errorf("efs: bad superblock magic %#x", magic)
	}
	fs.size = int32(binary.BigEndian.Uint32(sb[0:]))
	fs.firstCG = int32(binary.BigEndian.Uint32(sb[4:]))
	fs.cgFSize = int32(binary.BigEndian.Uint32(sb[8:]))
	fs.cgISize = int16(binary.BigEndian.Uint16(sb[12:]))
	fs.ncg = int16(binary.BigEndian.Uint16(sb[18:]))
	if fs.firstCG <= 0 || fs.cgFSize <= 0 || fs.cgISize <= 0 || fs.ncg <= 0 {
		return nil, fmt.Errorf("efs: implausible geometry firstcg=%d cgfsize=%d cgisize=%d ncg=%d",
			fs.firstCG, fs.cgFSize, fs.cgISize, fs.ncg)
	}
	return fs, nil
}

// readBlock returns one basic block, addressed relative to the filesystem.
func (fs *FS) readBlock(n int64) ([]byte, error) {
	b := make([]byte, blockSize)
	if _, err := fs.r.ReadAt(b, (fs.base+n)*blockSize); err != nil {
		return nil, err
	}
	return b, nil
}
