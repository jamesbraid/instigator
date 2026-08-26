package vfs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jamesbraid/instigator/efs"
)

// Tree is the assembled install-set filesystem: one directory per
// configured set, each the ordered merge of that set's layers, plus
// whatever instigator generates in memory. It is read-only and safe for
// concurrent use once Build returns.
//
// Names are io/fs names throughout - "." for the root, no leading slash,
// no ".." - so a client can only ever address a path the build itself
// materialized. Protocol adapters strip or add the leading slash their
// wire format wants.
type Tree struct {
	root    *node
	closers []io.Closer
	nextIno uint64
}

// File is an opened regular file: a standard fs.File that also supports
// random access, which TFTP and instcmd need.
type File interface {
	fs.File
	io.ReaderAt
	Size() int64
}

// Metadata is FileInfo.Sys() for a tree path: the fields ls -l/-i and the
// recorder need beyond fs.FileInfo. Ino is the stable per-path inode.
type Metadata struct {
	Ino    uint64
	UID    uint32
	GID    uint32
	Nlink  int
	Origin Origin
}

// node is one materialized path. A directory holds children; a regular
// file holds only what it takes to open its bytes later, so assembling a
// tree never reads media content.
type node struct {
	name   string
	dir    bool
	ino    uint64
	origin Origin

	perm  fs.FileMode
	size  int64
	mtime time.Time
	uid   uint32
	gid   uint32
	nlink int

	children map[string]*node // directories

	fsys fs.FS  // backed files: the source filesystem, read at origin.Path
	data []byte // generated files
}

// newDir makes a merged directory node. A merge of layers has no inode of
// its own, so it reports what a directory on the media reports: one EFS
// directory block, linked from itself and its parent.
func newDir(name string, origin Origin) *node {
	return &node{
		name:     name,
		dir:      true,
		origin:   origin,
		size:     512,
		nlink:    2,
		children: map[string]*node{},
	}
}

// errNotExist is what a missing path reports. It answers both
// errors.Is(err, ErrNotFound) - instigator's own sentinel - and
// errors.Is(err, fs.ErrNotExist), which io/fs callers test; ErrNotFound is
// a plain sentinel and cannot answer the second on its own.
type errNotExist struct{}

func (errNotExist) Error() string { return ErrNotFound.Error() }

func (errNotExist) Is(target error) bool {
	return target == ErrNotFound || target == fs.ErrNotExist
}

var errNotDir = errors.New("not a directory")

var errIsDir = errors.New("is a directory")

// Open opens a tree path. A directory opens as an fs.ReadDirFile; a
// regular file opens as a File, so a caller that needs random access can
// assert for it.
func (t *Tree) Open(name string) (fs.File, error) {
	n, err := t.find("open", name)
	if err != nil {
		return nil, err
	}
	f, err := n.open()
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return f, nil
}

// ReadDir lists a tree directory, sorted lexically.
func (t *Tree) ReadDir(name string) ([]fs.DirEntry, error) {
	n, err := t.find("readdir", name)
	if err != nil {
		return nil, err
	}
	if !n.dir {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: errNotDir}
	}
	return n.entries(), nil
}

// Stat describes a tree path. Sys reports a *Metadata: the stable inode,
// the owner and link count, and the resolved origin.
func (t *Tree) Stat(name string) (fs.FileInfo, error) {
	n, err := t.find("stat", name)
	if err != nil {
		return nil, err
	}
	return fileInfo{n}, nil
}

// Resolve reports where a tree path's bytes come from: the configured
// layer or generator, and the path within it. Every file resolves,
// generated ones included, so a served-file manifest line can always name
// its source. The tree root and the set directories are structural - no
// single layer backs them - and resolve to the zero Origin.
func (t *Tree) Resolve(name string) (Origin, error) {
	n, err := t.find("resolve", name)
	if err != nil {
		return Origin{}, err
	}
	return n.origin, nil
}

// Close releases every source the build opened.
func (t *Tree) Close() error {
	var first error
	for _, c := range t.closers {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	t.closers = nil
	return first
}

// find resolves a name to its node, reporting the io/fs errors callers
// test for: fs.ErrInvalid for a name io/fs would never produce, and both
// ErrNotFound and fs.ErrNotExist for one that simply is not there.
func (t *Tree) find(op, name string) (*node, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	n, ok := t.lookup(name)
	if !ok {
		return nil, &fs.PathError{Op: op, Path: name, Err: errNotExist{}}
	}
	return n, nil
}

// lookup walks a valid io/fs name to its node.
func (t *Tree) lookup(name string) (*node, bool) {
	n := t.root
	if name == "." {
		return n, true
	}
	for _, part := range strings.Split(name, "/") {
		if !n.dir {
			return nil, false
		}
		child, ok := n.children[part]
		if !ok {
			return nil, false
		}
		n = child
	}
	return n, true
}

// number assigns an inode to every node without one, by a lexical
// pre-order walk starting at the root's own well-known number. Numbering
// only the unassigned means a file generated after the build appends
// instead of shifting an inode a client has already seen.
func (t *Tree) number() {
	if t.nextIno < uint64(efs.RootIno) {
		t.nextIno = uint64(efs.RootIno)
	}
	var walk func(*node)
	walk = func(n *node) {
		if n.ino == 0 {
			n.ino = t.nextIno
			t.nextIno++
		}
		for _, name := range sortedNames(n.children) {
			walk(n.children[name])
		}
	}
	walk(t.root)
}

// entries returns a directory's children as sorted fs.DirEntry values.
func (n *node) entries() []fs.DirEntry {
	names := sortedNames(n.children)
	out := make([]fs.DirEntry, 0, len(names))
	for _, name := range names {
		out = append(out, fs.FileInfoToDirEntry(fileInfo{n.children[name]}))
	}
	return out
}

func sortedNames(children map[string]*node) []string {
	names := make([]string, 0, len(children))
	for name := range children {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// open opens the node's contents from whichever source backs it.
func (n *node) open() (fs.File, error) {
	if n.dir {
		return &dirFile{n: n, ents: n.entries()}, nil
	}
	return n.openRegular()
}

func (n *node) openRegular() (File, error) {
	if n.origin.Kind == OriginGenerated {
		return &memFile{n: n, r: bytes.NewReader(n.data)}, nil
	}
	f, err := n.fsys.Open(n.origin.Path)
	if err != nil {
		return nil, err
	}
	ra, ok := f.(io.ReaderAt)
	if !ok {
		f.Close()
		return nil, fmt.Errorf("%s: source file does not support random access", n.origin.Path)
	}
	return &fsFile{n: n, f: f, ra: ra}, nil
}

// fileInfo is a node's fs.FileInfo. A directory reports a fixed
// read-only mode because it is a merge of layers rather than any one
// layer's inode; a regular file reports its own source's metadata.
type fileInfo struct{ n *node }

func (i fileInfo) Name() string { return i.n.name }
func (i fileInfo) Size() int64  { return i.n.size }

func (i fileInfo) Mode() fs.FileMode {
	if i.n.dir {
		return fs.ModeDir | 0o755
	}
	return i.n.perm
}

func (i fileInfo) ModTime() time.Time { return i.n.mtime }
func (i fileInfo) IsDir() bool        { return i.n.dir }

func (i fileInfo) Sys() any {
	return &Metadata{
		Ino:    i.n.ino,
		UID:    i.n.uid,
		GID:    i.n.gid,
		Nlink:  i.n.nlink,
		Origin: i.n.origin,
	}
}

// dirFile is an opened directory. ReadDir walks the snapshot taken at
// open, so a listing stays consistent across partial reads.
type dirFile struct {
	n    *node
	ents []fs.DirEntry
	off  int
}

func (d *dirFile) Stat() (fs.FileInfo, error) { return fileInfo{d.n}, nil }
func (d *dirFile) Close() error               { return nil }

func (d *dirFile) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.n.name, Err: errIsDir}
}

func (d *dirFile) ReadDir(count int) ([]fs.DirEntry, error) {
	rest := d.ents[d.off:]
	if count <= 0 {
		d.off = len(d.ents)
		return rest, nil
	}
	if len(rest) == 0 {
		return nil, io.EOF
	}
	if count > len(rest) {
		count = len(rest)
	}
	d.off += count
	return rest[:count], nil
}

// fsFile reads a backed regular file from its source filesystem. The same
// opened file provides both the sequential and the random-access reads, so
// serving a 600MB product file buffers nothing.
type fsFile struct {
	n  *node
	f  fs.File
	ra io.ReaderAt
}

func (f *fsFile) Stat() (fs.FileInfo, error)              { return fileInfo{f.n}, nil }
func (f *fsFile) Close() error                            { return f.f.Close() }
func (f *fsFile) Size() int64                             { return f.n.size }
func (f *fsFile) Read(p []byte) (int, error)              { return f.f.Read(p) }
func (f *fsFile) ReadAt(p []byte, off int64) (int, error) { return f.ra.ReadAt(p, off) }

// memFile serves a generated file's bytes.
type memFile struct {
	n *node
	r *bytes.Reader
}

func (f *memFile) Stat() (fs.FileInfo, error)              { return fileInfo{f.n}, nil }
func (f *memFile) Close() error                            { return nil }
func (f *memFile) Size() int64                             { return f.n.size }
func (f *memFile) Read(p []byte) (int, error)              { return f.r.Read(p) }
func (f *memFile) ReadAt(p []byte, off int64) (int, error) { return f.r.ReadAt(p, off) }
