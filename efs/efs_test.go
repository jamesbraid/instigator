package efs

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// imgBuilder assembles a tiny valid EFS image in memory: superblock at
// block 1, one cylinder group of inodes at firstCG, data blocks after.
// Geometry is fixed small: 2 inode blocks per cg (8 inodes), 64 blocks
// per cg, 1 cg.
type imgBuilder struct {
	blocks  map[int64][]byte // block number -> 512 bytes
	nextinode uint32
	nextdata  int64
}

const (
	tFirstCG  = 2
	tCGFSize  = 64
	tCGISize  = 2 // inode blocks per cg -> 8 inodes
	tFSSize   = 66
)

func newImg() *imgBuilder {
	// inode 2 is reserved for the root directory (set via setDir)
	b := &imgBuilder{blocks: map[int64][]byte{}, nextinode: 3, nextdata: tFirstCG + tCGISize}
	sb := make([]byte, 512)
	binary.BigEndian.PutUint32(sb[0:], tFSSize)
	binary.BigEndian.PutUint32(sb[4:], tFirstCG)
	binary.BigEndian.PutUint32(sb[8:], tCGFSize)
	binary.BigEndian.PutUint16(sb[12:], tCGISize)
	binary.BigEndian.PutUint16(sb[14:], 1) // sectors
	binary.BigEndian.PutUint16(sb[16:], 1) // heads
	binary.BigEndian.PutUint16(sb[18:], 1) // ncg
	binary.BigEndian.PutUint32(sb[28:], SuperMagic)
	b.blocks[1] = sb
	return b
}

func (b *imgBuilder) block(n int64) []byte {
	if b.blocks[n] == nil {
		b.blocks[n] = make([]byte, 512)
	}
	return b.blocks[n]
}

// putInode writes a 128-byte inode for ino with the given mode/size/extents.
func (b *imgBuilder) putInode(ino uint32, mode uint16, size int32, mtime uint32, extents [][3]uint32) {
	blk := b.block(tFirstCG + int64(ino/4))
	d := blk[(ino%4)*128 : (ino%4)*128+128]
	binary.BigEndian.PutUint16(d[0:], mode)
	binary.BigEndian.PutUint16(d[2:], 1) // nlink
	binary.BigEndian.PutUint16(d[4:], 10)
	binary.BigEndian.PutUint16(d[6:], 20)
	binary.BigEndian.PutUint32(d[8:], uint32(size))
	binary.BigEndian.PutUint32(d[16:], mtime)
	binary.BigEndian.PutUint16(d[28:], uint16(len(extents)))
	for i, e := range extents { // e = {bn, length, logical offset}
		binary.BigEndian.PutUint32(d[32+i*8:], e[0]&0xffffff)
		binary.BigEndian.PutUint32(d[32+i*8+4:], e[1]<<24|e[2]&0xffffff)
	}
}

// addData writes raw bytes into consecutive data blocks, returning the run.
func (b *imgBuilder) addData(data []byte) (bn int64, nblocks uint32) {
	bn = b.nextdata
	n := (len(data) + 511) / 512
	for i := 0; i < n; i++ {
		end := (i + 1) * 512
		if end > len(data) {
			end = len(data)
		}
		copy(b.block(bn+int64(i)), data[i*512:end])
	}
	b.nextdata += int64(n)
	if n == 0 {
		b.nextdata++
		n = 1
	}
	return bn, uint32(n)
}

// addFile stores data as a regular file in one extent and returns its inode.
func (b *imgBuilder) addFile(mode uint16, data []byte) uint32 {
	ino := b.nextinode
	b.nextinode++
	bn, n := b.addData(data)
	b.putInode(ino, 0x8000|mode, int32(len(data)), 1000+ino, [][3]uint32{{uint32(bn), n, 0}})
	return ino
}

// dirblock builds one 512-byte EFS directory block from name->ino entries.
func dirblock(entries map[string]uint32) []byte {
	blk := make([]byte, 512)
	binary.BigEndian.PutUint16(blk[0:], 0xbeef)
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	// entries packed from the tail, 2-byte aligned
	off := 512
	slot := 4
	for _, name := range names {
		need := 5 + len(name)
		if need%2 != 0 {
			need++
		}
		off -= need
		binary.BigEndian.PutUint32(blk[off:], entries[name])
		blk[off+4] = byte(len(name))
		copy(blk[off+5:], name)
		blk[slot] = byte(off >> 1)
		slot++
	}
	blk[2] = byte(off >> 1) // firstused
	blk[3] = byte(len(names))
	return blk
}

// addDir stores a directory with the given entries and returns its inode.
func (b *imgBuilder) addDir(entries map[string]uint32) uint32 {
	ino := b.nextinode
	b.nextinode++
	b.setDir(ino, entries)
	return ino
}

// setDir writes directory content for a pre-chosen inode number.
func (b *imgBuilder) setDir(ino uint32, entries map[string]uint32) {
	blk := dirblock(entries)
	bn, n := b.addData(blk)
	b.putInode(ino, 0x4000|0o755, 512, 1000+ino, [][3]uint32{{uint32(bn), n, 0}})
}

// bytes lays the sparse block map out as one contiguous image.
func (b *imgBuilder) bytes() []byte {
	var max int64
	for n := range b.blocks {
		if n > max {
			max = n
		}
	}
	out := make([]byte, (max+1)*512)
	for n, blk := range b.blocks {
		copy(out[n*512:], blk)
	}
	return out
}

// addSymlink stores target as link content and returns the inode.
func (b *imgBuilder) addSymlink(target string) uint32 {
	ino := b.nextinode
	b.nextinode++
	bn, n := b.addData([]byte(target))
	b.putInode(ino, 0xA000|0o777, int32(len(target)), 1000+ino, [][3]uint32{{uint32(bn), n, 0}})
	return ino
}

// addFileExtents stores a file whose extent list is given explicitly.
func (b *imgBuilder) addFileExtents(size int32, extents [][3]uint32) uint32 {
	ino := b.nextinode
	b.nextinode++
	b.putInode(ino, 0x8000|0o644, size, 1000+ino, extents)
	return ino
}

// addFileIndirect stores a file with the real extent table in indirect
// blocks: the inode's direct slots describe blocks holding 8-byte extent
// records, EFS's layout once a file has more than 12 extents.
func (b *imgBuilder) addFileIndirect(size int32, extents [][3]uint32) uint32 {
	ino := b.nextinode
	b.nextinode++
	tbl := make([]byte, 512)
	for i, e := range extents {
		binary.BigEndian.PutUint32(tbl[i*8:], e[0]&0xffffff)
		binary.BigEndian.PutUint32(tbl[i*8+4:], e[1]<<24|e[2]&0xffffff)
	}
	tbn, tn := b.addData(tbl)
	blk := b.block(tFirstCG + int64(ino/4))
	d := blk[(ino%4)*128 : (ino%4)*128+128]
	binary.BigEndian.PutUint16(d[0:], 0x8000|0o644)
	binary.BigEndian.PutUint16(d[2:], 1)
	binary.BigEndian.PutUint32(d[8:], uint32(size))
	binary.BigEndian.PutUint16(d[28:], uint16(len(extents)))
	binary.BigEndian.PutUint32(d[32:], uint32(tbn)&0xffffff)
	binary.BigEndian.PutUint32(d[36:], tn<<24)
	return ino
}

// fixture builds an image with a small tree:
//
//	/hello.txt   "hello, octane\n"
//	/sub/nested.txt  "nested"
//	/link -> hello.txt
func fixture(t *testing.T) (*FS, map[string]uint32) {
	t.Helper()
	img := newImg()
	inos := map[string]uint32{}
	inos["hello.txt"] = img.addFile(0o644, []byte("hello, octane\n"))
	inos["nested.txt"] = img.addFile(0o444, []byte("nested"))
	inos["sub"] = img.addDir(map[string]uint32{"nested.txt": inos["nested.txt"]})
	inos["link"] = img.addSymlink("hello.txt")
	img.setDir(2, map[string]uint32{
		"hello.txt": inos["hello.txt"],
		"sub":       inos["sub"],
		"link":      inos["link"],
	})
	fs, err := Open(bytes.NewReader(img.bytes()), 0)
	if err != nil {
		t.Fatal(err)
	}
	return fs, inos
}

func TestInodeFields(t *testing.T) {
	fs, inos := fixture(t)
	ino, err := fs.Inode(inos["hello.txt"])
	if err != nil {
		t.Fatal(err)
	}
	if ino.Mode != 0x8000|0o644 || ino.Size != 14 || ino.UID != 10 || ino.GID != 20 {
		t.Fatalf("inode = %+v", ino)
	}
	if ino.Mtime != 1000+inos["hello.txt"] {
		t.Fatalf("mtime = %d, want %d", ino.Mtime, 1000+inos["hello.txt"])
	}
	if !ino.IsRegular() || ino.IsDir() {
		t.Fatal("type predicates wrong for regular file")
	}
}

func TestReadDirRoot(t *testing.T) {
	fs, _ := fixture(t)
	root, err := fs.Inode(RootIno)
	if err != nil {
		t.Fatal(err)
	}
	ents, err := fs.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range ents {
		got[e.Name] = true
	}
	for _, want := range []string{"hello.txt", "sub", "link"} {
		if !got[want] {
			t.Fatalf("missing %q in %v", want, ents)
		}
	}
}

func TestLookup(t *testing.T) {
	fs, inos := fixture(t)
	ino, err := fs.Lookup("/sub/nested.txt")
	if err != nil {
		t.Fatal(err)
	}
	if ino.Num != inos["nested.txt"] {
		t.Fatalf("resolved ino %d, want %d", ino.Num, inos["nested.txt"])
	}
	if _, err := fs.Lookup("/sub/absent"); err == nil {
		t.Fatal("Lookup found a file that does not exist")
	}
}

func TestReadAt(t *testing.T) {
	fs, inos := fixture(t)
	ino, _ := fs.Inode(inos["hello.txt"])
	p := make([]byte, 5)
	n, err := fs.ReadAt(ino, p, 7)
	if err != nil {
		t.Fatal(err)
	}
	if n != 5 || string(p) != "octan" {
		t.Fatalf("ReadAt(7) = %d %q", n, p)
	}
	// read spanning EOF returns the short count and io.EOF
	n, err = fs.ReadAt(ino, p, 12)
	if n != 2 || string(p[:n]) != "e\n" || err != io.EOF {
		t.Fatalf("ReadAt(12) = %d %q %v, want 2 \"e\\n\" EOF", n, p[:n], err)
	}
}

func TestReadAtSparseHole(t *testing.T) {
	img := newImg()
	bn, _ := img.addData([]byte("tail"))
	// one extent at logical block 2; blocks 0-1 are a hole
	ino := img.addFileExtents(2*512+4, [][3]uint32{{uint32(bn), 1, 2}})
	img.setDir(2, map[string]uint32{"sparse": ino})
	fs, err := Open(bytes.NewReader(img.bytes()), 0)
	if err != nil {
		t.Fatal(err)
	}
	node, _ := fs.Inode(ino)
	p := make([]byte, 6)
	if _, err := fs.ReadAt(node, p, 510); err != nil {
		t.Fatal(err)
	}
	if string(p) != "\x00\x00\x00\x00\x00\x00" {
		t.Fatalf("hole read = %q, want zeros", p)
	}
	if _, err := fs.ReadAt(node, p[:4], 1024); err != nil {
		t.Fatal(err)
	}
	if string(p[:4]) != "tail" {
		t.Fatalf("tail read = %q", p[:4])
	}
}

func TestMultiExtent(t *testing.T) {
	img := newImg()
	b1, _ := img.addData(bytes.Repeat([]byte("A"), 512))
	b2, _ := img.addData(bytes.Repeat([]byte("B"), 512))
	ino := img.addFileExtents(1024, [][3]uint32{{uint32(b1), 1, 0}, {uint32(b2), 1, 1}})
	img.setDir(2, map[string]uint32{"two": ino})
	fs, err := Open(bytes.NewReader(img.bytes()), 0)
	if err != nil {
		t.Fatal(err)
	}
	node, _ := fs.Inode(ino)
	p := make([]byte, 4)
	if _, err := fs.ReadAt(node, p, 510); err != nil {
		t.Fatal(err)
	}
	if string(p) != "AABB" {
		t.Fatalf("extent boundary read = %q, want AABB", p)
	}
}

func TestIndirectExtents(t *testing.T) {
	img := newImg()
	var extents [][3]uint32
	var want []byte
	for i := 0; i < 14; i++ {
		c := byte('a' + i)
		bn, _ := img.addData(bytes.Repeat([]byte{c}, 512))
		extents = append(extents, [3]uint32{uint32(bn), 1, uint32(i)})
		want = append(want, c)
	}
	ino := img.addFileIndirect(14*512, extents)
	img.setDir(2, map[string]uint32{"big": ino})
	fs, err := Open(bytes.NewReader(img.bytes()), 0)
	if err != nil {
		t.Fatal(err)
	}
	node, _ := fs.Inode(ino)
	if len(node.Extents) != 14 {
		t.Fatalf("got %d extents, want 14", len(node.Extents))
	}
	p := make([]byte, 1)
	for i, c := range want {
		if _, err := fs.ReadAt(node, p, int64(i)*512); err != nil {
			t.Fatal(err)
		}
		if p[0] != c {
			t.Fatalf("block %d starts with %q, want %q", i, p[0], c)
		}
	}
}

func TestReadlink(t *testing.T) {
	fs, inos := fixture(t)
	node, _ := fs.Inode(inos["link"])
	if !node.IsSymlink() {
		t.Fatal("link inode not a symlink")
	}
	target, err := fs.Readlink(node)
	if err != nil {
		t.Fatal(err)
	}
	if target != "hello.txt" {
		t.Fatalf("readlink = %q", target)
	}
}

func TestOpenRejectsBadMagic(t *testing.T) {
	if _, err := Open(bytes.NewReader(make([]byte, 4096)), 0); err == nil {
		t.Fatal("Open accepted zeroed superblock; want magic error")
	}
}

func TestOpenParsesSuperblock(t *testing.T) {
	img := newImg()
	img.setDir(2, nil)
	fs, err := Open(bytes.NewReader(img.bytes()), 0)
	if err != nil {
		t.Fatal(err)
	}
	if fs == nil {
		t.Fatal("nil FS")
	}
}
