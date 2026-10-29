package nfs

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/jamesbraid/instigator/internal/logging"
)

// ONC-RPC constants (RFC 5531).
const (
	callMsg  = 0
	replyMsg = 1

	msgAccepted = 0
	msgDenied   = 1

	// accept_stat
	success      = 0
	progUnavail  = 1
	progMismatch = 2
	procUnavail  = 3
	garbageArgs  = 4

	// reject_stat
	rpcMismatch = 0
)

// program numbers.
const (
	progPortmap = 100000
	progNFS     = 100003
	progMount   = 100005
)

// handlerFunc decodes a call's arguments and returns the reply body.
type handlerFunc func(caller net.Addr, args *xdrReader) []byte

type procKey struct{ prog, vers, proc uint32 }

// rpcTable is every procedure the server answers, and the version range
// of each program for PROG_MISMATCH replies.
type rpcTable struct {
	procs    map[procKey]handlerFunc
	versions map[uint32][2]uint32
}

func (s *Server) table() *rpcTable {
	s.tableOnce.Do(func() {
		if s.Verifier == ([8]byte{}) {
			binary.BigEndian.PutUint64(s.Verifier[:], uint64(time.Now().UnixNano()))
		}
		t := &rpcTable{procs: map[procKey]handlerFunc{}, versions: map[uint32][2]uint32{}}
		add := func(prog, vers uint32, procs map[uint32]handlerFunc) {
			for proc, h := range procs {
				t.procs[procKey{prog, vers, proc}] = h
			}
			r, ok := t.versions[prog]
			if !ok {
				r = [2]uint32{vers, vers}
			}
			t.versions[prog] = [2]uint32{min(r[0], vers), max(r[1], vers)}
		}
		add(progPortmap, 2, s.portmapProcs())
		add(progMount, 1, s.mountProcs(1))
		add(progMount, 3, s.mountProcs(3))
		add(progNFS, 2, s.nfs2Procs())
		add(progNFS, 3, s.nfs3Procs())
		s.rpc = t
	})
	return s.rpc
}

func null(net.Addr, *xdrReader) []byte { return nil }

// udpBuffer is the send and receive buffer ServeUDP asks for. The largest
// reply is a 32 KiB READ plus headers, and a host refuses a datagram bigger
// than the send buffer: macOS sizes it from net.inet.udp.maxdgram, 9216
// bytes, and fails the send with EMSGSIZE. Linux and Windows defaults
// already hold 64 KiB. A host may clamp the request.
const udpBuffer = 256 * 1024

// ServeUDP answers every program the server speaks - portmap, MOUNT and
// NFS - on pc, until pc is closed. Calls are answered one at a time. It
// enlarges pc's socket buffers when pc allows it, as a *net.UDPConn does.
func (s *Server) ServeUDP(pc net.PacketConn) error {
	if b, ok := pc.(interface {
		SetReadBuffer(int) error
		SetWriteBuffer(int) error
	}); ok {
		if err := errors.Join(b.SetReadBuffer(udpBuffer), b.SetWriteBuffer(udpBuffer)); err != nil {
			s.Logger.Warnf("nfs: raising the UDP buffers: %v; large replies may be dropped", err)
		}
	}
	buf := make([]byte, 65536)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if !s.allowed(addr) {
			continue
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		reply, ok := s.drc.lookup(addr, pkt)
		if !ok {
			reply = s.dispatch(addr, pkt)
			s.drc.store(addr, pkt, reply)
		}
		if reply != nil {
			if _, err := pc.WriteTo(reply, addr); err != nil && !errors.Is(err, net.ErrClosed) {
				s.Logger.Warnf("nfs: %s: dropped a %d-byte reply: %v", addr, len(reply), err)
			}
		}
	}
}

// drc is a duplicate request cache for UDP. A client that times out
// resends the same call, and a second REMOVE or RENAME would fail
// although the first succeeded, so the replies to those calls are kept
// and resent instead. Idempotent calls - reads, and WRITE of the same
// bytes to the same place - just run again.
type drc struct {
	mu   sync.Mutex
	ent  [64]drcEntry
	next int
}

type drcEntry struct {
	addr        string
	call, reply []byte
}

// nonIdempotent reports whether pkt is a call drc should keep: an NFS
// SETATTR, CREATE, REMOVE, RENAME, LINK, SYMLINK, MKDIR, RMDIR or MKNOD.
func nonIdempotent(pkt []byte) bool {
	if len(pkt) < 24 || binary.BigEndian.Uint32(pkt[12:]) != progNFS {
		return false
	}
	proc := binary.BigEndian.Uint32(pkt[20:])
	switch binary.BigEndian.Uint32(pkt[16:]) {
	case 2:
		return proc == 2 || proc >= 9 && proc <= 15
	case 3:
		return proc == 2 || proc >= 8 && proc <= 15
	}
	return false
}

func (d *drc) lookup(addr net.Addr, call []byte) ([]byte, bool) {
	if !nonIdempotent(call) {
		return nil, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	a := addr.String()
	for _, e := range d.ent {
		if e.addr == a && bytes.Equal(e.call, call) {
			return e.reply, true
		}
	}
	return nil, false
}

func (d *drc) store(addr net.Addr, call, reply []byte) {
	if !nonIdempotent(call) {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ent[d.next] = drcEntry{addr: addr.String(), call: call, reply: reply}
	d.next = (d.next + 1) % len(d.ent)
}

// maxRecord bounds one TCP call. The largest legitimate call is a WRITE
// of wtmax bytes plus its header; anything far beyond that is a broken
// or hostile client, and the connection is dropped.
const maxRecord = 1 << 20

// ServeTCP answers every program the server speaks on connections
// accepted from l, until l is closed. Each connection has its own
// goroutine and is answered one call at a time, with RFC 5531 record
// marking.
//
// When l closes, ServeTCP closes the connections it accepted and returns
// once they are done.
func (s *Server) ServeTCP(l net.Listener) error {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		conns = map[net.Conn]bool{}
	)
	defer func() {
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if !s.allowed(c.RemoteAddr()) {
			c.Close()
			continue
		}
		mu.Lock()
		conns[c] = true
		mu.Unlock()
		wg.Go(func() {
			s.serveConn(c)
			c.Close()
			mu.Lock()
			delete(conns, c)
			mu.Unlock()
		})
	}
}

func (s *Server) serveConn(c net.Conn) {
	r := bufio.NewReader(c)
	for {
		call, err := readRecord(r)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				s.Logger.Warnf("nfs: %s: %v", c.RemoteAddr(), err)
			}
			return
		}
		reply := s.dispatch(c.RemoteAddr(), call)
		if reply == nil {
			continue
		}
		if _, err := c.Write(recordMark(reply)); err != nil {
			return
		}
	}
}

var errRecordTooLarge = errors.New("rpc record too large")

// readRecord reads one record: fragments, each behind a 4-byte header
// whose top bit marks the last and whose low 31 bits are its length
// (RFC 5531 section 11).
func readRecord(r io.Reader) ([]byte, error) {
	var rec []byte
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if len(rec) > 0 && errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		h := binary.BigEndian.Uint32(hdr[:])
		n := int(h & 0x7fffffff)
		if len(rec)+n > maxRecord {
			return nil, errRecordTooLarge
		}
		start := len(rec)
		rec = append(rec, make([]byte, n)...)
		if _, err := io.ReadFull(r, rec[start:]); err != nil {
			return nil, io.ErrUnexpectedEOF
		}
		if h&0x80000000 != 0 {
			return rec, nil
		}
	}
}

// recordMark frames msg as a single last fragment.
func recordMark(msg []byte) []byte {
	out := binary.BigEndian.AppendUint32(make([]byte, 0, 4+len(msg)), 0x80000000|uint32(len(msg)))
	return append(out, msg...)
}

func (s *Server) allowed(addr net.Addr) bool {
	if s.AllowIP == nil {
		return true
	}
	var ip netip.Addr
	switch a := addr.(type) {
	case *net.UDPAddr:
		ip, _ = netip.AddrFromSlice(a.IP)
	case *net.TCPAddr:
		ip, _ = netip.AddrFromSlice(a.IP)
	default:
		return false
	}
	if !s.AllowIP(ip.Unmap()) {
		s.Logger.Warnf("nfs: %s: denied by client filter", addr)
		return false
	}
	return true
}

// dispatch decodes one call message and returns the reply message, or
// nil when the message is not a call.
func (s *Server) dispatch(addr net.Addr, pkt []byte) []byte {
	r := &xdrReader{b: pkt}
	xid := r.uint32()
	if r.uint32() != callMsg || r.err {
		return nil
	}
	if r.uint32() != 2 { // RPC version
		return replyDenied(xid)
	}
	prog := r.uint32()
	vers := r.uint32()
	proc := r.uint32()
	// Credential and verifier, each a flavor and an opaque body. Every
	// caller is treated alike, so neither is examined.
	r.uint32()
	skipOpaque(r)
	r.uint32()
	skipOpaque(r)
	if r.err {
		return replyAccepted(xid, garbageArgs, nil)
	}

	t := s.table()
	vr, ok := t.versions[prog]
	if !ok {
		return replyAccepted(xid, progUnavail, nil)
	}
	// Every served version answers NULL, so a missing NULL is a missing
	// version.
	if _, ok := t.procs[procKey{prog, vers, 0}]; !ok {
		w := &xdrWriter{}
		w.uint32(vr[0])
		w.uint32(vr[1])
		return replyAccepted(xid, progMismatch, w.b)
	}
	h, ok := t.procs[procKey{prog, vers, proc}]
	if !ok {
		if s.Logger.Enabled(logging.LevelDebug) {
			s.Logger.Debugf("nfs: %s: prog=%d vers=%d proc=%d unavailable", addr, prog, vers, proc)
		}
		return replyAccepted(xid, procUnavail, nil)
	}
	if s.Logger.Enabled(logging.LevelDebug) {
		s.Logger.Debugf("nfs: %s: prog=%d vers=%d proc=%d", addr, prog, vers, proc)
	}
	body := h(addr, r)
	if r.err {
		return replyAccepted(xid, garbageArgs, nil)
	}
	return replyAccepted(xid, success, body)
}

func skipOpaque(r *xdrReader) {
	n := r.uint32()
	if n > uint32(len(r.b)) {
		r.err = true
		return
	}
	r.off += (int(n) + 3) &^ 3
	if r.off > len(r.b) {
		r.err = true
	}
}

// replyAccepted frames an accepted RPC reply: xid, REPLY, MSG_ACCEPTED,
// null verifier, accept_stat, then the body.
func replyAccepted(xid, acceptStat uint32, body []byte) []byte {
	w := &xdrWriter{}
	w.uint32(xid)
	w.uint32(replyMsg)
	w.uint32(msgAccepted)
	w.uint32(0) // verf flavor AUTH_NULL
	w.uint32(0) // verf length
	w.uint32(acceptStat)
	w.fixed(body)
	return w.b
}

// replyDenied refuses a call for an RPC version other than 2, naming the
// one version served.
func replyDenied(xid uint32) []byte {
	w := &xdrWriter{}
	w.uint32(xid)
	w.uint32(replyMsg)
	w.uint32(msgDenied)
	w.uint32(rpcMismatch)
	w.uint32(2)
	w.uint32(2)
	return w.b
}
