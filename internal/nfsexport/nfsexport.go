// Package nfsexport adapts a standard io/fs filesystem to nfs.FS, served
// read-only. It keeps the nfs package's protocol implementation testable
// against a real tree; it is imported only by tests, since serve doesn't
// use NFS.
package nfsexport

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/jamesbraid/instigator/internal/vfs"
	"github.com/jamesbraid/instigator/nfs"
)

// resolver is the optional capability a fs.FS may have to name where a
// path's bytes come from - vfs.Tree implements it. Export uses it to build
// the Describe() manifest line; a fs.FS without it (fstest.MapFS, say)
// simply gets nodes that don't implement nfs.Describer at all.
type resolver interface {
	Resolve(name string) (vfs.Origin, error)
}

// record is everything Export precomputes for one path, so every nfs.FS
// method after Export is a map lookup rather than a walk.
type record struct {
	path     string
	isDir    bool
	id       uint64
	mode     uint32
	size     int64
	mtime    uint32
	describe string
}

// export is the nfs.FS Export returns.
type export struct {
	fsys fs.FS

	byPath map[string]*record
	byID   map[uint64]*record

	// describable is true when fsys exposed a resolver, so Export should
	// hand out nodes that implement nfs.Describer.
	describable bool
}

// Export adapts fsys to nfs.FS. fsys need only implement fs.FS; when it
// also implements fs.StatFS and/or the Resolve capability above (as
// vfs.Tree does), Export uses those for stable filehandles and manifest
// lines respectively.
func Export(fsys fs.FS) nfs.FS {
	_, hasResolve := fsys.(resolver)
	e := &export{
		fsys:        fsys,
		byPath:      map[string]*record{},
		byID:        map[uint64]*record{},
		describable: hasResolve,
	}
	e.index()
	return e
}

// index walks fsys once and populates byPath/byID. A walk error stops
// indexing early with whatever was found up to that point.
func (e *export) index() {
	statter, hasStat := e.fsys.(fs.StatFS)
	res, hasResolve := e.fsys.(resolver)
	var next uint64 = 1

	_ = fs.WalkDir(e.fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}

		rec := &record{
			path:  p,
			isDir: d.IsDir(),
			mode:  unixMode(info),
			size:  info.Size(),
			mtime: uint32(info.ModTime().Unix()),
		}

		id, ok := realIno(statter, hasStat, p)
		if !ok {
			id = next
			next++
		}
		rec.id = id

		if hasResolve && !d.IsDir() {
			if origin, err := res.Resolve(p); err == nil {
				rec.describe = fmt.Sprintf("%s:/%s", origin.Source, origin.Path)
			}
		}

		e.byPath[p] = rec
		e.byID[id] = rec
		return nil
	})
}

// realIno reports the stable inode fsys's Stat(p) carries in its
// *vfs.Metadata Sys(), if fsys is a fs.StatFS and reports one.
func realIno(statter fs.StatFS, hasStat bool, p string) (uint64, bool) {
	if !hasStat {
		return 0, false
	}
	info, err := statter.Stat(p)
	if err != nil {
		return 0, false
	}
	md, ok := info.Sys().(*vfs.Metadata)
	if !ok || md == nil {
		return 0, false
	}
	return md.Ino, true
}

// unixMode converts an fs.FileMode to the Unix mode-with-type-bits
// nfs.Node.Mode documents. An install tree materializes only directories
// and regular files; a symlink appears only in an fs.FS that can report
// one, such as an fstest.MapFS fixture.
func unixMode(info fs.FileInfo) uint32 {
	// The tree stores the full twelve permission bits numerically in its
	// Mode; Perm() would drop setuid/setgid/sticky, which NFS attributes
	// carry to the client.
	perm := uint32(info.Mode() & 0o7777)
	switch {
	case info.IsDir():
		return 0o040000 | perm
	case info.Mode()&fs.ModeSymlink != 0:
		return 0o120000 | perm
	}
	return 0o100000 | perm
}

// normalize turns an NFS mount path into an fs.FS name: no leading or
// trailing slash, "." for the root.
func normalize(p string) string {
	p = strings.Trim(p, "/")
	if p == "" {
		return "."
	}
	return p
}

// nfsNode is the plain nfs.Node: id, mode, size, mtime, nothing else.
type nfsNode struct{ rec *record }

func (n nfsNode) ID() uint64    { return n.rec.id }
func (n nfsNode) Mode() uint32  { return n.rec.mode }
func (n nfsNode) Size() int64   { return n.rec.size }
func (n nfsNode) Mtime() uint32 { return n.rec.mtime }

// record satisfies the hasRecord interface below via method promotion,
// which is how Lookup/ReadDir/ReadAt/Readlink recover a record from
// whichever of the two node types below they were handed.
func (n nfsNode) record() *record { return n.rec }

// describableNode additionally implements nfs.Describer; Export constructs
// it only when fsys has a resolver.
type describableNode struct{ nfsNode }

func (n describableNode) Describe() string { return n.rec.describe }

// hasRecord recovers the record backing an nfs.Node the export handed out
// earlier, regardless of which of the two node types it is.
type hasRecord interface {
	record() *record
}

func recordOf(n nfs.Node) (*record, bool) {
	hr, ok := n.(hasRecord)
	if !ok {
		return nil, false
	}
	return hr.record(), true
}

// node wraps rec in whichever concrete node type this export hands out.
func (e *export) node(rec *record) nfs.Node {
	base := nfsNode{rec: rec}
	if e.describable {
		return describableNode{base}
	}
	return base
}

func (e *export) MountRoot(p string) (nfs.Node, error) {
	rec, ok := e.byPath[normalize(p)]
	if !ok {
		return nil, nfs.ErrNotFound
	}
	return e.node(rec), nil
}

func (e *export) NodeByID(id uint64) (nfs.Node, error) {
	rec, ok := e.byID[id]
	if !ok {
		return nil, nfs.ErrStale
	}
	return e.node(rec), nil
}

// isComponent reports whether name is one ordinary directory entry, which
// is the only thing NFS LOOKUP may be given. Everything else is refused
// before the join below, because path.Join would resolve it: ".." joined
// onto a mounted subtree yields its parent, so a client mounted at
// /6.5.30 could otherwise walk up to the export root and read every other
// install set. "." and a name carrying a slash are the same mistake in a
// milder form - neither is an entry name.
func isComponent(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.Contains(name, "/")
}

func (e *export) Lookup(dir nfs.Node, name string) (nfs.Node, error) {
	drec, ok := recordOf(dir)
	if !ok || !drec.isDir {
		return nil, nfs.ErrNotDir
	}
	if !isComponent(name) {
		return nil, nfs.ErrNotFound
	}
	crec, ok := e.byPath[path.Join(drec.path, name)]
	if !ok {
		return nil, nfs.ErrNotFound
	}
	return e.node(crec), nil
}

func (e *export) ReadDir(dir nfs.Node) ([]nfs.DirEntry, error) {
	drec, ok := recordOf(dir)
	if !ok || !drec.isDir {
		return nil, nfs.ErrNotDir
	}
	ents, err := fs.ReadDir(e.fsys, drec.path)
	if err != nil {
		return nil, nfs.ErrIO
	}
	out := make([]nfs.DirEntry, 0, len(ents))
	for _, ent := range ents {
		crec, ok := e.byPath[path.Join(drec.path, ent.Name())]
		if !ok {
			// The index was built from the same walk fs.ReadDir draws
			// from; a name ReadDir reports but the index never saw would
			// mean fsys changed under us. Skip rather than fabricate.
			continue
		}
		out = append(out, nfs.DirEntry{Name: ent.Name(), Node: e.node(crec)})
	}
	return out, nil
}

func (e *export) ReadAt(n nfs.Node, p []byte, off int64) (int, error) {
	rec, ok := recordOf(n)
	if !ok {
		return 0, nfs.ErrIO
	}
	// A read at or past the end is an empty read, not an error, whatever
	// the underlying ReaderAt makes of the offset: fstest.MapFS refuses
	// one past the end outright.
	if off >= rec.size {
		return 0, nil
	}
	f, err := e.fsys.Open(rec.path)
	if err != nil {
		return 0, nfs.ErrIO
	}
	defer f.Close()
	ra, ok := f.(io.ReaderAt)
	if !ok {
		return 0, nfs.ErrIO
	}
	got, err := ra.ReadAt(p, off)
	// io.EOF is the normal end-of-file signal, not a failure: a read at or
	// past the end is a legitimate zero-length read the NFS client reads
	// as EOF.
	if err != nil && !errors.Is(err, io.EOF) {
		return got, nfs.ErrIO
	}
	return got, nil
}

// Readlink reports a symlink's target when fsys can read links. The
// assembled install tree never can: build.go resolves every symlink it
// walks, so there it always refuses.
func (e *export) Readlink(n nfs.Node) (string, error) {
	rec, ok := recordOf(n)
	if !ok || rec.mode&0o170000 != 0o120000 {
		return "", nfs.ErrIO
	}
	target, err := fs.ReadLink(e.fsys, rec.path)
	if err != nil {
		return "", nfs.ErrIO
	}
	return target, nil
}
