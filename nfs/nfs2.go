package nfs

import (
	"cmp"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"time"
)

// NFSv2 status codes (RFC 1094 section 2.3.1). They are errno values,
// and NFSv3 keeps the same numbers for the ones it shares.
const (
	nfsOK        = 0
	nfsErrPerm   = 1
	nfsErrNoent  = 2
	nfsErrIO     = 5
	nfsErrNotdir = 20
	nfsErrROFS   = 30
	nfsErrStale  = 70
)

// NFSv2 file types.
const (
	nfNON = 0
	nfREG = 1
	nfDIR = 2
	nfLNK = 5
)

const fhSize = 32

// nfs2Procs is the v2 procedure table (RFC 1094 section 2.2).
func (s *Server) nfs2Procs() map[uint32]handlerFunc {
	t := map[uint32]handlerFunc{
		0:  null,
		1:  s.nfsGetattr,
		2:  nfsROFS, // SETATTR
		3:  null,    // ROOT, obsolete
		4:  s.nfsLookup,
		5:  s.nfsReadlink,
		6:  s.nfsRead,
		7:  null,    // WRITECACHE, obsolete
		8:  nfsROFS, // WRITE
		9:  nfsROFS, // CREATE
		10: nfsROFS, // REMOVE
		11: nfsROFS, // RENAME
		12: nfsROFS, // LINK
		13: nfsROFS, // SYMLINK
		14: nfsROFS, // MKDIR
		15: nfsROFS, // RMDIR
		16: s.nfsReaddir,
		17: s.nfsStatfs,
	}
	if wfs, ok := s.FS.(WriteFS); ok {
		t[2] = func(_ net.Addr, r *xdrReader) []byte { return s.nfsSetattr(wfs, r) }
		t[8] = func(_ net.Addr, r *xdrReader) []byte { return s.nfsWrite(wfs, r) }
		t[9] = func(_ net.Addr, r *xdrReader) []byte { return s.nfsCreate(wfs, r, false) }
		t[10] = func(_ net.Addr, r *xdrReader) []byte { return s.nfsRemove(wfs, r, false) }
		t[11] = func(_ net.Addr, r *xdrReader) []byte { return s.nfsRename(wfs, r) }
		t[12] = nfsLink
		t[13] = func(_ net.Addr, r *xdrReader) []byte { return s.nfsSymlink(wfs, r) }
		t[14] = func(_ net.Addr, r *xdrReader) []byte { return s.nfsCreate(wfs, r, true) }
		t[15] = func(_ net.Addr, r *xdrReader) []byte { return s.nfsRemove(wfs, r, true) }
	}
	return t
}

// encodeFH packs a node id into the 32-byte filehandle: a version byte,
// then the id, zero-padded.
func encodeFH(id uint64) []byte {
	fh := make([]byte, fhSize)
	fh[0] = 1
	binary.BigEndian.PutUint64(fh[1:], id)
	return fh
}

func decodeFH(fh []byte) (uint64, bool) {
	if len(fh) != fhSize || fh[0] != 1 {
		return 0, false
	}
	return binary.BigEndian.Uint64(fh[1:]), true
}

// resolve turns a filehandle into a node, through the FS's own handles
// when it has them.
func (s *Server) resolve(fh []byte) (Node, error) {
	if h, ok := s.FS.(HandleFS); ok {
		return h.NodeByHandle(fh)
	}
	id, ok := decodeFH(fh)
	if !ok {
		return nil, ErrBadHandle
	}
	return s.FS.NodeByID(id)
}

func (s *Server) handle(n Node) []byte {
	if h, ok := s.FS.(HandleFS); ok {
		return h.Handle(n)
	}
	return encodeFH(n.ID())
}

// statuses maps FS errors to status values. Most are errno values both
// versions share; BADHANDLE and NOTSUPP exist only in NFSv3, and
// status2For substitutes for them.
var statuses = []struct {
	err  error
	code uint32
}{
	{ErrPerm, nfsErrPerm}, {ErrNotFound, nfsErrNoent}, {ErrIO, nfsErrIO},
	{ErrAccess, 13}, {ErrExist, 17}, {ErrXDev, 18}, {ErrNotDir, nfsErrNotdir},
	{ErrIsDir, 21}, {ErrInval, 22}, {ErrFBig, 27}, {ErrNoSpace, 28},
	{ErrROFS, nfsErrROFS}, {ErrNameTooLong, 63}, {ErrNotEmpty, 66},
	{ErrQuota, 69}, {ErrStale, nfsErrStale},
	{ErrBadHandle, 10001}, {ErrNotSupp, 10004},
}

// statusFor maps an FS error to an NFSv3 status, or a MOUNT status of
// either version. Anything unrecognized is an I/O error.
func statusFor(err error) uint32 {
	if err == nil {
		return nfsOK
	}
	for _, s := range statuses {
		if errors.Is(err, s.err) {
			return s.code
		}
	}
	return nfsErrIO
}

// status2For is statusFor for NFSv2, which has no BADHANDLE: a handle
// this server never issued is as stale as one it forgot.
func status2For(err error) uint32 {
	switch {
	case errors.Is(err, ErrBadHandle):
		return nfsErrStale
	case errors.Is(err, ErrNotSupp):
		return nfsErrIO
	}
	return statusFor(err)
}

func ftype(mode uint32) uint32 {
	switch mode & 0o170000 {
	case 0o040000:
		return nfDIR
	case 0o120000:
		return nfLNK
	case 0o100000:
		return nfREG
	default:
		return nfNON
	}
}

// clamp32 fits a size into an NFSv2 32-bit field.
func clamp32(v uint64) uint32 { return uint32(min(v, math.MaxUint32)) }

// writeFattr writes an NFSv2 fattr (68 bytes). The guest sees every
// file owned by root. Blocks are 512 bytes, matching EFS.
func writeFattr(w *xdrWriter, n Node) {
	a := attrOf(n)
	w.uint32(ftype(a.Mode))
	w.uint32(a.Mode)
	w.uint32(a.Nlink)
	w.uint32(0) // uid
	w.uint32(0) // gid
	w.uint32(clamp32(a.Size))
	w.uint32(512) // blocksize
	w.uint32(0)   // rdev
	w.uint32(clamp32(a.Used / 512))
	w.uint32(1) // fsid
	w.uint32(uint32(a.Fileid))
	for _, t := range []time.Time{a.Atime, a.Mtime, a.Ctime} {
		w.uint32(uint32(t.Unix()))
		w.uint32(uint32(t.Nanosecond() / 1000))
	}
}

// node resolves a filehandle to a node, or writes nothing and returns
// nil with a status the caller emits.
func (s *Server) node(fh []byte) (Node, uint32) {
	n, err := s.resolve(fh)
	if err != nil {
		return nil, status2For(err)
	}
	return n, nfsOK
}

// attrstat writes the reply most v2 procedures give: a status, then on
// success the attributes of the node fh names, read afresh.
func (s *Server) attrstat(fh []byte, err error) []byte {
	w := &xdrWriter{}
	if err != nil {
		w.uint32(status2For(err))
		return w.b
	}
	n, st := s.node(fh)
	w.uint32(st)
	if st == nfsOK {
		writeFattr(w, n)
	}
	return w.b
}

func (s *Server) nfsGetattr(_ net.Addr, r *xdrReader) []byte {
	return s.attrstat(r.bytes(fhSize), nil)
}

func (s *Server) nfsLookup(_ net.Addr, r *xdrReader) []byte {
	fh := r.bytes(fhSize)
	name := r.str()
	w := &xdrWriter{}
	dir, st := s.node(fh)
	if st != nfsOK {
		w.uint32(st)
		return w.b
	}
	child, err := s.FS.Lookup(dir, name)
	if err != nil {
		w.uint32(status2For(err))
		return w.b
	}
	s.logOpen(child)
	w.uint32(nfsOK)
	w.fixed(s.handle(child))
	writeFattr(w, child)
	return w.b
}

// logOpen logs a regular file LOOKUP resolved to - the NFS equivalent
// of an open, since a client LOOKUPs a file once and READs the handle it
// gets back rather than reopening it. A directory component on the way
// there doesn't log: this is a manifest of files served, not a trace of
// every path element walked.
func (s *Server) logOpen(n Node) {
	if ftype(n.Mode()) == nfREG {
		if d, ok := n.(Describer); ok {
			s.Logger.Infof("nfs: opened %s", d.Describe())
		}
	}
}

func (s *Server) nfsReadlink(_ net.Addr, r *xdrReader) []byte {
	fh := r.bytes(fhSize)
	w := &xdrWriter{}
	n, st := s.node(fh)
	if st != nfsOK {
		w.uint32(st)
		return w.b
	}
	target, err := s.FS.Readlink(n)
	if err != nil {
		w.uint32(status2For(err))
		return w.b
	}
	w.uint32(nfsOK)
	w.opaque([]byte(target))
	return w.b
}

// maxRead bounds a single READ reply so it fits a UDP datagram.
const maxRead = 8192

func (s *Server) nfsRead(_ net.Addr, r *xdrReader) []byte {
	fh := r.bytes(fhSize)
	offset := r.uint32()
	count := r.uint32()
	r.uint32() // totalcount, ignored per spec
	w := &xdrWriter{}
	n, st := s.node(fh)
	if st != nfsOK {
		w.uint32(st)
		return w.b
	}
	if count > maxRead {
		count = maxRead
	}
	buf := make([]byte, count)
	got, err := s.FS.ReadAt(n, buf, int64(offset))
	if err != nil && got == 0 {
		w.uint32(status2For(err))
		return w.b
	}
	w.uint32(nfsOK)
	writeFattr(w, n)
	w.opaque(buf[:got])
	return w.b
}

// nfsROFS refuses a v2 procedure that would change the export, without
// decoding its arguments: every such reply is a bare status on failure.
func nfsROFS(net.Addr, *xdrReader) []byte {
	w := &xdrWriter{}
	w.uint32(nfsErrROFS)
	return w.b
}

// nfsLink refuses hard links. NFSv2 has no NOTSUPP; PERM is what link(2)
// returns for a filesystem without hard links.
func nfsLink(net.Addr, *xdrReader) []byte {
	w := &xdrWriter{}
	w.uint32(nfsErrPerm)
	return w.b
}

// readTime2 reads an NFSv2 timeval from a sattr. All ones leaves the
// time alone; a microsecond count of one million asks for the server's
// clock, as Sun clients send for utimes(path, NULL).
func readTime2(r *xdrReader) *time.Time {
	sec, usec := r.uint32(), r.uint32()
	switch {
	case sec == math.MaxUint32:
		return nil
	case usec == 1000000:
		t := time.Now()
		return &t
	}
	t := time.Unix(int64(sec), int64(usec)*1000)
	return &t
}

// readSattr2 reads an NFSv2 sattr, where all ones means "don't set".
func readSattr2(r *xdrReader) SetAttr {
	var a SetAttr
	if m := r.uint32(); m != math.MaxUint32 {
		m &= 0o7777
		a.Mode = &m
	}
	r.uint32() // uid
	r.uint32() // gid
	if sz := r.uint32(); sz != math.MaxUint32 {
		v := uint64(sz)
		a.Size = &v
	}
	a.Atime = readTime2(r)
	a.Mtime = readTime2(r)
	return a
}

// dirop reads diropargs and resolves the directory, refusing a
// non-directory.
func (s *Server) dirop(r *xdrReader) (Node, string, error) {
	fh := r.bytes(fhSize)
	name := r.str()
	if r.err {
		return nil, "", nil
	}
	dir, err := s.resolve(fh)
	if err == nil && ftype(dir.Mode()) != nfDIR {
		err = ErrNotDir
	}
	return dir, name, err
}

func (s *Server) nfsSetattr(wfs WriteFS, r *xdrReader) []byte {
	fh := r.bytes(fhSize)
	a := readSattr2(r)
	if r.err {
		return nil
	}
	n, err := s.resolve(fh)
	if err == nil {
		err = wfs.SetAttr(n, a)
	}
	return s.attrstat(fh, err)
}

func (s *Server) nfsWrite(wfs WriteFS, r *xdrReader) []byte {
	fh := r.bytes(fhSize)
	r.uint32() // beginoffset, unused (RFC 1094 section 2.2.9)
	offset := r.uint32()
	r.uint32() // totalcount, unused
	data := r.opaque(maxRecord)
	if r.err {
		return nil
	}
	n, err := s.resolve(fh)
	if err == nil {
		err = writeTo(wfs, n, data, uint64(offset))
	}
	return s.attrstat(fh, err)
}

// writeTo checks n is a regular file and writes to it.
func writeTo(wfs WriteFS, n Node, data []byte, offset uint64) error {
	switch ftype(n.Mode()) {
	case nfREG:
	case nfDIR:
		return ErrIsDir
	default:
		return ErrInval
	}
	if offset > math.MaxInt64-uint64(len(data)) {
		return ErrFBig
	}
	return wfs.WriteAt(n, data, int64(offset))
}

// nfsCreate answers CREATE and, with mkdir set, MKDIR: both reply with
// the new node's handle and attributes. A v2 CREATE of a name that
// exists opens it and applies the attributes, so a size of zero
// truncates it.
func (s *Server) nfsCreate(wfs WriteFS, r *xdrReader, mkdir bool) []byte {
	dir, name, err := s.dirop(r)
	a := readSattr2(r)
	if r.err {
		return nil
	}
	var n Node
	if err == nil {
		if mkdir {
			n, err = wfs.Mkdir(dir, name, a)
		} else {
			n, err = wfs.Create(dir, name, Unchecked, a, [8]byte{})
		}
	}
	w := &xdrWriter{}
	if err != nil {
		w.uint32(status2For(err))
		return w.b
	}
	fh := s.handle(n)
	if n, err = s.resolve(fh); err != nil {
		w.uint32(status2For(err))
		return w.b
	}
	w.uint32(nfsOK)
	w.fixed(fh)
	writeFattr(w, n)
	return w.b
}

func (s *Server) nfsRemove(wfs WriteFS, r *xdrReader, rmdir bool) []byte {
	dir, name, err := s.dirop(r)
	if r.err {
		return nil
	}
	if err == nil {
		if rmdir {
			err = wfs.Rmdir(dir, name)
		} else {
			err = wfs.Remove(dir, name)
		}
	}
	w := &xdrWriter{}
	w.uint32(status2For(err))
	return w.b
}

func (s *Server) nfsRename(wfs WriteFS, r *xdrReader) []byte {
	from, fromName, err1 := s.dirop(r)
	to, toName, err2 := s.dirop(r)
	if r.err {
		return nil
	}
	err := cmp.Or(err1, err2)
	if err == nil {
		err = wfs.Rename(from, fromName, to, toName)
	}
	w := &xdrWriter{}
	w.uint32(status2For(err))
	return w.b
}

func (s *Server) nfsSymlink(wfs WriteFS, r *xdrReader) []byte {
	dir, name, err := s.dirop(r)
	target := r.str()
	a := readSattr2(r)
	if r.err {
		return nil
	}
	if err == nil {
		_, err = wfs.Symlink(dir, name, target, a)
	}
	w := &xdrWriter{}
	w.uint32(status2For(err))
	return w.b
}

// readdirStart is the index of the first entry after cookie. Entries
// with their own cookies resume after the last one the client saw, so
// a listing survives entries removed before it; others are numbered by
// position.
func readdirStart(ents []DirEntry, cookie uint64) int {
	if len(ents) > 0 && ents[0].Cookie != 0 {
		for i, e := range ents {
			if uint64(e.Cookie) > cookie {
				return i
			}
		}
		return len(ents)
	}
	return int(min(cookie, uint64(len(ents))))
}

func entryCookie(ents []DirEntry, i int) uint64 {
	if c := ents[i].Cookie; c != 0 {
		return uint64(c)
	}
	return uint64(i + 1)
}

func (s *Server) nfsReaddir(_ net.Addr, r *xdrReader) []byte {
	fh := r.bytes(fhSize)
	cookie := r.uint32()
	count := r.uint32()
	w := &xdrWriter{}
	dir, st := s.node(fh)
	if st != nfsOK {
		w.uint32(st)
		return w.b
	}
	ents, err := s.FS.ReadDir(dir)
	if err != nil {
		w.uint32(status2For(err))
		return w.b
	}
	w.uint32(nfsOK)
	budget := int(count)
	start := readdirStart(ents, uint64(cookie))
	i := start
	for ; i < len(ents); i++ {
		e := ents[i]
		entrySize := 4 + 4 + 4 + ((len(e.Name) + 3) &^ 3) + 4
		if budget-entrySize < 0 && i > start {
			break
		}
		budget -= entrySize
		w.uint32(1) // value follows
		w.uint32(uint32(attrOf(e.Node).Fileid))
		w.opaque([]byte(e.Name))
		w.uint32(uint32(entryCookie(ents, i)))
	}
	w.uint32(0) // no more entries in this reply
	w.bool(i >= len(ents))
	return w.b
}

func (s *Server) nfsStatfs(_ net.Addr, r *xdrReader) []byte {
	fh := r.bytes(fhSize)
	w := &xdrWriter{}
	if _, st := s.node(fh); st != nfsOK {
		w.uint32(st)
		return w.b
	}
	sp, err := s.space()
	if err != nil {
		w.uint32(status2For(err))
		return w.b
	}
	// Without capacity to report, the export is read-only media: 512-byte
	// blocks, none free. With it, 4 KiB blocks reach 16 TiB in 32 bits.
	bsize := uint64(512)
	if sp.Total != 0 {
		bsize = 4096
	}
	w.uint32(nfsOK)
	w.uint32(maxRead) // tsize
	w.uint32(uint32(bsize))
	w.uint32(clamp32(sp.Total / bsize))
	w.uint32(clamp32(sp.Free / bsize))
	w.uint32(clamp32(sp.Avail / bsize))
	return w.b
}

func (s *Server) space() (Space, error) {
	if sfs, ok := s.FS.(SpaceFS); ok {
		return sfs.Space()
	}
	return Space{}, nil
}
