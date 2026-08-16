package vfs

import (
	"fmt"
	"strings"
	"time"

	"github.com/jamesbraid/instigator/efs"
)

// FileInfo is a path's metadata. It follows the same level structure
// Open and ReadDir already resolve: a real EFS path returns its own
// inode's fields, and every tree level above the EFS boundary - the
// root, a media name, a disc slug, a combined-set name - gets stable
// synthetic values, since none of them is backed by a real inode.
type FileInfo struct {
	Ino   uint64
	IsDir bool
	Perm  uint32 // permission bits only, e.g. 0o755
	Nlink int
	UID   uint32
	GID   uint32
	Size  int64
	Mtime time.Time
}

// syntheticDirIno is reported for every synthetic directory level. It
// deliberately reuses efs.RootIno: a synthetic level plays the same
// role a filesystem root does, a directory that exists without being a
// disk block, so the same well-known inode number fits.
const syntheticDirIno = uint64(efs.RootIno)

func syntheticDir() FileInfo {
	return FileInfo{Ino: syntheticDirIno, IsDir: true, Perm: 0o755, Nlink: 2, Size: 512}
}

// Stat returns a path's metadata, resolving it exactly as Open and
// ReadDir do: a synthetic top-level file, a combined set, media/disc, or
// an EFS path within a disc. Anything Open or ReadDir can reach, Stat can
// describe.
func (t *Tree) Stat(path string) (FileInfo, error) {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return syntheticDir(), nil
	}
	if c, ok := t.synthetic[trimmed]; ok {
		return FileInfo{Ino: syntheticDirIno + 1, IsDir: false, Perm: 0o444, Nlink: 1, Size: int64(len(c))}, nil
	}
	parts := strings.SplitN(trimmed, "/", 2)
	if c, ok := t.combined[parts[0]]; ok {
		rest := ""
		if len(parts) == 2 {
			rest = parts[1]
		}
		return c.stat(rest)
	}
	parts3 := strings.SplitN(trimmed, "/", 3)
	discs, ok := t.medias[parts3[0]]
	if !ok {
		return FileInfo{}, fmt.Errorf("media %q: %w", parts3[0], ErrNotFound)
	}
	if len(parts3) == 1 {
		return syntheticDir(), nil
	}
	d, ok := discs[parts3[1]]
	if !ok {
		return FileInfo{}, fmt.Errorf("disc %q: %w", parts3[1], ErrNotFound)
	}
	rest := "/"
	if len(parts3) == 3 {
		rest = parts3[2]
	}
	node, err := d.lookupFollow(rest, 8)
	if err != nil {
		return FileInfo{}, fmt.Errorf("%s: %w", path, ErrNotFound)
	}
	return inodeInfo(node), nil
}

// inodeInfo converts a real EFS inode to FileInfo.
func inodeInfo(n *efs.Inode) FileInfo {
	return FileInfo{
		Ino:   uint64(n.Num),
		IsDir: n.IsDir(),
		Perm:  uint32(n.Mode) & 0o7777,
		Nlink: int(n.Nlink),
		UID:   uint32(n.UID),
		GID:   uint32(n.GID),
		Size:  int64(n.Size),
		Mtime: time.Unix(int64(n.Mtime), 0),
	}
}
