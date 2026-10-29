package nfs

import (
	"math"
	"net"
)

// NFSv3 status codes (RFC 1813 section 2.6) beyond those it shares with
// version 2, which keep their numbers.
const (
	nfs3ErrInval     = 22
	nfs3ErrIsdir     = 21
	nfs3ErrBadHandle = 10001
	nfs3ErrTooSmall  = 10005
)

// maxRead3 is rtmax and wtmax. 32 KiB plus headers still fits one UDP
// datagram.
const maxRead3 = 32768

// ACCESS bits (RFC 1813 section 3.3.4).
const (
	access3Read    = 0x01
	access3Lookup  = 0x02
	access3Execute = 0x20
)

// nfs3Procs is the v3 procedure table (RFC 1813 section 3.3). Every
// procedure that would change the export is refused with ROFS.
func (s *Server) nfs3Procs() map[uint32]handlerFunc {
	return map[uint32]handlerFunc{
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
}

// rofs3 refuses a v3 procedure that would change the export, without
// decoding its arguments. A failure body is pre_op_attr and post_op_attr
// values with nothing to report, and empty counts them: two for each
// wcc_data, one for a lone post_op_attr.
func rofs3(empty int) handlerFunc {
	return func(net.Addr, *xdrReader) []byte {
		w := &xdrWriter{}
		w.uint32(nfsErrROFS)
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
	id, ok := decodeFH(fh)
	if !ok {
		return nil, nfs3ErrBadHandle
	}
	n, err := s.FS.NodeByID(id)
	if err != nil {
		return nil, statusFor(err)
	}
	return n, nfsOK
}

// writeFattr3 writes an NFSv3 fattr3 (84 bytes) on the same terms as
// writeFattr: owner root, one link, 512-byte blocks, the mtime for all
// three times.
func writeFattr3(w *xdrWriter, n Node) {
	mode := n.Mode()
	size := uint64(max(n.Size(), 0))
	w.uint32(ftype(mode))
	w.uint32(mode & 0o7777)
	w.uint32(1) // nlink
	w.uint32(0) // uid
	w.uint32(0) // gid
	w.uint64(size)
	w.uint64((size + 511) &^ 511) // used
	w.uint32(0)                   // rdev specdata1
	w.uint32(0)                   // rdev specdata2
	w.uint64(1)                   // fsid
	w.uint64(n.ID())
	mt := n.Mtime()
	for range 3 {
		w.uint32(mt)
		w.uint32(0)
	}
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
	w.opaque(encodeFH(child.ID()))
	writePostOp(w, child)
	writePostOp(w, dir)
	return w.b
}

// nfs3Access grants what root may do on a read-only export: read
// anything, search any directory, and execute a file with an execute
// bit set.
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
	switch {
	case ftype(n.Mode()) == nfDIR:
		allow |= access3Lookup
	case n.Mode()&0o111 != 0:
		allow |= access3Execute
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
	i := int(min(cookie, uint64(len(ents))))
	start := i
	for ; i < len(ents); i++ {
		if !fits(ents[i]) {
			if i == start {
				return false
			}
			break
		}
		w.bool(true)
		emit(ents[i], uint64(i+1))
	}
	w.bool(false)
	w.bool(i >= len(ents))
	return true
}

// The cookie verifier is always zero: cookies are entry indexes, and an
// incoming verifier is not checked (RFC 1813 allows either).
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
		body.uint64(e.Node.ID())
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
		body.uint64(e.Node.ID())
		body.opaque([]byte(e.Name))
		body.uint64(c)
		writePostOp(body, e.Node)
		body.bool(true) // post_op_fh3
		body.opaque(encodeFH(e.Node.ID()))
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

// nfs3Fsstat reports no space, as STATFS does: the export is read-only
// media.
func (s *Server) nfs3Fsstat(_ net.Addr, r *xdrReader) []byte {
	fh := readFH3(r)
	w := &xdrWriter{}
	n, st := s.node3(fh)
	w.uint32(st)
	writePostOp(w, n)
	if st != nfsOK {
		return w.b
	}
	for range 6 { // tbytes, fbytes, abytes, tfiles, ffiles, afiles
		w.uint64(0)
	}
	w.uint32(0) // invarsec
	return w.b
}

// FSINFO properties (RFC 1813 section 3.3.19).
const (
	fsf3Symlink     = 0x02
	fsf3Homogeneous = 0x08
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
	w.uint32(1) // time_delta: whole seconds
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
