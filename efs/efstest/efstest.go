// Package efstest builds tiny synthetic EFS images for tests. Everything
// instigator serves in CI is generated here — no SGI-derived bytes enter
// the tree. Geometry is fixed small: one cylinder group, 2 inode blocks
// (8 inodes), 64 blocks.
package efstest

import "encoding/binary"

const (
	firstCG = 2
	cgFSize = 64
	cgISize = 2
	fsSize  = 66

	superMagic = 0x00072959
	dvhMagic   = 0x0be5a941
)

// Builder assembles an EFS image in memory. The zero value is not usable;
// call New.
type Builder struct {
	blocks    map[int64][]byte
	nextinode uint32
	nextdata  int64
}

// New returns a Builder holding a valid superblock. Inode 2 is reserved
// for the root directory; populate it with SetRoot.
func New() *Builder {
	b := &Builder{blocks: map[int64][]byte{}, nextinode: 3, nextdata: firstCG + cgISize}
	sb := make([]byte, 512)
	binary.BigEndian.PutUint32(sb[0:], fsSize)
	binary.BigEndian.PutUint32(sb[4:], firstCG)
	binary.BigEndian.PutUint32(sb[8:], cgFSize)
	binary.BigEndian.PutUint16(sb[12:], cgISize)
	binary.BigEndian.PutUint16(sb[14:], 1) // sectors
	binary.BigEndian.PutUint16(sb[16:], 1) // heads
	binary.BigEndian.PutUint16(sb[18:], 1) // ncg
	binary.BigEndian.PutUint32(sb[28:], superMagic)
	b.blocks[1] = sb
	return b
}

func (b *Builder) block(n int64) []byte {
	if b.blocks[n] == nil {
		b.blocks[n] = make([]byte, 512)
	}
	return b.blocks[n]
}

// PutInode writes a raw 128-byte inode. Extents are {block, length,
// logical offset} triples.
func (b *Builder) PutInode(ino uint32, mode uint16, size int32, mtime uint32, extents [][3]uint32) {
	blk := b.block(firstCG + int64(ino/4))
	d := blk[(ino%4)*128 : (ino%4)*128+128]
	binary.BigEndian.PutUint16(d[0:], mode)
	binary.BigEndian.PutUint16(d[2:], 1) // nlink
	binary.BigEndian.PutUint16(d[4:], 10)
	binary.BigEndian.PutUint16(d[6:], 20)
	binary.BigEndian.PutUint32(d[8:], uint32(size))
	binary.BigEndian.PutUint32(d[16:], mtime)
	binary.BigEndian.PutUint16(d[28:], uint16(len(extents)))
	for i, e := range extents {
		binary.BigEndian.PutUint32(d[32+i*8:], e[0]&0xffffff)
		binary.BigEndian.PutUint32(d[32+i*8+4:], e[1]<<24|e[2]&0xffffff)
	}
}

// AddData writes raw bytes into consecutive data blocks and returns the
// run's first block and block count.
func (b *Builder) AddData(data []byte) (bn int64, nblocks uint32) {
	bn = b.nextdata
	n := (len(data) + 511) / 512
	for i := 0; i < n; i++ {
		end := (i + 1) * 512
		if end > len(data) {
			end = len(data)
		}
		copy(b.block(bn+int64(i)), data[i*512:end])
	}
	if n == 0 {
		n = 1
	}
	b.nextdata += int64(n)
	return bn, uint32(n)
}

// AddFile stores data as a regular file in one extent and returns its
// inode number.
func (b *Builder) AddFile(perm uint16, data []byte) uint32 {
	ino := b.nextinode
	b.nextinode++
	bn, n := b.AddData(data)
	b.PutInode(ino, 0x8000|perm, int32(len(data)), 1000+ino, [][3]uint32{{uint32(bn), n, 0}})
	return ino
}

// AddSymlink stores a symlink and returns its inode number.
func (b *Builder) AddSymlink(target string) uint32 {
	ino := b.nextinode
	b.nextinode++
	bn, n := b.AddData([]byte(target))
	b.PutInode(ino, 0xA000|0o777, int32(len(target)), 1000+ino, [][3]uint32{{uint32(bn), n, 0}})
	return ino
}

// AddFIFO stores a named pipe and returns its inode number. It exists so
// a test can aim a symlink at something that is neither a directory nor a
// regular file, which is the one shape real media occasionally carries
// and the tree refuses to serve.
func (b *Builder) AddFIFO() uint32 {
	ino := b.nextinode
	b.nextinode++
	b.PutInode(ino, 0x1000|0o644, 0, 1000+ino, nil)
	return ino
}

// AddFileExtents stores a file whose extent list is given explicitly.
func (b *Builder) AddFileExtents(size int32, extents [][3]uint32) uint32 {
	ino := b.nextinode
	b.nextinode++
	b.PutInode(ino, 0x8000|0o644, size, 1000+ino, extents)
	return ino
}

// AddFileIndirect stores a file with its extent table in an indirect
// block, EFS's layout once a file has more than 12 extents.
func (b *Builder) AddFileIndirect(size int32, extents [][3]uint32) uint32 {
	ino := b.nextinode
	b.nextinode++
	tbl := make([]byte, 512)
	for i, e := range extents {
		binary.BigEndian.PutUint32(tbl[i*8:], e[0]&0xffffff)
		binary.BigEndian.PutUint32(tbl[i*8+4:], e[1]<<24|e[2]&0xffffff)
	}
	tbn, tn := b.AddData(tbl)
	blk := b.block(firstCG + int64(ino/4))
	d := blk[(ino%4)*128 : (ino%4)*128+128]
	binary.BigEndian.PutUint16(d[0:], 0x8000|0o644)
	binary.BigEndian.PutUint16(d[2:], 1)
	binary.BigEndian.PutUint32(d[8:], uint32(size))
	binary.BigEndian.PutUint16(d[28:], uint16(len(extents)))
	binary.BigEndian.PutUint32(d[32:], uint32(tbn)&0xffffff)
	binary.BigEndian.PutUint32(d[36:], tn<<24)
	return ino
}

// AddDir stores a directory and returns its inode number.
func (b *Builder) AddDir(entries map[string]uint32) uint32 {
	ino := b.nextinode
	b.nextinode++
	b.setDir(ino, entries)
	return ino
}

// SetRoot writes the root directory (inode 2).
func (b *Builder) SetRoot(entries map[string]uint32) {
	b.setDir(2, entries)
}

func (b *Builder) setDir(ino uint32, entries map[string]uint32) {
	blk := dirblock(entries)
	bn, n := b.AddData(blk)
	b.PutInode(ino, 0x4000|0o755, 512, 1000+ino, [][3]uint32{{uint32(bn), n, 0}})
}

func dirblock(entries map[string]uint32) []byte {
	blk := make([]byte, 512)
	binary.BigEndian.PutUint16(blk[0:], 0xbeef)
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
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
	blk[2] = byte(off >> 1)
	blk[3] = byte(len(names))
	return blk
}

// Bytes lays the image out contiguously, filesystem block 0 first.
func (b *Builder) Bytes() []byte {
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

// CDImage wraps the filesystem in an SGI volume header the way install
// media are laid out: header at block 0, EFS partition in slot 7 at base,
// optional raw boot files in the volume directory.
func (b *Builder) CDImage(base int64, bootFiles map[string][]byte) []byte {
	efs := b.Bytes()
	// lay boot files after the partition to keep offsets simple
	type bf struct {
		name string
		lbn  int32
		data []byte
	}
	var boots []bf
	next := base + int64(len(efs)+511)/512
	for name, data := range bootFiles {
		boots = append(boots, bf{name, int32(next), data})
		next += int64(len(data)+511) / 512
	}

	hdr := make([]byte, 512)
	binary.BigEndian.PutUint32(hdr[0:4], dvhMagic)
	for i, f := range boots {
		off := 72 + i*16
		copy(hdr[off:off+8], f.name)
		binary.BigEndian.PutUint32(hdr[off+8:], uint32(f.lbn))
		binary.BigEndian.PutUint32(hdr[off+12:], uint32(len(f.data)))
	}
	// partition 7: the filesystem; partition 8: volume header, by convention
	p7 := 312 + 7*12
	binary.BigEndian.PutUint32(hdr[p7:], uint32(len(efs)/512))
	binary.BigEndian.PutUint32(hdr[p7+4:], uint32(base))
	binary.BigEndian.PutUint32(hdr[p7+8:], 7)
	p8 := 312 + 8*12
	binary.BigEndian.PutUint32(hdr[p8:], uint32(base))
	binary.BigEndian.PutUint32(hdr[p8+4:], 0)
	binary.BigEndian.PutUint32(hdr[p8+8:], 0)
	var sum uint32
	for off := 0; off < 512; off += 4 {
		sum += binary.BigEndian.Uint32(hdr[off : off+4])
	}
	binary.BigEndian.PutUint32(hdr[504:], -sum)

	out := make([]byte, next*512)
	copy(out, hdr)
	copy(out[base*512:], efs)
	for _, f := range boots {
		copy(out[f.lbn*512:], f.data)
	}
	return out
}
