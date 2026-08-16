package dvh

import (
	"encoding/binary"
	"testing"
)

// buildHeader assembles a synthetic volume header: boot files in the volume
// directory (offset 72, 15 x 16 bytes), partitions in the table (offset 312,
// 16 x 12 bytes), and a valid vh_csum (offset 504) so the 512-byte header
// sums to zero as big-endian int32 words.
func buildHeader(boot []BootFile, parts map[int]Partition) []byte {
	b := make([]byte, 512)
	binary.BigEndian.PutUint32(b[0:4], Magic)
	for i, f := range boot {
		off := 72 + i*16
		copy(b[off:off+8], f.Name)
		binary.BigEndian.PutUint32(b[off+8:], uint32(f.LBN))
		binary.BigEndian.PutUint32(b[off+12:], uint32(f.Bytes))
	}
	for i, p := range parts {
		off := 312 + i*12
		binary.BigEndian.PutUint32(b[off:], uint32(p.Blocks))
		binary.BigEndian.PutUint32(b[off+4:], uint32(p.First))
		binary.BigEndian.PutUint32(b[off+8:], uint32(p.Type))
	}
	var sum uint32
	for off := 0; off < 512; off += 4 {
		sum += binary.BigEndian.Uint32(b[off : off+4])
	}
	binary.BigEndian.PutUint32(b[504:], -sum)
	return b
}

func TestParseRejectsBadMagic(t *testing.T) {
	b := make([]byte, 512)
	if _, err := Parse(b); err == nil {
		t.Fatal("Parse accepted a zeroed header; want magic error")
	}
}

func TestParseVolumeDirectory(t *testing.T) {
	h, err := Parse(buildHeader([]BootFile{{Name: "sash", LBN: 4, Bytes: 12345}}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.BootFiles) != 1 {
		t.Fatalf("got %d boot files, want 1", len(h.BootFiles))
	}
	f := h.BootFiles[0]
	if f.Name != "sash" || f.LBN != 4 || f.Bytes != 12345 {
		t.Fatalf("got %+v, want {sash 4 12345}", f)
	}
}

func TestParsePartitionTable(t *testing.T) {
	h, err := Parse(buildHeader(nil, map[int]Partition{
		7:  {Blocks: 100000, First: 64, Type: TypeEFS},
		10: {Blocks: 500, First: 0, Type: TypeVolHdr},
	}))
	if err != nil {
		t.Fatal(err)
	}
	p := h.Partitions[7]
	if p.Blocks != 100000 || p.First != 64 || p.Type != TypeEFS {
		t.Fatalf("partition 7 = %+v, want {100000 64 efs}", p)
	}
	if h.Partitions[0].Blocks != 0 {
		t.Fatalf("partition 0 = %+v, want empty", h.Partitions[0])
	}
}

func TestEFSPrefersSlotSeven(t *testing.T) {
	h, err := Parse(buildHeader(nil, map[int]Partition{
		3: {Blocks: 10, First: 999, Type: TypeEFS},
		7: {Blocks: 100000, First: 64, Type: TypeEFS},
	}))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := h.EFS()
	if !ok || p.First != 64 {
		t.Fatalf("EFS() = %+v %v, want slot-7 partition at 64", p, ok)
	}
}

func TestEFSFallsBackToTypeScan(t *testing.T) {
	h, err := Parse(buildHeader(nil, map[int]Partition{
		3: {Blocks: 10, First: 999, Type: TypeEFS},
	}))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := h.EFS()
	if !ok || p.First != 999 {
		t.Fatalf("EFS() = %+v %v, want type-scan hit at 999", p, ok)
	}
}

func TestChecksum(t *testing.T) {
	good := buildHeader(nil, nil)
	h, err := Parse(good)
	if err != nil {
		t.Fatal(err)
	}
	if !h.ChecksumOK() {
		t.Fatal("valid checksum reported bad")
	}
	good[100] ^= 0xff // corrupt a device-params byte the sum covers
	h, err = Parse(good)
	if err != nil {
		t.Fatal(err)
	}
	if h.ChecksumOK() {
		t.Fatal("corrupted header reported good checksum")
	}
}
