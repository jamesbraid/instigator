package vfs

import (
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
	e := &nfsExport{tree: t, discs: map[uint16]*Disc{}, index: map[string]uint16{}}
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
		e.index[p] = idx
	}
	return e
}

type nfsExport struct {
	tree  *Tree
	discs map[uint16]*Disc  // disc index -> disc
	index map[string]uint16 // "media/slug" -> disc index
}

// fh packs a disc index and EFS inode into one 64-bit id: the top 16
// bits select the disc, the low 48 the inode.
func fh(disc uint16, ino uint32) uint64 { return uint64(disc)<<48 | uint64(ino) }

func splitFH(id uint64) (uint16, uint32) { return uint16(id >> 48), uint32(id & 0xffffffffffff) }

// nfsNode carries the disc index so later operations resolve the right
// image without another path lookup.
type nfsNode struct {
	disc uint16
	ino  *efs.Inode
}

func (n nfsNode) ID() uint64    { return fh(n.disc, n.ino.Num) }
func (n nfsNode) Mode() uint32  { return uint32(n.ino.Mode) }
func (n nfsNode) Size() int64   { return int64(n.ino.Size) }
func (n nfsNode) Mtime() uint32 { return n.ino.Mtime }

func (e *nfsExport) MountRoot(path string) (nfs.Node, error) {
	idx, ok := e.index[strings.Trim(path, "/")]
	if !ok {
		return nil, nfs.ErrNotFound
	}
	ino, err := e.discs[idx].FS().Inode(efs.RootIno)
	if err != nil {
		return nil, nfs.ErrIO
	}
	return nfsNode{idx, ino}, nil
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
	return nfsNode{idx, ino}, nil
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
			return nfsNode{dn.disc, ino}, nil
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
		out = append(out, nfs.DirEntry{Name: ent.Name, Node: nfsNode{dn.disc, ino}})
	}
	return out, nil
}

func (e *nfsExport) ReadAt(n nfs.Node, p []byte, off int64) (int, error) {
	nn := n.(nfsNode)
	got, err := e.discs[nn.disc].FS().ReadAt(nn.ino, p, off)
	if err != nil && got == 0 {
		return 0, nfs.ErrIO
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
