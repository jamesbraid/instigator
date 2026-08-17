package nfs_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/internal/logging"
	"github.com/jamesbraid/instigator/nfs"
)

// memNode / memFS implement nfs.FS in memory.
type memNode struct {
	id       uint64
	mode     uint32 // unix mode incl type bits
	size     int64
	mtime    uint32
	data     []byte
	entries  map[string]*memNode
	target   string
	describe string // non-empty implements nfs.Describer, for the per-open log test
}

func (n *memNode) ID() uint64    { return n.id }
func (n *memNode) Mode() uint32  { return n.mode }
func (n *memNode) Size() int64   { return n.size }
func (n *memNode) Mtime() uint32 { return n.mtime }

// Describe implements nfs.Describer only when the fixture set one:
// TestLookupLogsOpenOncePerRegularFile needs it, everything else
// exercises the ordinary no-Describer path other FS implementations
// take.
func (n *memNode) Describe() string { return n.describe }

type memFS struct {
	root *memNode
	byID map[uint64]*memNode
}

func (f *memFS) MountRoot(path string) (nfs.Node, error) {
	if path != "/" {
		return nil, nfs.ErrNotFound
	}
	return f.root, nil
}

func (f *memFS) NodeByID(id uint64) (nfs.Node, error) {
	n, ok := f.byID[id]
	if !ok {
		return nil, nfs.ErrStale
	}
	return n, nil
}

func (f *memFS) Lookup(dir nfs.Node, name string) (nfs.Node, error) {
	d := dir.(*memNode)
	if d.entries == nil {
		return nil, nfs.ErrNotDir
	}
	c, ok := d.entries[name]
	if !ok {
		return nil, nfs.ErrNotFound
	}
	return c, nil
}

func (f *memFS) ReadDir(dir nfs.Node) ([]nfs.DirEntry, error) {
	d := dir.(*memNode)
	if d.entries == nil {
		return nil, nfs.ErrNotDir
	}
	var out []nfs.DirEntry
	for name, c := range d.entries {
		out = append(out, nfs.DirEntry{Name: name, Node: c})
	}
	return out, nil
}

func (f *memFS) ReadAt(n nfs.Node, p []byte, off int64) (int, error) {
	m := n.(*memNode)
	if off >= int64(len(m.data)) {
		return 0, nil
	}
	return copy(p, m.data[off:]), nil
}

func (f *memFS) Readlink(n nfs.Node) (string, error) {
	return n.(*memNode).target, nil
}

func testFS() *memFS {
	file := &memNode{id: 10, mode: 0o100644, size: 700, mtime: 111,
		data: bytes.Repeat([]byte("F"), 700)}
	link := &memNode{id: 11, mode: 0o120777, size: 9, mtime: 112, target: "hello.txt"}
	root := &memNode{id: 1, mode: 0o040755, mtime: 100,
		entries: map[string]*memNode{"hello.txt": file, "link": link}}
	return &memFS{root: root, byID: map[uint64]*memNode{1: root, 10: file, 11: link}}
}

// startNFS runs portmap, mountd and nfsd on loopback ephemeral ports.
func startNFS(t *testing.T) (*nfs.Server, *net.UDPAddr) {
	return startNFSServer(t, &nfs.Server{FS: testFS()})
}

// startNFSServer is startNFS for a caller that needs its own FS or
// Logger on the Server, e.g. to capture the per-open log line.
func startNFSServer(t *testing.T, s *nfs.Server) (*nfs.Server, *net.UDPAddr) {
	t.Helper()
	pm, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	mnt, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	nf, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pm.Close(); mnt.Close(); nf.Close() })
	go s.ServePortmap(pm)
	go s.ServeMount(mnt)
	go s.ServeNFS(nf)
	s.SetPorts(
		uint32(mnt.LocalAddr().(*net.UDPAddr).Port),
		uint32(nf.LocalAddr().(*net.UDPAddr).Port),
	)
	return s, pm.LocalAddr().(*net.UDPAddr)
}

// rpcCall sends one ONC-RPC call over UDP and returns the reply body
// after the accepted-reply header.
func rpcCall(t *testing.T, addr *net.UDPAddr, prog, vers, proc uint32, args []byte) []byte {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var b bytes.Buffer
	w := func(v uint32) { binary.Write(&b, binary.BigEndian, v) }
	w(0x11223344) // xid
	w(0)          // CALL
	w(2)          // rpc version
	w(prog)
	w(vers)
	w(proc)
	w(0) // AUTH_NULL flavor
	w(0) // length
	w(0) // verf flavor
	w(0) // verf length
	b.Write(args)
	if _, err := c.WriteTo(b.Bytes(), addr); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 65536)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	r := buf[:n]
	if got := binary.BigEndian.Uint32(r[0:4]); got != 0x11223344 {
		t.Fatalf("xid = %#x", got)
	}
	if binary.BigEndian.Uint32(r[4:8]) != 1 {
		t.Fatal("not a reply")
	}
	if binary.BigEndian.Uint32(r[8:12]) != 0 {
		t.Fatal("reply rejected")
	}
	// skip verf flavor+len, accept stat
	if stat := binary.BigEndian.Uint32(r[20:24]); stat != 0 {
		t.Fatalf("accept stat = %d", stat)
	}
	return r[24:]
}

func u32(b []byte, off int) uint32 { return binary.BigEndian.Uint32(b[off:]) }

func TestPortmapGetport(t *testing.T) {
	s, pm := startNFS(t)
	_ = s
	var args bytes.Buffer
	binary.Write(&args, binary.BigEndian, uint32(100003)) // nfs prog
	binary.Write(&args, binary.BigEndian, uint32(2))
	binary.Write(&args, binary.BigEndian, uint32(17)) // udp
	binary.Write(&args, binary.BigEndian, uint32(0))
	rep := rpcCall(t, pm, 100000, 2, 3, args.Bytes())
	if u32(rep, 0) == 0 {
		t.Fatal("GETPORT(nfs) = 0")
	}
	args.Reset()
	binary.Write(&args, binary.BigEndian, uint32(100005)) // mount prog
	binary.Write(&args, binary.BigEndian, uint32(1))
	binary.Write(&args, binary.BigEndian, uint32(17))
	binary.Write(&args, binary.BigEndian, uint32(0))
	rep = rpcCall(t, pm, 100000, 2, 3, args.Bytes())
	if u32(rep, 0) == 0 {
		t.Fatal("GETPORT(mount) = 0")
	}
}

// mnt asks mountd for the root filehandle.
func mnt(t *testing.T, s *nfs.Server, pmAddr *net.UDPAddr, path string) []byte {
	t.Helper()
	var args bytes.Buffer
	binary.Write(&args, binary.BigEndian, uint32(len(path)))
	args.WriteString(path)
	for args.Len()%4 != 0 {
		args.WriteByte(0)
	}
	mp := mountAddr(t, s, pmAddr)
	rep := rpcCall(t, mp, 100005, 1, 1, args.Bytes())
	if st := u32(rep, 0); st != 0 {
		t.Fatalf("MNT status = %d", st)
	}
	return rep[4 : 4+32]
}

// mountAddr resolves mountd's port through portmap, the way a real
// client would.
func mountAddr(t *testing.T, s *nfs.Server, pm *net.UDPAddr) *net.UDPAddr {
	t.Helper()
	var args bytes.Buffer
	binary.Write(&args, binary.BigEndian, uint32(100005))
	binary.Write(&args, binary.BigEndian, uint32(1))
	binary.Write(&args, binary.BigEndian, uint32(17))
	binary.Write(&args, binary.BigEndian, uint32(0))
	rep := rpcCall(t, pm, 100000, 2, 3, args.Bytes())
	return &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: int(u32(rep, 0))}
}

func nfsAddr(t *testing.T, s *nfs.Server, pm *net.UDPAddr) *net.UDPAddr {
	t.Helper()
	var args bytes.Buffer
	binary.Write(&args, binary.BigEndian, uint32(100003))
	binary.Write(&args, binary.BigEndian, uint32(2))
	binary.Write(&args, binary.BigEndian, uint32(17))
	binary.Write(&args, binary.BigEndian, uint32(0))
	rep := rpcCall(t, pm, 100000, 2, 3, args.Bytes())
	return &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: int(u32(rep, 0))}
}

func TestMountAndGetattr(t *testing.T) {
	s, pm := startNFS(t)
	fh := mnt(t, s, pm, "/")
	rep := rpcCall(t, nfsAddr(t, s, pm), 100003, 2, 1, fh) // GETATTR
	if st := u32(rep, 0); st != 0 {
		t.Fatalf("GETATTR status = %d", st)
	}
	if ftype := u32(rep, 4); ftype != 2 { // NFDIR
		t.Fatalf("type = %d, want NFDIR", ftype)
	}
}

func TestLookupReadAndReadlink(t *testing.T) {
	s, pm := startNFS(t)
	root := mnt(t, s, pm, "/")
	na := nfsAddr(t, s, pm)

	// LOOKUP hello.txt
	var args bytes.Buffer
	args.Write(root)
	binary.Write(&args, binary.BigEndian, uint32(len("hello.txt")))
	args.WriteString("hello.txt")
	for args.Len()%4 != 0 {
		args.WriteByte(0)
	}
	rep := rpcCall(t, na, 100003, 2, 4, args.Bytes())
	if st := u32(rep, 0); st != 0 {
		t.Fatalf("LOOKUP status = %d", st)
	}
	fh := rep[4:36]
	attrs := rep[36:]
	if u32(attrs, 0) != 1 { // NFREG
		t.Fatalf("lookup type = %d", u32(attrs, 0))
	}
	if u32(attrs, 20) != 700 { // size
		t.Fatalf("lookup size = %d", u32(attrs, 20))
	}

	// READ 100 bytes at offset 600
	args.Reset()
	args.Write(fh)
	binary.Write(&args, binary.BigEndian, uint32(600)) // offset
	binary.Write(&args, binary.BigEndian, uint32(200)) // count
	binary.Write(&args, binary.BigEndian, uint32(0))   // totalcount
	rep = rpcCall(t, na, 100003, 2, 6, args.Bytes())
	if st := u32(rep, 0); st != 0 {
		t.Fatalf("READ status = %d", st)
	}
	dataLen := u32(rep, 4+68) // after attrs
	if dataLen != 100 {
		t.Fatalf("read len = %d, want 100 (EOF-clamped)", dataLen)
	}

	// LOOKUP link, READLINK
	args.Reset()
	args.Write(root)
	binary.Write(&args, binary.BigEndian, uint32(4))
	args.WriteString("link")
	rep = rpcCall(t, na, 100003, 2, 4, args.Bytes())
	if st := u32(rep, 0); st != 0 {
		t.Fatalf("LOOKUP link status = %d", st)
	}
	lfh := rep[4:36]
	rep = rpcCall(t, na, 100003, 2, 5, lfh)
	if st := u32(rep, 0); st != 0 {
		t.Fatalf("READLINK status = %d", st)
	}
	tlen := u32(rep, 4)
	if string(rep[8:8+tlen]) != "hello.txt" {
		t.Fatalf("readlink = %q", rep[8:8+tlen])
	}
}

func TestReaddir(t *testing.T) {
	s, pm := startNFS(t)
	root := mnt(t, s, pm, "/")
	na := nfsAddr(t, s, pm)

	var args bytes.Buffer
	args.Write(root)
	binary.Write(&args, binary.BigEndian, uint32(0))    // cookie
	binary.Write(&args, binary.BigEndian, uint32(4096)) // count
	rep := rpcCall(t, na, 100003, 2, 16, args.Bytes())
	if st := u32(rep, 0); st != 0 {
		t.Fatalf("READDIR status = %d", st)
	}
	// walk entries: value-follows u32, fileid, name, cookie
	names := map[string]bool{}
	off := 4
	for u32(rep, off) == 1 {
		off += 4
		off += 4 // fileid
		nl := int(u32(rep, off))
		off += 4
		names[string(rep[off:off+nl])] = true
		off += (nl + 3) &^ 3
		off += 4 // cookie
	}
	off += 4 // skip value-follows false
	if eof := u32(rep, off); eof != 1 {
		t.Fatalf("eof = %d", eof)
	}
	if !names["hello.txt"] || !names["link"] {
		t.Fatalf("names = %v", names)
	}
}

func TestWriteRefused(t *testing.T) {
	s, pm := startNFS(t)
	root := mnt(t, s, pm, "/")
	na := nfsAddr(t, s, pm)
	// WRITE (proc 8): fh + beginoffset + offset + totalcount + data
	var args bytes.Buffer
	args.Write(root)
	for i := 0; i < 3; i++ {
		binary.Write(&args, binary.BigEndian, uint32(0))
	}
	binary.Write(&args, binary.BigEndian, uint32(1))
	args.WriteByte('x')
	args.Write([]byte{0, 0, 0})
	rep := rpcCall(t, na, 100003, 2, 8, args.Bytes())
	if st := u32(rep, 0); st != 30 { // NFSERR_ROFS
		t.Fatalf("WRITE status = %d, want 30 (read-only fs)", st)
	}
}

// syncLogBuf is a mutex-protected buffer: the server logs from its own
// goroutines while a test is still making RPC calls on another.
type syncLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestLookupLogsOpenOncePerRegularFile is the requirement: a per-open
// manifest, not a per-RPC trace. One LOOKUP of a regular file logs one
// INFO line naming it; GETATTR/READ against the handle LOOKUP already
// returned add nothing more, and a LOOKUP that resolves to a directory
// or a symlink - not the thing being "opened" - never logs at all.
func TestLookupLogsOpenOncePerRegularFile(t *testing.T) {
	var logbuf syncLogBuf
	logger := logging.New(&logbuf, logging.LevelInfo)
	fs := testFS()
	fs.byID[10].describe = "test.iso:/hello.txt" // the regular file, id 10
	s, pm := startNFSServer(t, &nfs.Server{FS: fs, Logger: logger})
	root := mnt(t, s, pm, "/")
	na := nfsAddr(t, s, pm)

	lookup := func(fh []byte, name string) []byte {
		var b bytes.Buffer
		b.Write(fh)
		binary.Write(&b, binary.BigEndian, uint32(len(name)))
		b.WriteString(name)
		for b.Len()%4 != 0 {
			b.WriteByte(0)
		}
		return rpcCall(t, na, 100003, 2, 4, b.Bytes()) // LOOKUP
	}

	rep := lookup(root, "hello.txt")
	if st := u32(rep, 0); st != 0 {
		t.Fatalf("LOOKUP hello.txt status = %d", st)
	}
	fh := rep[4:36]
	if got := strings.Count(logbuf.String(), "nfs: opened test.iso:/hello.txt"); got != 1 {
		t.Fatalf("logged %d opens after one LOOKUP, want 1:\n%s", got, logbuf.String())
	}

	rpcCall(t, na, 100003, 2, 1, fh) // GETATTR
	var readArgs bytes.Buffer
	readArgs.Write(fh)
	binary.Write(&readArgs, binary.BigEndian, uint32(0))
	binary.Write(&readArgs, binary.BigEndian, uint32(100))
	binary.Write(&readArgs, binary.BigEndian, uint32(0))
	rpcCall(t, na, 100003, 2, 6, readArgs.Bytes()) // READ
	if got := strings.Count(logbuf.String(), "nfs: opened"); got != 1 {
		t.Fatalf("GETATTR/READ against an open handle logged again: %d opens, want 1:\n%s", got, logbuf.String())
	}

	if rep := lookup(root, "link"); u32(rep, 0) != 0 {
		t.Fatalf("LOOKUP link status = %d", u32(rep, 0))
	}
	if got := strings.Count(logbuf.String(), "nfs: opened"); got != 1 {
		t.Fatalf("LOOKUP of a symlink logged an open: %d opens, want still 1:\n%s", got, logbuf.String())
	}
}

func TestStaleHandle(t *testing.T) {
	s, pm := startNFS(t)
	na := nfsAddr(t, s, pm)
	bogus := make([]byte, 32)
	bogus[0] = 1
	binary.BigEndian.PutUint64(bogus[1:], 999999)
	rep := rpcCall(t, na, 100003, 2, 1, bogus)
	if st := u32(rep, 0); st != 70 { // NFSERR_STALE
		t.Fatalf("GETATTR bogus = %d, want 70 (stale)", st)
	}
}

var _ = fmt.Sprintf
