// Package nfs is an NFS server, versions 2 and 3 over UDP and TCP, with
// the portmap and MOUNT protocols beside it. It is aimed at clients no
// modern server still speaks to: an SGI PROM miniroot mounts its install
// distribution over NFSv2/UDP, and IRIX 6.5 mounts NFSv3 over TCP. No
// memory-safe userspace NFS-over-UDP server exists, so this is written
// from the RPCs up.
//
// The server keeps no state of its own beyond a reply cache for
// retransmitted UDP calls. An FS is served read-only unless it
// implements WriteFS, and every write reaches the FS before the reply.
package nfs

import (
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jamesbraid/instigator/internal/logging"
)

// Errors an FS returns; each maps to an NFS status code.
var (
	ErrNotFound    = errors.New("no such file")
	ErrNotDir      = errors.New("not a directory")
	ErrStale       = errors.New("stale handle")
	ErrIO          = errors.New("io error")
	ErrBadHandle   = errors.New("not a handle this server issues")
	ErrExist       = errors.New("file exists")
	ErrIsDir       = errors.New("is a directory")
	ErrNotEmpty    = errors.New("directory not empty")
	ErrInval       = errors.New("invalid argument")
	ErrAccess      = errors.New("permission denied")
	ErrPerm        = errors.New("operation not permitted")
	ErrNoSpace     = errors.New("no space left")
	ErrQuota       = errors.New("quota exceeded")
	ErrNameTooLong = errors.New("name too long")
	ErrFBig        = errors.New("file too large")
	ErrXDev        = errors.New("cross-device rename")
	ErrROFS        = errors.New("read-only filesystem")
	ErrNotSupp     = errors.New("operation not supported")
)

// Node is one filesystem object. ID is the fileid clients see, unique
// within the export; for an FS that doesn't implement HandleFS it is
// also the filehandle, so it must be stable for the lifetime of the
// server. Mode is a Unix mode with type bits.
type Node interface {
	ID() uint64
	Mode() uint32
	Size() int64
	Mtime() uint32
}

// Attr is a node's full attributes. A Node that implements AttrNode
// reports these; any other Node is described by its four methods, with
// one link, 512-byte blocks and its mtime for every time.
type Attr struct {
	Mode                uint32 // Unix mode with type bits
	Nlink               uint32
	Size, Used          uint64 // Used is bytes of storage
	Fileid              uint64
	Atime, Mtime, Ctime time.Time
}

// AttrNode is a Node that reports full attributes.
type AttrNode interface {
	Node
	Attr() Attr
}

func attrOf(n Node) Attr {
	if a, ok := n.(AttrNode); ok {
		return a.Attr()
	}
	size := uint64(max(n.Size(), 0))
	mt := time.Unix(int64(n.Mtime()), 0)
	return Attr{
		Mode: n.Mode(), Nlink: 1, Size: size, Used: (size + 511) &^ 511, Fileid: n.ID(),
		Atime: mt, Mtime: mt, Ctime: mt,
	}
}

// DirEntry is one directory entry. Cookie, when nonzero, is the entry's
// READDIR cookie: a value that stays put while the directory changes
// around it, and that fits in 32 bits for NFSv2. Entries with cookies
// come sorted by them, and a listing resumes after the last cookie the
// client saw. Entries without one are numbered by position.
type DirEntry struct {
	Name   string
	Node   Node
	Cookie uint32
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
// IDs must be unique across everything the FS can mount, since unless
// the FS implements HandleFS the ID is the filehandle the client keeps.
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

// HandleFS is an FS that makes its own filehandles instead of carrying
// Node.ID. Handle returns exactly 32 bytes. NodeByHandle returns
// ErrBadHandle for bytes it could never have issued and ErrStale for a
// handle whose file is gone.
type HandleFS interface {
	FS
	Handle(n Node) []byte
	NodeByHandle(fh []byte) (Node, error)
}

// SetAttr is the attributes a client asks to change. A nil field is
// left alone. Owners are not here: the export shows every file owned by
// root, and a chown succeeds without changing anything.
type SetAttr struct {
	Mode         *uint32 // permission bits
	Size         *uint64
	Atime, Mtime *time.Time
}

// CreateHow is how CREATE treats an existing name (RFC 1813 section
// 3.3.8).
type CreateHow int

const (
	// Unchecked opens an existing file and applies the attributes.
	Unchecked CreateHow = iota
	// Guarded fails with ErrExist if the name exists.
	Guarded
	// Exclusive creates the file with a verifier recorded in it, and
	// succeeds again for a retransmission carrying the same verifier.
	Exclusive
)

// WriteFS is an FS the server may change. An FS without it is served
// read-only, every change refused with ROFS. Methods return the node
// they made; the server rereads attributes it reports afterwards.
type WriteFS interface {
	FS
	SetAttr(n Node, a SetAttr) error
	WriteAt(n Node, p []byte, off int64) error
	Create(dir Node, name string, how CreateHow, a SetAttr, verf [8]byte) (Node, error)
	Mkdir(dir Node, name string, a SetAttr) (Node, error)
	Symlink(dir Node, name, target string, a SetAttr) (Node, error)
	Remove(dir Node, name string) error
	Rmdir(dir Node, name string) error
	Rename(fromDir Node, fromName string, toDir Node, toName string) error
}

// Space is filesystem capacity, in bytes and in files.
type Space struct {
	Total, Free, Avail           uint64
	Files, FreeFiles, AvailFiles uint64
}

// SpaceFS is an FS that can report its capacity. Without it the server
// reports none, which suits read-only media.
type SpaceFS interface {
	Space() (Space, error)
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

	// Verifier is the write verifier WRITE and COMMIT return (RFC 1813
	// section 3.3.7). It must change when the server restarts, so a
	// client knows to resend unstable writes; every write here is
	// stable, so it only matters to clients that check it. Zero picks
	// one from the start time.
	Verifier [8]byte

	tableOnce sync.Once
	rpc       *rpcTable
	drc       drc
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
