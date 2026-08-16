// Package dvh reads the SGI disk volume header ("dvh"): the 512-byte label
// at block 0 of SGI disks and CD images, holding a directory of raw boot
// files (sash, fx, ide) and the partition table.
package dvh

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// Magic is the big-endian value at byte 0 of a volume header.
const Magic = 0x0be5a941

// Partition types from the SGI partition table.
const (
	TypeVolHdr PartitionType = 0
	TypeRaw    PartitionType = 3
	TypeVolume PartitionType = 6
	TypeEFS    PartitionType = 7
	TypeXFS    PartitionType = 10
)

// PartitionType identifies what a partition holds.
type PartitionType int32

// BootFile is a volume-directory entry: a raw file (sash, fx, ide) stored
// outside any filesystem, addressed by logical block number.
type BootFile struct {
	Name  string
	LBN   int32 // first 512-byte block of the file
	Bytes int32
}

// Partition is one slot of the 16-entry partition table. Sizes and offsets
// are in 512-byte blocks.
type Partition struct {
	Blocks int32
	First  int32
	Type   PartitionType
}

// Header is a parsed volume header.
type Header struct {
	BootFiles  []BootFile
	Partitions [16]Partition

	csumOK bool
}

// Parse reads a volume header from b, which must hold at least 512 bytes.
// A checksum mismatch does not fail the parse — real images are served on
// magic and bounds, and ChecksumOK lets the caller warn.
func Parse(b []byte) (*Header, error) {
	if len(b) < 512 {
		return nil, fmt.Errorf("dvh: header needs 512 bytes, got %d", len(b))
	}
	if m := binary.BigEndian.Uint32(b[0:4]); m != Magic {
		return nil, fmt.Errorf("dvh: bad magic %#x, want %#x", m, Magic)
	}
	h := &Header{}
	for i := 0; i < 15; i++ {
		off := 72 + i*16
		name := b[off : off+8]
		if j := bytes.IndexByte(name, 0); j >= 0 {
			name = name[:j]
		}
		if len(name) == 0 {
			continue
		}
		h.BootFiles = append(h.BootFiles, BootFile{
			Name:  string(name),
			LBN:   int32(binary.BigEndian.Uint32(b[off+8:])),
			Bytes: int32(binary.BigEndian.Uint32(b[off+12:])),
		})
	}
	for i := 0; i < 16; i++ {
		off := 312 + i*12
		h.Partitions[i] = Partition{
			Blocks: int32(binary.BigEndian.Uint32(b[off:])),
			First:  int32(binary.BigEndian.Uint32(b[off+4:])),
			Type:   PartitionType(binary.BigEndian.Uint32(b[off+8:])),
		}
	}
	var sum uint32
	for off := 0; off < 512; off += 4 {
		sum += binary.BigEndian.Uint32(b[off : off+4])
	}
	h.csumOK = sum == 0
	return h, nil
}

// ChecksumOK reports whether the header's int32-word sum is zero, the dvh
// checksum rule.
func (h *Header) ChecksumOK() bool { return h.csumOK }

// EFS returns the filesystem partition to serve: slot 7 when populated (the
// SGI convention for the whole filesystem), otherwise the first partition of
// type EFS.
func (h *Header) EFS() (Partition, bool) {
	if h.Partitions[7].Blocks > 0 {
		return h.Partitions[7], true
	}
	for _, p := range h.Partitions {
		if p.Type == TypeEFS && p.Blocks > 0 {
			return p, true
		}
	}
	return Partition{}, false
}
