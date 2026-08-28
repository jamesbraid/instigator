package efs

import (
	"errors"
	"io"
	"io/fs"
	"path"
	"sort"
	"time"
)

// FSys returns an io/fs.FS view of the filesystem: path-addressed and read
// only. It also satisfies fs.ReadDirFS, fs.StatFS and fs.ReadLinkFS.
// Symlinks are left unfollowed, exactly as Lookup leaves them, so a caller
// resolves them through ReadLink; opened regular files also satisfy
// io.ReaderAt, so a server random-accesses a product file without
// buffering it.
func (fs *FS) FSys() fs.FS { return &fsView{fs} }

// Stat is the FileInfo.Sys() value for every entry an EFS fs view returns:
// the owner, link count, and full mode io/fs.FileInfo cannot carry on its
// own. Mode is the raw inode mode, including the setuid, setgid, and
// sticky bits that FileInfo.Mode() masks away, so a caller that serves
// those bits can recover them.
type Stat struct {
	UID   uint16
	GID   uint16
	Nlink int16
	Mode  uint16
}

type fsView struct{ fs *FS }

// resolve maps an io/fs name to its inode without following a trailing
// symlink. "." is the root; a name io/fs would never produce is ErrInvalid;
// a name that is simply absent is ErrNotExist.
func (v *fsView) resolve(op, name string) (*Inode, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	if name == "." {
		ino, err := v.fs.Inode(RootIno)
		if err != nil {
			return nil, &fs.PathError{Op: op, Path: name, Err: err}
		}
		return ino, nil
	}
	ino, err := v.fs.Lookup(name)
	if err != nil {
		// Only a genuine absence is ErrNotExist. An I/O or corruption error -
		// including a failed re-fetch of an evicted chunk behind a remote
		// image - must surface as itself, not as a misleading "not found" that
		// would make a served file look deleted mid-install.
		if errors.Is(err, ErrNotFound) {
			return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
		}
		return nil, &fs.PathError{Op: op, Path: name, Err: err}
	}
	return ino, nil
}

func (v *fsView) Open(name string) (fs.File, error) {
	ino, err := v.resolve("open", name)
	if err != nil {
		return nil, err
	}
	info := fsInfo{name: path.Base(name), ino: ino}
	if ino.IsDir() {
		ents, err := v.dirEntries(name, ino)
		if err != nil {
			return nil, err
		}
		return &fsDir{info: info, ents: ents}, nil
	}
	return &fsFile{fs: v.fs, ino: ino, info: info}, nil
}

func (v *fsView) Stat(name string) (fs.FileInfo, error) {
	ino, err := v.resolve("stat", name)
	if err != nil {
		return nil, err
	}
	return fsInfo{name: path.Base(name), ino: ino}, nil
}

func (v *fsView) ReadDir(name string) ([]fs.DirEntry, error) {
	ino, err := v.resolve("readdir", name)
	if err != nil {
		return nil, err
	}
	if !ino.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	return v.dirEntries(name, ino)
}

func (v *fsView) ReadLink(name string) (string, error) {
	ino, err := v.resolve("readlink", name)
	if err != nil {
		return "", err
	}
	if !ino.IsSymlink() {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: fs.ErrInvalid}
	}
	return v.fs.Readlink(ino)
}

// Lstat is Stat: resolve never follows a trailing symlink, so the two
// already agree. It exists to satisfy fs.ReadLinkFS, which requires both.
func (v *fsView) Lstat(name string) (fs.FileInfo, error) {
	return v.Stat(name)
}

// dirEntries reads a directory's children, reading each child's inode so
// the entry can report its type and owner. "." and ".." are dropped, as
// io/fs directory listings never include them.
func (v *fsView) dirEntries(name string, ino *Inode) ([]fs.DirEntry, error) {
	raw, err := v.fs.ReadDir(ino)
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	out := make([]fs.DirEntry, 0, len(raw))
	for _, e := range raw {
		if e.Name == "." || e.Name == ".." {
			continue
		}
		child, err := v.fs.Inode(e.Ino)
		if err != nil {
			return nil, &fs.PathError{Op: "readdir", Path: path.Join(name, e.Name), Err: err}
		}
		out = append(out, fs.FileInfoToDirEntry(fsInfo{name: e.Name, ino: child}))
	}
	// fs.ReadDirFS requires entries sorted by name; EFS stores them in
	// directory-slot order, so sort before returning for a deterministic walk.
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// fsInfo is an inode's fs.FileInfo. Mode carries the type bits io/fs
// expects; Sys carries the EFS owner and link count.
type fsInfo struct {
	name string
	ino  *Inode
}

func (i fsInfo) Name() string { return i.name }
func (i fsInfo) Size() int64  { return int64(i.ino.Size) }

func (i fsInfo) Mode() fs.FileMode {
	m := fs.FileMode(i.ino.Mode & 0o777)
	switch {
	case i.ino.IsDir():
		m |= fs.ModeDir
	case i.ino.IsSymlink():
		m |= fs.ModeSymlink
	case !i.ino.IsRegular():
		// A FIFO, device, or socket real media occasionally carries is
		// none of the three types io/fs names on its own, and it must not
		// read as a regular file: a caller asserting Mode().IsRegular()
		// - as the tree walker does when it refuses to serve a link that
		// lands on a special file - has to see it is not one.
		m |= fs.ModeIrregular
	}
	return m
}

func (i fsInfo) ModTime() time.Time { return time.Unix(int64(i.ino.Mtime), 0) }
func (i fsInfo) IsDir() bool        { return i.ino.IsDir() }
func (i fsInfo) Sys() any {
	return &Stat{UID: i.ino.UID, GID: i.ino.GID, Nlink: i.ino.Nlink, Mode: i.ino.Mode}
}

// fsFile is an opened regular file. It reads straight from the image
// through the extent reader, so nothing is buffered.
type fsFile struct {
	fs   *FS
	ino  *Inode
	info fsInfo
	off  int64
}

func (f *fsFile) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *fsFile) Close() error               { return nil }

func (f *fsFile) Read(p []byte) (int, error) {
	n, err := f.fs.ReadAt(f.ino, p, f.off)
	f.off += int64(n)
	return n, err
}

func (f *fsFile) ReadAt(p []byte, off int64) (int, error) {
	return f.fs.ReadAt(f.ino, p, off)
}

// fsDir is an opened directory, listing the snapshot taken at open.
type fsDir struct {
	info fsInfo
	ents []fs.DirEntry
	off  int
}

func (d *fsDir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *fsDir) Close() error               { return nil }
func (d *fsDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.name, Err: fs.ErrInvalid}
}

func (d *fsDir) ReadDir(count int) ([]fs.DirEntry, error) {
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
