package nfs

import (
	"cmp"
	"math"
	"net"
	"time"
)

// NFSv3 status codes (RFC 1813 section 2.6) beyond those it shares with
// version 2, which keep their numbers.
const (
	nfs3ErrInval     = 22
	nfs3ErrIsdir     = 21
	nfs3ErrBadHandle = 10001
	nfs3ErrNotSync   = 10002
	nfs3ErrNotSupp   = 10004
	nfs3ErrTooSmall  = 10005
)

// maxRead3 is rtmax and wtmax. 32 KiB plus headers still fits one UDP
// datagram.
const maxRead3 = 32768

// ACCESS bits (RFC 1813 section 3.3.4).
const (
	access3Read    = 0x01
	access3Lookup  = 0x02
	access3Modify  = 0x04
	access3Extend  = 0x08
	access3Delete  = 0x10
	access3Execute = 0x20
)

// nfs3Procs is the v3 procedure table (RFC 1813 section 3.3). On an FS
// that isn't a WriteFS, every procedure that would change the export is
// refused with ROFS.
func (s *Server) nfs3Procs() map[uint32]handlerFunc {
	t := map[uint32]handlerFunc{
		0:  null,
		1:  s.nfs3Getattr,
		2:  rofs3(2), // SETATTR: obj_wcc
		3:  s.nfs3Lookup,
		4:  s.nfs3Access,
		5:  s.nfs3Readlink,
		6:  s.nfs3Read,
		7:  rofs3(2), // WRITE: file_wcc
		8:  rofs3(2), // CREATE: dir_wcc
		9:  rofs3(2), // MKDIR
		10: rofs3(2), // SYMLINK
		11: rofs3(2), // MKNOD
		12: rofs3(2), // REMOVE
		13: rofs3(2), // RMDIR
		14: rofs3(4), // RENAME: fromdir_wcc, todir_wcc
		15: rofs3(3), // LINK: file_attributes, linkdir_wcc
		16: s.nfs3Readdir,
		17: s.nfs3Readdirplus,
		18: s.nfs3Fsstat,
		19: s.nfs3Fsinfo,
		20: s.nfs3Pathconf,
		21: rofs3(2), // COMMIT: file_wcc
	}
	wfs, ok := s.FS.(WriteFS)
	if !ok {
		return t
	}
	with := func(f func(WriteFS, *xdrReader) []byte) handlerFunc {
		return func(_ net.Addr, r *xdrReader) []byte { return f(wfs, r) }
	}
	t[2] = with(s.nfs3Setattr)
	t[7] = with(s.nfs3Write)
	t[8] = with(s.nfs3Create)
	t[9] = with(s.nfs3Mkdir)
	t[10] = with(s.nfs3Symlink)
	t[11] = notsupp3(2) // MKNOD: no device nodes in a share
	t[12] = with(func(wfs WriteFS, r *xdrReader) []byte { return s.nfs3Remove(wfs, r, false) })
	t[13] = with(func(wfs WriteFS, r *xdrReader) []byte { return s.nfs3Remove(wfs, r, true) })
	t[14] = with(s.nfs3Rename)
	t[15] = notsupp3(3) // LINK: the export keeps one path per file
	t[21] = s.nfs3Commit
	return t
}

// rofs3 refuses a v3 procedure that would change the export, without
// decoding its arguments. A failure body is pre_op_attr and post_op_attr
// values with nothing to report, and empty counts them: two for each
// wcc_data, one for a lone post_op_attr.
func rofs3(empty int) handlerFunc { return refuse3(nfsErrROFS, empty) }

// notsupp3 refuses a procedure the server never implements, as rofs3
// does.
func notsupp3(empty int) handlerFunc { return refuse3(nfs3ErrNotSupp, empty) }

func refuse3(status uint32, empty int) handlerFunc {
	return func(net.Addr, *xdrReader) []byte {
		w := &xdrWriter{}
		w.uint32(status)
		for range empty {
			w.bool(false)
		}
		return w.b
	}
}

// readFH3 reads an nfs_fh3, a variable-length opaque of at most 64
// bytes.
func readFH3(r *xdrReader) []byte { return r.opaque(64) }

// node3 resolves a v3 handle. A handle this server could never have
// issued is BADHANDLE; one it no longer knows is STALE.
func (s *Server) node3(fh []byte) (Node, uint32) {
	n, err := s.resolve(fh)
	if err != nil {
		return nil, statusFor(err)
	}
	return n, nfsOK
}

// writeFattr3 writes an NFSv3 fattr3 (84 bytes). The guest sees every
// file owned by root.
func writeFattr3(w *xdrWriter, n Node) {
	a := attrOf(n)
	w.uint32(ftype(a.Mode))
	w.uint32(a.Mode & 0o7777)
	w.uint32(a.Nlink)
	w.uint32(0) // uid
	w.uint32(0) // gid
	w.uint64(a.Size)
	w.uint64(a.Used)
	w.uint32(0) // rdev specdata1
	w.uint32(0) // rdev specdata2
	w.uint64(1) // fsid
	w.uint64(a.Fileid)
	writeTime3(w, a.Atime)
	writeTime3(w, a.Mtime)
	writeTime3(w, a.Ctime)
}

func writeTime3(w *xdrWriter, t time.Time) {
	w.uint32(uint32(t.Unix()))
	w.uint32(uint32(t.Nanosecond()))
}

// writePostOp writes a post_op_attr: the attributes when n is known.
func writePostOp(w *xdrWriter, n Node) {
	if n == nil {
		w.bool(false)
		return
	}
	w.bool(true)
	writeFattr3(w, n)
}

func (s *Server) nfs3Getattr(_ net.Addr, r *xdrReader) []byte {
	fh := readFH3(r)
	w := &xdrWriter{}
	n, st := s.node3(fh)
	w.uint32(st)
	if st == nfsOK {
		writeFattr3(w, n)
	}
	return w.b
}

func (s *Server) nfs3Lookup(_ net.Addr, r *xdrReader) []byte {
	fh := readFH3(r)
	name := r.str()
	w := &xdrWriter{}
	dir, st := s.node3(fh)
	if st != nfsOK {
		w.uint32(st)
		writePostOp(w, nil)
		return w.b
	}
	if ftype(dir.Mode()) != nfDIR {
		w.uint32(nfsErrNotdir)
		writePostOp(w, dir)
		return w.b
	}
	child, err := s.FS.Lookup(dir, name)
	if err != nil {
		w.uint32(statusFor(err))
		writePostOp(w, dir)
		return w.b
	}
	s.logOpen(child)
	w.uint32(nfsOK)
	w.opaque(s.handle(child))
	writePostOp(w, child)
	writePostOp(w, dir)
	return w.b
}

// nfs3Access grants what root may do: read anything, search any
// directory, execute a file with an execute bit set, and on a writable
// export change any directory or file.
func (s *Server) nfs3Access(_ net.Addr, r *xdrReader) []byte {
	fh := readFH3(r)
	want := r.uint32()
	w := &xdrWriter{}
	n, st := s.node3(fh)
	if st != nfsOK {
		w.uint32(st)
		writePostOp(w, nil)
		return w.b
	}
	allow := uint32(access3Read)
	_, writable := s.FS.(WriteFS)
	switch ftype(n.Mode()) {
	case nfDIR:
		allow |= access3Lookup
		if writable {
			allow |= access3Modify | access3Extend | access3Delete
		}
	case nfREG:
		if n.Mode()&0o111 != 0 {
			allow |= access3Execute
		}
		if writable {
			allow |= access3Modify | access3Extend
		}
	}
	w.uint32(nfsOK)
	writePostOp(w, n)
	w.uint32(want & allow)
	return w.b
}

func (s *Server) nfs3Readlink(_ net.Addr, r *xdrReader) []byte {
	fh := readFH3(r)
	w := &xdrWriter{}
	n, st := s.node3(fh)
	if st != nfsOK {
		w.uint32(st)
		writePostOp(w, nil)
		return w.b
	}
	if ftype(n.Mode()) != nfLNK {
		w.uint32(nfs3ErrInval)
		writePostOp(w, n)
		return w.b
	}
	target, err := s.FS.Readlink(n)
	if err != nil {
		w.uint32(statusFor(err))
		writePostOp(w, n)
		return w.b
	}
	w.uint32(nfsOK)
	writePostOp(w, n)
	w.opaque([]byte(target))
	return w.b
}

func (s *Server) nfs3Read(_ net.Addr, r *xdrReader) []byte {
	fh := readFH3(r)
	offset := r.uint64()
	count := r.uint32()
	w := &xdrWriter{}
	n, st := s.node3(fh)
	if st != nfsOK {
		w.uint32(st)
		writePostOp(w, nil)
		return w.b
	}
	switch ftype(n.Mode()) {
	case nfDIR:
		st = nfs3ErrIsdir
	case nfREG:
	default:
		st = nfs3ErrInval
	}
	if st != nfsOK {
		w.uint32(st)
		writePostOp(w, n)
		return w.b
	}
	size := max(n.Size(), 0)
	var data []byte
	if offset < uint64(size) {
		data = make([]byte, min(count, maxRead3))
		got, err := s.FS.ReadAt(n, data, int64(offset))
		if err != nil && got == 0 {
			w.uint32(statusFor(err))
			writePostOp(w, n)
			return w.b
		}
		data = data[:got]
	}
	w.uint32(nfsOK)
	writePostOp(w, n)
	w.uint32(uint32(len(data)))
	w.bool(offset+uint64(len(data)) >= uint64(size))
	w.opaque(data)
	return w.b
}

// xdrStrLen is the encoded size of a string: length word plus padded
// bytes.
func xdrStrLen(s string) int { return 4 + (len(s)+3)&^3 }

// readdirFixed is the size of a READDIR or READDIRPLUS reply around its
// entries: status, dir_attributes, cookieverf, the list terminator and
// eof.
const readdirFixed = 4 + 4 + 84 + 8 + 4 + 4

// readdirStart resolves a READDIR or READDIRPLUS directory and its
// entries. On failure it writes the failure reply and returns false.
func (s *Server) readdirStart(w *xdrWriter, fh []byte) (Node, []DirEntry, bool) {
	dir, st := s.node3(fh)
	if st != nfsOK {
		w.uint32(st)
		writePostOp(w, nil)
		return nil, nil, false
	}
	if ftype(dir.Mode()) != nfDIR {
		w.uint32(nfsErrNotdir)
		writePostOp(w, dir)
		return nil, nil, false
	}
	ents, err := s.FS.ReadDir(dir)
	if err != nil {
		w.uint32(statusFor(err))
		writePostOp(w, dir)
		return nil, nil, false
	}
	return dir, ents, true
}

// readdirEntries writes entries from cookie on while they fit, then the
// terminator and eof. size gives an entry's cost against the two
// budgets the caller tracks; emit writes it. It reports false, having
// written nothing, when not even the first remaining entry fits.
func readdirEntries(w *xdrWriter, ents []DirEntry, cookie uint64, fits func(DirEntry) bool, emit func(DirEntry, uint64)) bool {
	start := readdirStart(ents, cookie)
	i := start
	for ; i < len(ents); i++ {
		if !fits(ents[i]) {
			if i == start {
				return false
			}
			break
		}
		w.bool(true)
		emit(ents[i], entryCookie(ents, i))
	}
	w.bool(false)
	w.bool(i >= len(ents))
	return true
}

// The cookie verifier is always zero and an incoming one is not checked
// (RFC 1813 allows either): a cookie is an entry's position or its own
// stable value, and neither is invalidated by the directory changing.
var zeroVerf [8]byte

func (s *Server) nfs3Readdir(_ net.Addr, r *xdrReader) []byte {
	fh := readFH3(r)
	cookie := r.uint64()
	r.bytes(8) // cookieverf
	count := int(r.uint32())
	w := &xdrWriter{}
	dir, ents, ok := s.readdirStart(w, fh)
	if !ok {
		return w.b
	}
	body := &xdrWriter{}
	budget := count - readdirFixed
	fits := func(e DirEntry) bool {
		n := 4 + 8 + xdrStrLen(e.Name) + 8
		if n > budget {
			return false
		}
		budget -= n
		return true
	}
	emit := func(e DirEntry, c uint64) {
		body.uint64(attrOf(e.Node).Fileid)
		body.opaque([]byte(e.Name))
		body.uint64(c)
	}
	if !readdirEntries(body, ents, cookie, fits, emit) {
		w.uint32(nfs3ErrTooSmall)
		writePostOp(w, dir)
		return w.b
	}
	w.uint32(nfsOK)
	writePostOp(w, dir)
	w.fixed(zeroVerf[:])
	w.fixed(body.b)
	return w.b
}

func (s *Server) nfs3Readdirplus(_ net.Addr, r *xdrReader) []byte {
	fh := readFH3(r)
	cookie := r.uint64()
	r.bytes(8) // cookieverf
	dircount := int(r.uint32())
	maxcount := int(r.uint32())
	w := &xdrWriter{}
	dir, ents, ok := s.readdirStart(w, fh)
	if !ok {
		return w.b
	}
	body := &xdrWriter{}
	// dircount bounds the entries' fileid, name and cookie alone;
	// maxcount bounds the whole reply.
	dirBudget, maxBudget := dircount, maxcount-readdirFixed
	fits := func(e DirEntry) bool {
		d := 8 + xdrStrLen(e.Name) + 8
		m := 4 + d + 4 + 84 + 4 + 4 + fhSize
		if d > dirBudget || m > maxBudget {
			return false
		}
		dirBudget -= d
		maxBudget -= m
		return true
	}
	emit := func(e DirEntry, c uint64) {
		body.uint64(attrOf(e.Node).Fileid)
		body.opaque([]byte(e.Name))
		body.uint64(c)
		writePostOp(body, e.Node)
		body.bool(true) // post_op_fh3
		body.opaque(s.handle(e.Node))
	}
	if !readdirEntries(body, ents, cookie, fits, emit) {
		w.uint32(nfs3ErrTooSmall)
		writePostOp(w, dir)
		return w.b
	}
	w.uint32(nfsOK)
	writePostOp(w, dir)
	w.fixed(zeroVerf[:])
	w.fixed(body.b)
	return w.b
}

// nfs3Fsstat reports the FS's capacity, or none for read-only media.
func (s *Server) nfs3Fsstat(_ net.Addr, r *xdrReader) []byte {
	fh := readFH3(r)
	w := &xdrWriter{}
	n, st := s.node3(fh)
	var sp Space
	if st == nfsOK {
		var err error
		sp, err = s.space()
		st = statusFor(err)
	}
	w.uint32(st)
	writePostOp(w, n)
	if st != nfsOK {
		return w.b
	}
	for _, v := range []uint64{sp.Total, sp.Free, sp.Avail, sp.Files, sp.FreeFiles, sp.AvailFiles} {
		w.uint64(v)
	}
	w.uint32(0) // invarsec
	return w.b
}

// FSINFO properties (RFC 1813 section 3.3.19).
const (
	fsf3Symlink     = 0x02
	fsf3Homogeneous = 0x08
	fsf3CanSetTime  = 0x10
)

func (s *Server) nfs3Fsinfo(_ net.Addr, r *xdrReader) []byte {
	fh := readFH3(r)
	w := &xdrWriter{}
	n, st := s.node3(fh)
	w.uint32(st)
	writePostOp(w, n)
	if st != nfsOK {
		return w.b
	}
	w.uint32(maxRead3) // rtmax
	w.uint32(maxRead3) // rtpref
	w.uint32(4096)     // rtmult
	w.uint32(maxRead3) // wtmax
	w.uint32(maxRead3) // wtpref
	w.uint32(4096)     // wtmult
	w.uint32(8192)     // dtpref
	w.uint64(math.MaxInt64)
	// A writable export is a host directory, which keeps times to the
	// nanosecond; read-only media keep whole seconds.
	if _, ok := s.FS.(WriteFS); ok {
		w.uint32(0) // time_delta
		w.uint32(1)
		w.uint32(fsf3Symlink | fsf3Homogeneous | fsf3CanSetTime)
		return w.b
	}
	w.uint32(1) // time_delta
	w.uint32(0)
	w.uint32(fsf3Symlink | fsf3Homogeneous)
	return w.b
}

func (s *Server) nfs3Pathconf(_ net.Addr, r *xdrReader) []byte {
	fh := readFH3(r)
	w := &xdrWriter{}
	n, st := s.node3(fh)
	w.uint32(st)
	writePostOp(w, n)
	if st != nfsOK {
		return w.b
	}
	w.uint32(1)   // linkmax: no hard links
	w.uint32(255) // name_max
	w.bool(true)  // no_trunc
	w.bool(true)  // chown_restricted
	w.bool(false) // case_insensitive
	w.bool(true)  // case_preserving
	return w.b
}

// writePreOp writes a pre_op_attr from attributes taken before a change.
func writePreOp(w *xdrWriter, a *Attr) {
	if a == nil {
		w.bool(false)
		return
	}
	w.bool(true)
	w.uint64(a.Size)
	writeTime3(w, a.Mtime)
	writeTime3(w, a.Ctime)
}

// wcc is weak cache consistency data for one object (RFC 1813 section
// 2.6): its attributes before a change, and afterwards those of
// whatever its handle names then.
type wcc struct {
	fh  []byte
	pre *Attr
}

func (s *Server) wccOf(fh []byte, n Node) wcc {
	if n == nil {
		return wcc{}
	}
	a := attrOf(n)
	return wcc{fh: fh, pre: &a}
}

func (s *Server) writeWcc(w *xdrWriter, c wcc) {
	writePreOp(w, c.pre)
	if c.fh == nil {
		writePostOp(w, nil)
		return
	}
	n, _ := s.resolve(c.fh)
	writePostOp(w, n)
}

// readSetTime3 reads a set_atime or set_mtime.
func readSetTime3(r *xdrReader) *time.Time {
	switch r.uint32() {
	case 0: // DONT_CHANGE
		return nil
	case 1: // SET_TO_SERVER_TIME
		t := time.Now()
		return &t
	case 2: // SET_TO_CLIENT_TIME
		sec := r.uint32()
		t := time.Unix(int64(sec), int64(r.uint32()))
		return &t
	}
	r.err = true
	return nil
}

func readSattr3(r *xdrReader) SetAttr {
	var a SetAttr
	if r.bool() {
		m := r.uint32() & 0o7777
		a.Mode = &m
	}
	if r.bool() {
		r.uint32() // uid: every file is root's
	}
	if r.bool() {
		r.uint32() // gid
	}
	if r.bool() {
		sz := r.uint64()
		a.Size = &sz
	}
	a.Atime = readSetTime3(r)
	a.Mtime = readSetTime3(r)
	return a
}

// dirop3 reads diropargs3 and resolves the directory, refusing a
// non-directory. It returns the directory's wcc for the reply.
func (s *Server) dirop3(r *xdrReader) (Node, string, wcc, error) {
	fh := readFH3(r)
	name := r.str()
	if r.err {
		return nil, "", wcc{}, nil
	}
	dir, err := s.resolve(fh)
	c := s.wccOf(fh, dir)
	if err == nil && ftype(dir.Mode()) != nfDIR {
		err = ErrNotDir
	}
	return dir, name, c, err
}

func (s *Server) nfs3Setattr(wfs WriteFS, r *xdrReader) []byte {
	fh := readFH3(r)
	a := readSattr3(r)
	var guard *time.Time
	if r.bool() {
		sec := r.uint32()
		t := time.Unix(int64(sec), int64(r.uint32()))
		guard = &t
	}
	if r.err {
		return nil
	}
	n, err := s.resolve(fh)
	c := s.wccOf(fh, n)
	st := statusFor(err)
	if err == nil {
		if guard != nil && !guard.Equal(c.pre.Ctime) {
			st = nfs3ErrNotSync
		} else {
			st = statusFor(wfs.SetAttr(n, a))
		}
	}
	w := &xdrWriter{}
	w.uint32(st)
	s.writeWcc(w, c)
	return w.b
}

const fileSync = 2 // stable_how FILE_SYNC

func (s *Server) nfs3Write(wfs WriteFS, r *xdrReader) []byte {
	fh := readFH3(r)
	offset := r.uint64()
	count := r.uint32()
	r.uint32() // stable: every write is FILE_SYNC
	data := r.opaque(maxRecord)
	if r.err {
		return nil
	}
	data = data[:min(int(count), len(data))]
	n, err := s.resolve(fh)
	c := s.wccOf(fh, n)
	if err == nil {
		err = writeTo(wfs, n, data, offset)
	}
	w := &xdrWriter{}
	w.uint32(statusFor(err))
	s.writeWcc(w, c)
	if err != nil {
		return w.b
	}
	w.uint32(uint32(len(data)))
	w.uint32(fileSync)
	w.fixed(s.Verifier[:])
	return w.b
}

// nfs3Commit has nothing to flush, since every WRITE was FILE_SYNC; it
// returns the verifier so a client can tell the server hasn't
// restarted.
func (s *Server) nfs3Commit(_ net.Addr, r *xdrReader) []byte {
	fh := readFH3(r)
	r.uint64() // offset
	r.uint32() // count
	if r.err {
		return nil
	}
	n, err := s.resolve(fh)
	w := &xdrWriter{}
	w.uint32(statusFor(err))
	writePreOp(w, nil)
	writePostOp(w, n)
	if err == nil {
		w.fixed(s.Verifier[:])
	}
	return w.b
}

// newNode writes the success reply of CREATE, MKDIR and SYMLINK: the
// node's handle and attributes, then the directory's wcc. A node made
// but then not found again still succeeds, without the optional parts.
func (s *Server) newNode(w *xdrWriter, n Node, dir wcc) {
	w.uint32(nfsOK)
	fh := s.handle(n)
	n, err := s.resolve(fh)
	if err != nil {
		w.bool(false) // post_op_fh3
		writePostOp(w, nil)
	} else {
		w.bool(true)
		w.opaque(fh)
		writePostOp(w, n)
	}
	s.writeWcc(w, dir)
}

// made finishes CREATE, MKDIR or SYMLINK.
func (s *Server) made(n Node, err error, dir wcc) []byte {
	w := &xdrWriter{}
	if err != nil {
		w.uint32(statusFor(err))
		s.writeWcc(w, dir)
		return w.b
	}
	s.newNode(w, n, dir)
	return w.b
}

func (s *Server) nfs3Create(wfs WriteFS, r *xdrReader) []byte {
	dir, name, c, err := s.dirop3(r)
	how := CreateHow(r.uint32())
	var a SetAttr
	var verf [8]byte
	switch how {
	case Unchecked, Guarded:
		a = readSattr3(r)
	case Exclusive:
		copy(verf[:], r.bytes(8))
	default:
		r.err = true
	}
	if r.err {
		return nil
	}
	var n Node
	if err == nil {
		n, err = wfs.Create(dir, name, how, a, verf)
	}
	return s.made(n, err, c)
}

func (s *Server) nfs3Mkdir(wfs WriteFS, r *xdrReader) []byte {
	dir, name, c, err := s.dirop3(r)
	a := readSattr3(r)
	if r.err {
		return nil
	}
	var n Node
	if err == nil {
		n, err = wfs.Mkdir(dir, name, a)
	}
	return s.made(n, err, c)
}

func (s *Server) nfs3Symlink(wfs WriteFS, r *xdrReader) []byte {
	dir, name, c, err := s.dirop3(r)
	a := readSattr3(r)
	target := r.str()
	if r.err {
		return nil
	}
	var n Node
	if err == nil {
		n, err = wfs.Symlink(dir, name, target, a)
	}
	return s.made(n, err, c)
}

func (s *Server) nfs3Remove(wfs WriteFS, r *xdrReader, rmdir bool) []byte {
	dir, name, c, err := s.dirop3(r)
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
	w.uint32(statusFor(err))
	s.writeWcc(w, c)
	return w.b
}

func (s *Server) nfs3Rename(wfs WriteFS, r *xdrReader) []byte {
	from, fromName, fc, err1 := s.dirop3(r)
	to, toName, tc, err2 := s.dirop3(r)
	if r.err {
		return nil
	}
	err := cmp.Or(err1, err2)
	if err == nil {
		err = wfs.Rename(from, fromName, to, toName)
	}
	w := &xdrWriter{}
	w.uint32(statusFor(err))
	s.writeWcc(w, fc)
	s.writeWcc(w, tc)
	return w.b
}
