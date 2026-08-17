package nfs

import (
	"encoding/binary"
	"net"
)

// NFSv2 status codes (RFC 1094).
const (
	nfsOK        = 0
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
	nfBLK = 3
	nfCHR = 4
	nfLNK = 5
)

const fhSize = 32

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

func statusFor(err error) uint32 {
	switch err {
	case nil:
		return nfsOK
	case ErrNotFound:
		return nfsErrNoent
	case ErrNotDir:
		return nfsErrNotdir
	case ErrStale:
		return nfsErrStale
	default:
		return nfsErrIO
	}
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

// writeFattr writes an NFSv2 fattr (68 bytes). Blocks are reported in
// 512-byte units; blocksize is fixed at 512, matching EFS.
func writeFattr(w *xdrWriter, n Node) {
	mode := n.Mode()
	size := uint32(n.Size())
	w.uint32(ftype(mode))
	w.uint32(mode)
	w.uint32(1) // nlink
	w.uint32(0) // uid (served as root; media ownership is irrelevant to install)
	w.uint32(0) // gid
	w.uint32(size)
	w.uint32(512)                // blocksize
	w.uint32(0)                  // rdev
	w.uint32((size + 511) / 512) // blocks
	w.uint32(1)                  // fsid
	w.uint32(uint32(n.ID()))     // fileid
	mt := n.Mtime()
	w.uint32(mt)
	w.uint32(0) // atime usec
	w.uint32(mt)
	w.uint32(0)
	w.uint32(mt)
	w.uint32(0)
}

// node resolves a filehandle to a node, or writes nothing and returns
// nil with a status the caller emits.
func (s *Server) node(fh []byte) (Node, uint32) {
	id, ok := decodeFH(fh)
	if !ok {
		return nil, nfsErrStale
	}
	n, err := s.FS.NodeByID(id)
	if err != nil {
		return nil, statusFor(err)
	}
	return n, nfsOK
}

func (s *Server) mountMnt(caller net.Addr, r *xdrReader) []byte {
	path := r.str()
	w := &xdrWriter{}
	root, err := s.FS.MountRoot(path)
	if err != nil {
		s.Logger.Warnf("nfs: mount %q from %s: %v", path, caller, err)
		w.uint32(statusFor(err))
		return w.b
	}
	s.Logger.Infof("nfs: mount %q from %s", path, caller)
	w.uint32(nfsOK)
	w.fixed(encodeFH(root.ID()))
	return w.b
}

func (s *Server) mountUmnt(_ net.Addr, r *xdrReader) []byte {
	r.str()
	return nil
}

func (s *Server) nfsGetattr(_ net.Addr, r *xdrReader) []byte {
	fh := r.bytes(fhSize)
	w := &xdrWriter{}
	n, st := s.node(fh)
	if st != nfsOK {
		w.uint32(st)
		return w.b
	}
	w.uint32(nfsOK)
	writeFattr(w, n)
	return w.b
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
		w.uint32(statusFor(err))
		return w.b
	}
	// Log once, here, for a regular file - the NFSv2 equivalent of an
	// open, since a client LOOKUPs a file once and READs the handle it
	// gets back rather than reopening it. A directory component on the
	// way there doesn't log: this is a manifest of files served, not a
	// trace of every path element walked.
	if ftype(child.Mode()) == nfREG {
		if d, ok := child.(Describer); ok {
			s.Logger.Infof("nfs: opened %s", d.Describe())
		}
	}
	w.uint32(nfsOK)
	w.fixed(encodeFH(child.ID()))
	writeFattr(w, child)
	return w.b
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
		w.uint32(statusFor(err))
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
		w.uint32(statusFor(err))
		return w.b
	}
	w.uint32(nfsOK)
	writeFattr(w, n)
	w.opaque(buf[:got])
	return w.b
}

func (s *Server) nfsWrite(_ net.Addr, r *xdrReader) []byte {
	// read-only export: refuse without decoding the payload
	w := &xdrWriter{}
	w.uint32(nfsErrROFS)
	return w.b
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
		w.uint32(statusFor(err))
		return w.b
	}
	w.uint32(nfsOK)
	// cookie is the index of the next entry to emit; entries are stable
	// in FS order within one server lifetime
	budget := int(count)
	i := int(cookie)
	for ; i < len(ents); i++ {
		e := ents[i]
		entrySize := 4 + 4 + 4 + ((len(e.Name) + 3) &^ 3) + 4
		if budget-entrySize < 0 && i > int(cookie) {
			break
		}
		budget -= entrySize
		w.uint32(1) // value follows
		w.uint32(uint32(e.Node.ID()))
		w.opaque([]byte(e.Name))
		w.uint32(uint32(i + 1)) // cookie
	}
	w.uint32(0) // no more entries in this reply
	if i >= len(ents) {
		w.uint32(1) // eof
	} else {
		w.uint32(0)
	}
	return w.b
}

func (s *Server) nfsStatfs(_ net.Addr, r *xdrReader) []byte {
	fh := r.bytes(fhSize)
	w := &xdrWriter{}
	if _, st := s.node(fh); st != nfsOK {
		w.uint32(st)
		return w.b
	}
	w.uint32(nfsOK)
	w.uint32(maxRead) // tsize
	w.uint32(512)     // bsize
	w.uint32(0)       // blocks (read-only media; report 0 free)
	w.uint32(0)       // bfree
	w.uint32(0)       // bavail
	return w.b
}
