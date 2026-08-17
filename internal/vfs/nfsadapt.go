package vfs

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/jamesbraid/instigator/efs"
	"github.com/jamesbraid/instigator/nfs"
)

// NFSExport adapts the whole tree to nfs.FS. NFS mounts one disc at a
// time, chosen by the mount path (/media/disc); the filehandle encodes
// which disc alongside the EFS inode, so handles never collide between
// discs and stay valid for the server's lifetime.
func (t *Tree) NFSExport() nfs.FS {
	// assign each disc a stable small index for filehandle encoding
	e := &nfsExport{tree: t, discs: map[uint16]*Disc{}, images: map[uint16]string{}, index: map[string]uint16{}}
	var paths []string
	for media, discs := range t.medias {
		for slug := range discs {
			paths = append(paths, media+"/"+slug)
		}
	}
	sort.Strings(paths)
	for i, p := range paths {
		idx := uint16(i + 1)
		parts := strings.SplitN(p, "/", 2)
		e.discs[idx] = t.medias[parts[0]][parts[1]]
		e.images[idx] = t.files[parts[0]][parts[1]]
		e.index[p] = idx
	}
	return e
}

type nfsExport struct {
	tree   *Tree
	discs  map[uint16]*Disc  // disc index -> disc
	images map[uint16]string // disc index -> image filename
	index  map[string]uint16 // "media/slug" -> disc index
}

// fh packs a disc index and EFS inode into one 64-bit id: the top 16
// bits select the disc, the low 48 the inode.
func fh(disc uint16, ino uint32) uint64 { return uint64(disc)<<48 | uint64(ino) }

func splitFH(id uint64) (uint16, uint32) { return uint16(id >> 48), uint32(id & 0xffffffffffff) }

// nfsNode carries the disc index so later operations resolve the right
// image without another path lookup. image and path back Describe,
// the per-open log line: path is only ever built incrementally by
// MountRoot/Lookup/ReadDir, since a bare filehandle (NodeByID) carries
// no path of its own to reconstruct it from.
type nfsNode struct {
	disc  uint16
	ino   *efs.Inode
	image string
	path  string
}

func (n nfsNode) ID() uint64    { return fh(n.disc, n.ino.Num) }
func (n nfsNode) Mode() uint32  { return uint32(n.ino.Mode) }
func (n nfsNode) Size() int64   { return int64(n.ino.Size) }
func (n nfsNode) Mtime() uint32 { return n.ino.Mtime }

// Describe renders the image and in-image path for a per-open log
// line, via the optional nfs.Describer interface.
func (n nfsNode) Describe() string { return fmt.Sprintf("%s:/%s", n.image, n.path) }

// joinPath extends a path already relative to the export root with
// one more name; the root itself is "".
func joinPath(base, name string) string {
	if base == "" {
		return name
	}
	return base + "/" + name
}

func (e *nfsExport) MountRoot(path string) (nfs.Node, error) {
	idx, ok := e.index[strings.Trim(path, "/")]
	if !ok {
		return nil, nfs.ErrNotFound
	}
	ino, err := e.discs[idx].FS().Inode(efs.RootIno)
	if err != nil {
		return nil, nfs.ErrIO
	}
	return nfsNode{disc: idx, ino: ino, image: e.images[idx]}, nil
}

func (e *nfsExport) NodeByID(id uint64) (nfs.Node, error) {
	idx, num := splitFH(id)
	d, ok := e.discs[idx]
	if !ok {
		return nil, nfs.ErrStale
	}
	ino, err := d.FS().Inode(num)
	if err != nil {
		return nil, nfs.ErrStale
	}
	return nfsNode{disc: idx, ino: ino, image: e.images[idx]}, nil
}

func (e *nfsExport) Lookup(dir nfs.Node, name string) (nfs.Node, error) {
	dn := dir.(nfsNode)
	ents, err := e.discs[dn.disc].FS().ReadDir(dn.ino)
	if err != nil {
		return nil, nfs.ErrNotDir
	}
	for _, ent := range ents {
		if ent.Name == name {
			ino, err := e.discs[dn.disc].FS().Inode(ent.Ino)
			if err != nil {
				return nil, nfs.ErrIO
			}
			return nfsNode{disc: dn.disc, ino: ino, image: dn.image, path: joinPath(dn.path, name)}, nil
		}
	}
	return nil, nfs.ErrNotFound
}

func (e *nfsExport) ReadDir(dir nfs.Node) ([]nfs.DirEntry, error) {
	dn := dir.(nfsNode)
	ents, err := e.discs[dn.disc].FS().ReadDir(dn.ino)
	if err != nil {
		return nil, nfs.ErrNotDir
	}
	var out []nfs.DirEntry
	for _, ent := range ents {
		if ent.Name == "." || ent.Name == ".." {
			continue
		}
		ino, err := e.discs[dn.disc].FS().Inode(ent.Ino)
		if err != nil {
			return nil, nfs.ErrIO
		}
		out = append(out, nfs.DirEntry{Name: ent.Name, Node: nfsNode{disc: dn.disc, ino: ino, image: dn.image, path: joinPath(dn.path, ent.Name)}})
	}
	return out, nil
}

func (e *nfsExport) ReadAt(n nfs.Node, p []byte, off int64) (int, error) {
	nn := n.(nfsNode)
	got, err := e.discs[nn.disc].FS().ReadAt(nn.ino, p, off)
	// io.EOF is the normal end-of-file signal, not a failure: a read at
	// or past the end is a legitimate zero-length read the NFS client
	// reads as EOF.
	if err != nil && !errors.Is(err, io.EOF) {
		return got, nfs.ErrIO
	}
	return got, nil
}

func (e *nfsExport) Readlink(n nfs.Node) (string, error) {
	nn := n.(nfsNode)
	target, err := e.discs[nn.disc].FS().Readlink(nn.ino)
	if err != nil {
		return "", nfs.ErrIO
	}
	return target, nil
}
