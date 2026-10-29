// Package nfs is a read-only NFS server, versions 2 and 3 over UDP and
// TCP, with the portmap and MOUNT protocols beside it. It is aimed at
// clients no modern server still speaks to: an SGI PROM miniroot mounts
// its install distribution over NFSv2/UDP, and IRIX 6.5 mounts NFSv3
// over TCP. No memory-safe userspace NFS-over-UDP server exists, so
// this is written from the RPCs up. Read only - every write path is
// refused - which makes it stateless and small.
package nfs

import (
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"

	"github.com/jamesbraid/instigator/internal/logging"
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

// Describer is optionally implemented by a Node that can render its
// own path and backing image, for the per-open log line nfsLookup
// emits when the resolved node is a regular file. An FS whose Node
// doesn't back onto named media (a test double, say) need not
// implement it; that log line is simply skipped.
type Describer interface {
	Describe() string
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

// Exporter is optionally implemented by an FS that can list the paths a
// client may mount, for MOUNT's EXPORT procedure. Without it the server
// lists "/".
type Exporter interface {
	Exports() []string
}

// Server serves the portmap, MOUNT and NFS protocols over one FS. Every
// listener answers all three, so a client that knows the port can skip
// portmap entirely.
type Server struct {
	FS FS

	// AllowIP filters clients; nil allows everyone.
	AllowIP func(netip.Addr) bool

	// Logger, when set, receives leveled log output: DEBUG for every
	// RPC call (program, procedure, caller), INFO for a mount and each
	// regular file opened (once per LOOKUP that resolves to one, not
	// per read/getattr against an already-open handle), WARN for a
	// call refused by the client filter. A nil Logger is silent.
	Logger *logging.Logger

	// ports is what portmap advertises. Atomic because tests bind
	// ephemeral ports and set them after Serve has started.
	ports atomic.Pointer[[2]Ports]

	tableOnce sync.Once
	rpc       *rpcTable
}

// Ports are the ports portmap advertises on one transport. A zero port
// leaves that program unregistered there.
type Ports struct {
	Portmap, Mount, NFS uint32
}

// SetPorts records what portmap GETPORT and DUMP report for UDP and TCP.
// In production these are the ports the listeners are bound to.
func (s *Server) SetPorts(udp, tcp Ports) {
	s.ports.Store(&[2]Ports{udp, tcp})
}
