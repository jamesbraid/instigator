package efs

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strings"
)

// RootIno is the inode number of the filesystem root directory.
const RootIno uint32 = 2

const (
	inodeSize     = 128
	directExtents = 12
	dirblkMagic   = 0xbeef

	fmtMask    = 0xf000
	fmtRegular = 0x8000
	fmtDir     = 0x4000
	fmtSymlink = 0xa000
)

// Extent is one contiguous run of basic blocks. Offset is the logical
// block position within the file; a gap between extents is a hole that
// reads as zeros.
type Extent struct {
	Block  uint32 // first basic block, filesystem-relative
	Len    uint32 // run length in blocks
	Offset uint32 // logical block offset within the file
}

// Inode is a parsed EFS inode.
type Inode struct {
	Num   uint32
	Mode  uint16
	Nlink int16
	UID   uint16
	GID   uint16
	Size  int32
	Atime uint32
	Mtime uint32
	Ctime uint32
	Gen   int32

	Extents []Extent // sorted by Offset
}

// IsRegular reports whether the inode is a regular file.
func (i *Inode) IsRegular() bool { return i.Mode&fmtMask == fmtRegular }

// IsDir reports whether the inode is a directory.
func (i *Inode) IsDir() bool { return i.Mode&fmtMask == fmtDir }

// IsSymlink reports whether the inode is a symbolic link.
func (i *Inode) IsSymlink() bool { return i.Mode&fmtMask == fmtSymlink }

// DirEntry is one directory entry.
type DirEntry struct {
	Name string
	Ino  uint32
}

// Inode reads inode number ino.
func (fs *FS) Inode(ino uint32) (*Inode, error) {
	inopcg := uint32(fs.cgISize) * inodesPerBB
	if inopcg == 0 {
		return nil, fmt.Errorf("efs: zero inodes per cylinder group")
	}
	cg := ino / inopcg
	if int16(cg) >= fs.ncg {
		return nil, fmt.Errorf("efs: inode %d beyond %d cylinder groups", ino, fs.ncg)
	}
	bb := int64(fs.firstCG) + int64(cg)*int64(fs.cgFSize) + int64((ino%inopcg)/inodesPerBB)
	blk, err := fs.readBlock(bb)
	if err != nil {
		return nil, fmt.Errorf("efs: inode %d: %w", ino, err)
	}
	d := blk[(ino%inodesPerBB)*inodeSize:][:inodeSize]

	n := &Inode{
		Num:   ino,
		Mode:  binary.BigEndian.Uint16(d[0:]),
		Nlink: int16(binary.BigEndian.Uint16(d[2:])),
		UID:   binary.BigEndian.Uint16(d[4:]),
		GID:   binary.BigEndian.Uint16(d[6:]),
		Size:  int32(binary.BigEndian.Uint32(d[8:])),
		Atime: binary.BigEndian.Uint32(d[12:]),
		Mtime: binary.BigEndian.Uint32(d[16:]),
		Ctime: binary.BigEndian.Uint32(d[20:]),
		Gen:   int32(binary.BigEndian.Uint32(d[24:])),
	}
	numext := int(int16(binary.BigEndian.Uint16(d[28:])))
	if numext < 0 || numext > 0xffff {
		return nil, fmt.Errorf("efs: inode %d: implausible extent count %d", ino, numext)
	}
	if numext <= directExtents {
		for i := 0; i < numext; i++ {
			n.Extents = append(n.Extents, parseExtent(d[32+i*8:]))
		}
	} else {
		// The direct slots describe blocks holding the real extent table.
		var indir []Extent
		for i := 0; i < directExtents; i++ {
			e := parseExtent(d[32+i*8:])
			if e.Len == 0 {
				break
			}
			indir = append(indir, e)
		}
		got := 0
	outer:
		for _, ie := range indir {
			for b := uint32(0); b < ie.Len; b++ {
				blk, err := fs.readBlock(int64(ie.Block + b))
				if err != nil {
					return nil, fmt.Errorf("efs: inode %d extent table: %w", ino, err)
				}
				for k := 0; k < blockSize/8 && got < numext; k++ {
					n.Extents = append(n.Extents, parseExtent(blk[k*8:]))
					got++
				}
				if got >= numext {
					break outer
				}
			}
		}
		if got < numext {
			return nil, fmt.Errorf("efs: inode %d: extent table short: %d of %d", ino, got, numext)
		}
	}
	sort.Slice(n.Extents, func(a, b int) bool { return n.Extents[a].Offset < n.Extents[b].Offset })
	return n, nil
}

func parseExtent(b []byte) Extent {
	w1 := binary.BigEndian.Uint32(b[0:])
	w2 := binary.BigEndian.Uint32(b[4:])
	return Extent{Block: w1 & 0xffffff, Len: w2 >> 24, Offset: w2 & 0xffffff}
}

// ReadAt reads from the file at byte offset off into p, returning io.EOF
// past the end as io.ReaderAt requires. Holes between extents read as
// zeros.
func (fs *FS) ReadAt(ino *Inode, p []byte, off int64) (int, error) {
	size := int64(ino.Size)
	if off >= size {
		return 0, io.EOF
	}
	want := int64(len(p))
	if off+want > size {
		want = size - off
	}
	var done int64
	for done < want {
		pos := off + done
		blk := uint32(pos / blockSize)
		bo := pos % blockSize
		chunk := int64(blockSize) - bo
		if chunk > want-done {
			chunk = want - done
		}
		src, err := fs.blockFor(ino, blk)
		if err != nil {
			return int(done), err
		}
		if src == nil {
			for i := int64(0); i < chunk; i++ {
				p[done+i] = 0
			}
		} else {
			copy(p[done:done+chunk], src[bo:bo+chunk])
		}
		done += chunk
	}
	if done < int64(len(p)) {
		return int(done), io.EOF
	}
	return int(done), nil
}

// blockFor maps a logical file block to its data, or nil for a hole.
func (fs *FS) blockFor(ino *Inode, logical uint32) ([]byte, error) {
	i := sort.Search(len(ino.Extents), func(i int) bool {
		e := ino.Extents[i]
		return e.Offset+e.Len > logical
	})
	if i >= len(ino.Extents) || ino.Extents[i].Offset > logical {
		return nil, nil // hole
	}
	e := ino.Extents[i]
	return fs.readBlock(int64(e.Block + (logical - e.Offset)))
}

// ReadDir parses directory blocks: magic 0xbeef, a slot-offset table, and
// entries of inode number, name length, name.
func (fs *FS) ReadDir(ino *Inode) ([]DirEntry, error) {
	if !ino.IsDir() {
		return nil, fmt.Errorf("efs: inode %d is not a directory", ino.Num)
	}
	data := make([]byte, ino.Size)
	if _, err := fs.ReadAt(ino, data, 0); err != nil && err != io.EOF {
		return nil, err
	}
	var out []DirEntry
	for base := 0; base < len(data); base += blockSize {
		end := base + blockSize
		if end > len(data) {
			end = len(data)
		}
		blk := data[base:end]
		if len(blk) < 4 || binary.BigEndian.Uint16(blk[0:]) != dirblkMagic {
			continue
		}
		slots := int(blk[3])
		for s := 0; s < slots && 4+s < len(blk); s++ {
			so := int(blk[4+s]) << 1
			if so == 0 || so+5 > len(blk) {
				continue
			}
			nameLen := int(blk[so+4])
			if so+5+nameLen > len(blk) {
				continue
			}
			out = append(out, DirEntry{
				Name: string(blk[so+5 : so+5+nameLen]),
				Ino:  binary.BigEndian.Uint32(blk[so:]),
			})
		}
	}
	return out, nil
}

// Readlink returns a symlink's target, stored as the link's file content.
func (fs *FS) Readlink(ino *Inode) (string, error) {
	if !ino.IsSymlink() {
		return "", fmt.Errorf("efs: inode %d is not a symlink", ino.Num)
	}
	p := make([]byte, ino.Size)
	if _, err := fs.ReadAt(ino, p, 0); err != nil && err != io.EOF {
		return "", err
	}
	return string(p), nil
}

// Lookup resolves a slash-separated path from the root. Symlinks in the
// path are not followed; install media use them rarely and the callers
// serve them as links.
func (fs *FS) Lookup(path string) (*Inode, error) {
	ino, err := fs.Inode(RootIno)
	if err != nil {
		return nil, err
	}
	for _, part := range strings.Split(strings.Trim(path, "/"), "/") {
		if part == "" {
			continue
		}
		ents, err := fs.ReadDir(ino)
		if err != nil {
			return nil, err
		}
		var next uint32
		for _, e := range ents {
			if e.Name == part {
				next = e.Ino
				break
			}
		}
		if next == 0 {
			return nil, fmt.Errorf("efs: %s: %q not found", path, part)
		}
		if ino, err = fs.Inode(next); err != nil {
			return nil, err
		}
	}
	return ino, nil
}
