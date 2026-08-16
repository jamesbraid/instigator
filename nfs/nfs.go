// Package nfs is a read-only NFSv2 server over UDP with the portmap and
// mount protocols beside it, aimed at clients no modern server still
// speaks to: an SGI PROM miniroot mounts its install distribution over
// NFSv2/UDP. No memory-safe userspace NFS-over-UDP server exists, so
// this is written from the RPCs up. Read only - every write path is
// refused - which makes it stateless and small.
package nfs

import (
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
)

// Errors an FS returns; each maps to an NFS status code.
var (
	ErrNotFound = errors.New("no such file")
	ErrNotDir   = errors.New("not a directory")
	ErrStale    = errors.New("stale handle")
	ErrIO       = errors.New("io error")
)

// Node is one filesystem object. ID must be stable for the lifetime of
// the server: it is the filehandle. Mode is a Unix mode with type bits.
type Node interface {
	ID() uint64
	Mode() uint32
	Size() int64
	Mtime() uint32
}

// DirEntry is one directory entry.
type DirEntry struct {
	Name string
	Node Node
}

// FS is the tree the server exports. All methods are read-only. Node
// IDs must be unique across everything the FS can mount, since the ID is
// the filehandle the client keeps.
type FS interface {
	// MountRoot resolves a mount path to its root node. A client mounts
	// one export; the path chooses which.
	MountRoot(path string) (Node, error)
	NodeByID(id uint64) (Node, error)
	Lookup(dir Node, name string) (Node, error)
	ReadDir(dir Node) ([]DirEntry, error)
	ReadAt(n Node, p []byte, off int64) (int, error)
	Readlink(n Node) (string, error)
}

// Server serves the portmap, mount, and NFS protocols over one FS.
type Server struct {
	FS FS

	// AllowIP filters clients; nil allows everyone.
	AllowIP func(netip.Addr) bool

	// Logf, when set, receives one line per call.
	Logf func(format string, args ...any)

	// Ports portmap hands out. Atomic because SetPorts can race the
	// serve goroutines when tests bind ephemeral ports after Serve.
	mountPort, nfsPort atomic.Uint32
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// SetPorts records the mount and nfs UDP ports portmap hands out. In
// production these are the well-known 635/2049; tests use ephemeral
// ports and set them after binding.
func (s *Server) SetPorts(mount, nfs uint32) {
	s.mountPort.Store(mount)
	s.nfsPort.Store(nfs)
}

// ServePortmap answers portmap GETPORT so a client can find mountd and
// nfsd. Conventionally bound to UDP 111.
func (s *Server) ServePortmap(pc net.PacketConn) error {
	return s.serveRPC(pc, progPortmap, 2, map[uint32]handlerFunc{
		3: s.pmapGetport, // PMAPPROC_GETPORT
	})
}

// ServeMount answers the mount protocol. Conventionally bound to UDP 635.
func (s *Server) ServeMount(pc net.PacketConn) error {
	return s.serveRPC(pc, progMount, 1, map[uint32]handlerFunc{
		1: s.mountMnt, // MOUNTPROC_MNT
		3: s.mountUmnt,
		0: func(net.Addr, *xdrReader) []byte { return nil }, // NULL
	})
}

// ServeNFS answers NFSv2. Conventionally bound to UDP 2049.
func (s *Server) ServeNFS(pc net.PacketConn) error {
	return s.serveRPC(pc, progNFS, 2, map[uint32]handlerFunc{
		0:  func(net.Addr, *xdrReader) []byte { return nil }, // NULL
		1:  s.nfsGetattr,
		4:  s.nfsLookup,
		5:  s.nfsReadlink,
		6:  s.nfsRead,
		8:  s.nfsWrite, // refused
		16: s.nfsReaddir,
		17: s.nfsStatfs,
	})
}

func (s *Server) pmapGetport(_ net.Addr, r *xdrReader) []byte {
	prog := r.uint32()
	r.uint32() // vers
	r.uint32() // proto
	r.uint32() // port (0)
	w := &xdrWriter{}
	switch prog {
	case progMount:
		w.uint32(s.mountPort.Load())
	case progNFS:
		w.uint32(s.nfsPort.Load())
	default:
		w.uint32(0)
	}
	return w.b
}
