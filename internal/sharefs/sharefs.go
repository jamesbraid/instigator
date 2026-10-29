// Package sharefs exports a host directory for the nfs server to read
// and write, under the rules a share follows:
//
//   - Nothing outside the directory is ever reached. Every operation goes
//     through an os.Root, which refuses ".." and symlinks that lead out,
//     and a name from the client must be a single path element. A
//     symlink is served as a symlink; the client resolves it, not the
//     server.
//   - The guest sees every file owned by root, and every change lands
//     owned by whoever runs the server. A chown succeeds and changes
//     nothing. The setuid bit is never shown, never set, and is cleared
//     by a write.
//   - A filehandle carries the share's id and the file's device, inode
//     and generation, so it outlives a server restart and goes stale
//     when its file is replaced. Where the host has no usable inode
//     numbers (Windows), it carries an id from a table of paths kept in
//     a file instead.
package sharefs

import (
	"encoding/binary"
	"errors"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jamesbraid/instigator/nfs"
)

// Options configures Open.
type Options struct {
	// PathTable keeps filehandles in a table of paths saved in this file
	// instead of deriving them from inodes. Windows needs one; elsewhere
	// setting it chooses one.
	PathTable string

	// AttrFilter, when set, sees every attribute set before the server
	// does. The transcript tests use it to pin the times and sizes a host
	// chooses itself.
	AttrFilter func(*nfs.Attr)
}

// FS is one share. It implements nfs.WriteFS, nfs.HandleFS, nfs.SpaceFS
// and nfs.Exporter.
type FS struct {
	name     string
	dir      string
	root     *os.Root
	exportID uint32
	sys      sysFS
	filter   func(*nfs.Attr)

	// Exactly one of these names files: table when the share uses a path
	// table, otherwise cache, which remembers where each inode was last
	// seen.
	table *pathTable
	mu    sync.Mutex
	cache map[key]string
}

// key is what a handle carries to name a file. With a path table, ino
// is the table id and dev and gen are zero.
type key struct{ dev, ino, gen uint64 }

// Handle format bytes, so a handle from a share that changed how it
// names files is stale rather than misread.
const (
	fmtInode = 2
	fmtTable = 3
)

// Open exports dir as the share name, mounted as "/name".
func Open(name, dir string, opts Options) (*FS, error) {
	if name == "" || strings.ContainsAny(name, "/\\") {
		return nil, errors.New("sharefs: share name must be one path element")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(abs); err != nil {
		return nil, err
	} else if !fi.IsDir() {
		return nil, &fs.PathError{Op: "share", Path: abs, Err: nfs.ErrNotDir}
	}
	h := fnv.New32a()
	h.Write([]byte(name))
	f := &FS{name: name, dir: abs, exportID: h.Sum32(), filter: opts.AttrFilter}
	if opts.PathTable == "" && !inodesUsable {
		return nil, errors.New("sharefs: this host has no stable inode numbers, so a share needs a path table")
	}
	if f.root, err = os.OpenRoot(abs); err != nil {
		return nil, err
	}
	if f.sys, err = openSys(abs); err != nil {
		f.root.Close()
		return nil, err
	}
	if opts.PathTable != "" {
		if f.table, err = openPathTable(opts.PathTable); err != nil {
			f.Close()
			return nil, err
		}
	} else {
		f.cache = map[key]string{}
	}
	return f, nil
}

// Close releases the share's directory and path table.
func (f *FS) Close() error {
	var errs []error
	if f.table != nil {
		errs = append(errs, f.table.close())
	}
	errs = append(errs, f.sys.close(), f.root.Close())
	return errors.Join(errs...)
}

// Dir is the host directory the share exports.
func (f *FS) Dir() string { return f.dir }

// Exports lists the one mount path.
func (f *FS) Exports() []string { return []string{"/" + f.name} }

// node is a file as last statted.
type node struct {
	path string // slash-separated, relative to the share; "." is its root
	raw  uint32 // host mode with type bits, setuid included
	attr nfs.Attr
	key  key
}

func (n *node) ID() uint64     { return n.attr.Fileid }
func (n *node) Mode() uint32   { return n.attr.Mode }
func (n *node) Size() int64    { return int64(n.attr.Size) }
func (n *node) Mtime() uint32  { return uint32(n.attr.Mtime.Unix()) }
func (n *node) Attr() nfs.Attr { return n.attr }

func (n *node) isDir() bool  { return n.raw&0o170000 == 0o040000 }
func (n *node) isReg() bool  { return n.raw&0o170000 == 0o100000 }
func (n *node) isLink() bool { return n.raw&0o170000 == 0o120000 }

// stat is what the platform reports for a file.
type stat struct {
	mode                uint32 // with type bits
	nlink               uint32
	size, used          uint64
	atime, mtime, ctime time.Time
	dev, ino, gen       uint64
}

// osPath turns a share path into one for f.root.
func osPath(rel string) string { return filepath.FromSlash(rel) }

func join(dir, name string) string {
	if dir == "." {
		return name
	}
	return dir + "/" + name
}

// statNode stats the file at rel, without following a final symlink,
// and describes it.
func (f *FS) statNode(rel string) (*node, error) {
	fi, err := f.root.Lstat(osPath(rel))
	if err != nil {
		return nil, mapErr(err)
	}
	st := fileStat(fi)
	st.gen = f.sys.generation(rel, st)
	return f.describe(rel, st)
}

func (f *FS) describe(rel string, st stat) (*node, error) {
	var k key
	if f.table != nil {
		id, err := f.table.idFor(rel)
		if err != nil {
			return nil, nfs.ErrIO
		}
		k = key{ino: id}
	} else {
		k = key{dev: st.dev, ino: st.ino, gen: st.gen}
		f.mu.Lock()
		f.cache[k] = rel
		f.mu.Unlock()
	}
	n := &node{path: rel, raw: st.mode, key: k, attr: nfs.Attr{
		Mode:   st.mode &^ 0o4000,
		Nlink:  st.nlink,
		Size:   st.size,
		Used:   st.used,
		Fileid: k.ino,
		Atime:  st.atime,
		Mtime:  st.mtime,
		Ctime:  st.ctime,
	}}
	if f.filter != nil {
		f.filter(&n.attr)
	}
	return n, nil
}

// own recovers a node this FS handed out.
func own(n nfs.Node) (*node, error) {
	nn, ok := n.(*node)
	if !ok {
		return nil, nfs.ErrStale
	}
	return nn, nil
}

func ownDir(n nfs.Node) (*node, error) {
	d, err := own(n)
	if err == nil && !d.isDir() {
		err = nfs.ErrNotDir
	}
	return d, err
}

// checkName refuses a name that is not one ordinary directory entry.
// Joined onto a path, ".." or a separator would reach another
// directory, possibly outside the share.
func checkName(name string) error {
	switch {
	case name == "" || name == "." || name == "..":
		return nfs.ErrInval
	case strings.ContainsRune(name, '/') || strings.ContainsRune(name, 0) || strings.ContainsAny(name, badNameChars):
		return nfs.ErrInval
	case len(name) > 255:
		return nfs.ErrNameTooLong
	}
	return nil
}

// MountRoot resolves "/name", or a directory beneath it, to its node.
func (f *FS) MountRoot(p string) (nfs.Node, error) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if parts[0] != f.name {
		return nil, nfs.ErrNotFound
	}
	n, err := f.statNode(".")
	if err != nil {
		return nil, err
	}
	for _, part := range parts[1:] {
		if part == "" {
			continue
		}
		if checkName(part) != nil {
			return nil, nfs.ErrNotFound
		}
		if !n.isDir() {
			return nil, nfs.ErrNotDir
		}
		if n, err = f.statNode(join(n.path, part)); err != nil {
			return nil, err
		}
	}
	return n, nil
}

// NodeByID is unused: the server resolves this FS's own handles.
func (f *FS) NodeByID(uint64) (nfs.Node, error) { return nil, nfs.ErrStale }

// Handle encodes a node as 32 bytes: format, three zero bytes, the
// share id, then device, inode and generation.
func (f *FS) Handle(n nfs.Node) []byte {
	nn, err := own(n)
	if err != nil {
		return make([]byte, 32)
	}
	fh := make([]byte, 32)
	fh[0] = fmtInode
	if f.table != nil {
		fh[0] = fmtTable
	}
	binary.BigEndian.PutUint32(fh[4:], f.exportID)
	binary.BigEndian.PutUint64(fh[8:], nn.key.dev)
	binary.BigEndian.PutUint64(fh[16:], nn.key.ino)
	binary.BigEndian.PutUint64(fh[24:], nn.key.gen)
	return fh
}

// NodeByHandle finds the file a handle names. A handle is stale once
// its share id, inode or generation no longer matches a file in the
// share.
func (f *FS) NodeByHandle(fh []byte) (nfs.Node, error) {
	want := byte(fmtInode)
	if f.table != nil {
		want = fmtTable
	}
	if len(fh) != 32 || fh[1]|fh[2]|fh[3] != 0 || (fh[0] != fmtInode && fh[0] != fmtTable) {
		return nil, nfs.ErrBadHandle
	}
	if fh[0] != want || binary.BigEndian.Uint32(fh[4:]) != f.exportID {
		return nil, nfs.ErrStale
	}
	k := key{
		dev: binary.BigEndian.Uint64(fh[8:]),
		ino: binary.BigEndian.Uint64(fh[16:]),
		gen: binary.BigEndian.Uint64(fh[24:]),
	}
	if f.table != nil {
		rel, ok := f.table.path(k.ino)
		if !ok {
			return nil, nfs.ErrStale
		}
		n, err := f.statNode(rel)
		if errors.Is(err, nfs.ErrNotFound) {
			return nil, nfs.ErrStale
		}
		return n, err
	}
	f.mu.Lock()
	rel, ok := f.cache[k]
	f.mu.Unlock()
	if ok {
		if n, err := f.statNode(rel); err == nil && n.key == k {
			return n, nil
		}
	}
	return f.search(k)
}

// search walks the share for the file k names, for a handle the cache
// doesn't place: one issued before a restart, or for a file moved on the
// host.
func (f *FS) search(k key) (nfs.Node, error) {
	queue := []string{"."}
	if n, err := f.statNode("."); err == nil && n.key == k {
		return n, nil
	}
	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		d, err := f.root.Open(osPath(dir))
		if err != nil {
			continue
		}
		ents, _ := d.ReadDir(-1)
		d.Close()
		for _, e := range ents {
			rel := join(dir, e.Name())
			if e.IsDir() {
				queue = append(queue, rel)
			}
			if info, err := e.Info(); err != nil || !sameInode(info, k) {
				continue
			}
			if n, err := f.statNode(rel); err == nil && n.key == k {
				return n, nil
			}
		}
	}
	return nil, nfs.ErrStale
}

// Lookup finds name in dir. "." is dir itself and ".." its parent; the
// share's root is its own parent, as a mount point's is.
func (f *FS) Lookup(dir nfs.Node, name string) (nfs.Node, error) {
	d, err := ownDir(dir)
	if err != nil {
		return nil, err
	}
	switch name {
	case ".":
		return f.statNode(d.path)
	case "..":
		return f.statNode(path.Dir(d.path))
	}
	if checkName(name) != nil {
		return nil, nfs.ErrNotFound
	}
	return f.statNode(join(d.path, name))
}

// cookie is a directory entry's READDIR cookie: a hash of its name, so
// it stays put however the directory changes. Two names in one directory
// sharing a hash would make a listing that pauses between them skip the
// second; with 31 bits that is rare enough to accept.
func cookie(name string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(name))
	return max(h.Sum32()&0x7fffffff, 1)
}

// ReadDir lists dir in cookie order. "." and ".." are not listed.
func (f *FS) ReadDir(dir nfs.Node) ([]nfs.DirEntry, error) {
	d, err := ownDir(dir)
	if err != nil {
		return nil, err
	}
	fd, err := f.root.Open(osPath(d.path))
	if err != nil {
		return nil, mapErr(err)
	}
	ents, err := fd.ReadDir(-1)
	fd.Close()
	if err != nil {
		return nil, mapErr(err)
	}
	// Stat in name order, not the directory's own, so a path table numbers
	// new entries the same way on every host.
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	out := make([]nfs.DirEntry, 0, len(ents))
	for _, e := range ents {
		n, err := f.statNode(join(d.path, e.Name()))
		if err != nil {
			continue // removed since the listing
		}
		out = append(out, nfs.DirEntry{Name: e.Name(), Node: n, Cookie: cookie(e.Name())})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Cookie != out[j].Cookie {
			return out[i].Cookie < out[j].Cookie
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (f *FS) ReadAt(n nfs.Node, p []byte, off int64) (int, error) {
	nn, err := own(n)
	if err != nil {
		return 0, err
	}
	if !nn.isReg() {
		return 0, nfs.ErrInval
	}
	fd, err := f.root.Open(osPath(nn.path))
	if err != nil {
		return 0, mapErr(err)
	}
	defer fd.Close()
	got, err := fd.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return got, mapErr(err)
	}
	return got, nil
}

func (f *FS) Readlink(n nfs.Node) (string, error) {
	nn, err := own(n)
	if err != nil {
		return "", err
	}
	if !nn.isLink() {
		return "", nfs.ErrInval
	}
	target, err := f.root.Readlink(osPath(nn.path))
	return filepath.ToSlash(target), mapErr(err)
}

// fileMode converts Unix permission bits, less setuid, to an
// fs.FileMode for chmod.
func fileMode(m uint32) fs.FileMode {
	mode := fs.FileMode(m & 0o777)
	if m&0o2000 != 0 {
		mode |= fs.ModeSetgid
	}
	if m&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}

// SetAttr applies a client's changes. A symlink's mode and times are
// left alone: the host can't set them without following the link.
func (f *FS) SetAttr(n nfs.Node, a nfs.SetAttr) error {
	nn, err := own(n)
	if err != nil {
		return err
	}
	rel := osPath(nn.path)
	if a.Size != nil {
		switch {
		case nn.isDir():
			return nfs.ErrIsDir
		case !nn.isReg():
			return nfs.ErrInval
		}
		fd, err := f.root.OpenFile(rel, os.O_WRONLY, 0)
		if err != nil {
			return mapErr(err)
		}
		err = fd.Truncate(int64(min(*a.Size, 1<<63-1)))
		fd.Close()
		if err != nil {
			return mapErr(err)
		}
		f.clearSetuid(nn)
	}
	if nn.isLink() {
		return nil
	}
	if a.Mode != nil {
		if err := f.root.Chmod(rel, fileMode(*a.Mode)); err != nil {
			return mapErr(err)
		}
	}
	if a.Atime != nil || a.Mtime != nil {
		var at, mt time.Time // zero leaves a time as it is
		if a.Atime != nil {
			at = *a.Atime
		}
		if a.Mtime != nil {
			mt = *a.Mtime
		}
		if err := f.root.Chtimes(rel, at, mt); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

// clearSetuid drops setuid, and setgid on a group-executable file, after
// a change to the file's contents, as a local write would.
func (f *FS) clearSetuid(n *node) {
	strip := uint32(0o4000)
	if n.raw&0o010 != 0 {
		strip |= 0o2000
	}
	if n.raw&strip != 0 {
		f.root.Chmod(osPath(n.path), fileMode(n.raw&^strip))
	}
}

// WriteAt writes to the host file before returning. It is not synced to
// disk: the guest runs on this host, so a crash that loses the host's
// cache loses the guest too.
func (f *FS) WriteAt(n nfs.Node, p []byte, off int64) error {
	nn, err := own(n)
	if err != nil {
		return err
	}
	if !nn.isReg() {
		return nfs.ErrInval
	}
	fd, err := f.root.OpenFile(osPath(nn.path), os.O_WRONLY, 0)
	if err != nil {
		return mapErr(err)
	}
	_, err = fd.WriteAt(p, off)
	if cerr := fd.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return mapErr(err)
	}
	f.clearSetuid(nn)
	return nil
}

// newEntry checks a name for a new entry in dir and returns its path.
func newEntry(dir nfs.Node, name string) (string, error) {
	d, err := ownDir(dir)
	if err != nil {
		return "", err
	}
	if err := checkName(name); err != nil {
		return "", err
	}
	return join(d.path, name), nil
}

// verfTimes is where an exclusive create keeps its verifier: in the new
// file's access and modification seconds, until the client's SETATTR
// replaces them (RFC 1813 section 3.3.8).
func verfTimes(verf [8]byte) (time.Time, time.Time) {
	return time.Unix(int64(binary.BigEndian.Uint32(verf[:4])), 0),
		time.Unix(int64(binary.BigEndian.Uint32(verf[4:])), 0)
}

func (f *FS) Create(dir nfs.Node, name string, how nfs.CreateHow, a nfs.SetAttr, verf [8]byte) (nfs.Node, error) {
	rel, err := newEntry(dir, name)
	if err != nil {
		return nil, err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if how == nfs.Unchecked {
		if n, err := f.statNode(rel); err == nil {
			if !n.isReg() {
				return nil, nfs.ErrExist
			}
			return n, f.SetAttr(n, a)
		}
		flags = os.O_WRONLY | os.O_CREATE
	}
	perm := uint32(0o644)
	if a.Mode != nil {
		perm = *a.Mode
	}
	fd, err := f.root.OpenFile(osPath(rel), flags, fs.FileMode(perm&0o777))
	if err != nil {
		if how == nfs.Exclusive && errors.Is(err, fs.ErrExist) {
			return f.retransmitted(rel, verf)
		}
		return nil, mapErr(err)
	}
	fd.Close()
	if how == nfs.Exclusive {
		at, mt := verfTimes(verf)
		a = nfs.SetAttr{Atime: &at, Mtime: &mt}
	}
	// Set the mode outright: the open applied the server's umask, and
	// the client has applied the guest's already.
	a.Mode = &perm
	n, err := f.statNode(rel)
	if err != nil {
		return nil, err
	}
	if err := f.SetAttr(n, a); err != nil {
		return nil, err
	}
	return f.statNode(rel)
}

// retransmitted answers an exclusive create of a name that exists: the
// same request again if the file carries its verifier, else EXIST.
func (f *FS) retransmitted(rel string, verf [8]byte) (nfs.Node, error) {
	n, err := f.statNode(rel)
	if err != nil {
		return nil, err
	}
	fi, err := f.root.Lstat(osPath(rel))
	if err != nil {
		return nil, mapErr(err)
	}
	at, mt := verfTimes(verf)
	st := fileStat(fi)
	if n.isReg() && st.atime.Equal(at) && st.mtime.Equal(mt) {
		return n, nil
	}
	return nil, nfs.ErrExist
}

func (f *FS) Mkdir(dir nfs.Node, name string, a nfs.SetAttr) (nfs.Node, error) {
	rel, err := newEntry(dir, name)
	if err != nil {
		return nil, err
	}
	if err := f.root.Mkdir(osPath(rel), 0o755); err != nil {
		return nil, mapErr(err)
	}
	n, err := f.statNode(rel)
	if err != nil {
		return nil, err
	}
	a.Size = nil
	if err := f.SetAttr(n, a); err != nil {
		return nil, err
	}
	return f.statNode(rel)
}

// Symlink stores target as the client gave it. The server never follows
// it, so a target outside the share is only text.
func (f *FS) Symlink(dir nfs.Node, name, target string, _ nfs.SetAttr) (nfs.Node, error) {
	rel, err := newEntry(dir, name)
	if err != nil {
		return nil, err
	}
	if target == "" || strings.ContainsRune(target, 0) {
		return nil, nfs.ErrInval
	}
	if err := f.root.Symlink(filepath.FromSlash(target), osPath(rel)); err != nil {
		return nil, mapErr(err)
	}
	return f.statNode(rel)
}

func (f *FS) Remove(dir nfs.Node, name string) error {
	return f.remove(dir, name, false)
}

func (f *FS) Rmdir(dir nfs.Node, name string) error {
	return f.remove(dir, name, true)
}

func (f *FS) remove(dir nfs.Node, name string, wantDir bool) error {
	rel, err := newEntry(dir, name)
	if err != nil {
		return err
	}
	n, err := f.statNode(rel)
	if err != nil {
		return err
	}
	switch {
	case wantDir && !n.isDir():
		return nfs.ErrNotDir
	case !wantDir && n.isDir():
		return nfs.ErrIsDir
	}
	if err := f.root.Remove(osPath(rel)); err != nil {
		return mapErr(err)
	}
	f.forget(rel)
	return nil
}

func (f *FS) Rename(fromDir nfs.Node, fromName string, toDir nfs.Node, toName string) error {
	from, err := newEntry(fromDir, fromName)
	if err != nil {
		return err
	}
	to, err := newEntry(toDir, toName)
	if err != nil {
		return err
	}
	if err := f.root.Rename(osPath(from), osPath(to)); err != nil {
		return mapErr(err)
	}
	if from == to {
		return nil
	}
	f.forget(to)
	f.moved(from, to)
	return nil
}

// under reports whether p is base or inside it.
func under(p, base string) bool {
	return p == base || strings.HasPrefix(p, base+"/")
}

// forget drops what the share remembers about rel and everything under
// it.
func (f *FS) forget(rel string) {
	if f.table != nil {
		f.table.remove(rel)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, p := range f.cache {
		if under(p, rel) {
			delete(f.cache, k)
		}
	}
}

// moved records a rename, so handles under the old path keep working
// without a search.
func (f *FS) moved(from, to string) {
	if f.table != nil {
		f.table.move(from, to)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, p := range f.cache {
		if under(p, from) {
			f.cache[k] = to + p[len(from):]
		}
	}
}

// Space reports the capacity of the filesystem holding the share.
func (f *FS) Space() (nfs.Space, error) {
	sp, err := diskSpace(f.dir)
	return sp, mapErr(err)
}

// mapErr converts a host error to the nfs error it is reported as.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	for _, e := range errnos {
		if errors.Is(err, e.errno) {
			return e.err
		}
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nfs.ErrNotFound
	case errors.Is(err, fs.ErrExist):
		return nfs.ErrExist
	case errors.Is(err, fs.ErrPermission):
		return nfs.ErrAccess
	}
	return nfs.ErrIO
}
