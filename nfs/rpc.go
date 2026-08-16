package nfs

import (
	"errors"
	"net"
	"net/netip"

	"github.com/jamesbraid/instigator/internal/logging"
)

// ONC-RPC constants (RFC 5531).
const (
	callMsg  = 0
	replyMsg = 1

	msgAccepted = 0

	// accept_stat
	success     = 0
	progUnavail = 1
	procUnavail = 3
	garbageArgs = 4
)

// program numbers.
const (
	progPortmap = 100000
	progNFS     = 100003
	progMount   = 100005
)

// handlerFunc decodes a call's arguments and returns the reply body.
type handlerFunc func(caller net.Addr, args *xdrReader) []byte

// serveRPC reads ONC-RPC calls on pc and dispatches by procedure. prog
// and vers identify this service; dispatch maps procedure to handler.
func (s *Server) serveRPC(pc net.PacketConn, prog, vers uint32, dispatch map[uint32]handlerFunc) error {
	buf := make([]byte, 65536)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		if reply := s.dispatch(addr, pkt, prog, vers, dispatch); reply != nil {
			pc.WriteTo(reply, addr)
		}
	}
}

func (s *Server) dispatch(addr net.Addr, pkt []byte, prog, vers uint32, dispatch map[uint32]handlerFunc) []byte {
	r := &xdrReader{b: pkt}
	xid := r.uint32()
	if r.uint32() != callMsg {
		return nil
	}
	if r.uint32() != 2 { // RPC version
		return nil
	}
	cprog := r.uint32()
	cvers := r.uint32()
	cproc := r.uint32()
	// AUTH: flavor+opaque for cred and verf
	r.uint32()
	skipOpaque(r)
	r.uint32()
	skipOpaque(r)
	if r.err {
		return replyAccepted(xid, garbageArgs, nil)
	}

	if s.AllowIP != nil {
		if udp, ok := addr.(*net.UDPAddr); ok {
			ip, _ := netip.AddrFromSlice(udp.IP)
			if !s.AllowIP(ip.Unmap()) {
				s.Logger.Warnf("nfs: %s: denied by client filter", addr)
				return nil
			}
		}
	}

	if cprog != prog {
		return replyAccepted(xid, progUnavail, nil)
	}
	_ = cvers
	h, ok := dispatch[cproc]
	if !ok {
		if s.Logger.Enabled(logging.LevelDebug) {
			s.Logger.Debugf("nfs: %s: prog=%d proc=%d unavailable", addr, cprog, cproc)
		}
		return replyAccepted(xid, procUnavail, nil)
	}
	if s.Logger.Enabled(logging.LevelDebug) {
		s.Logger.Debugf("nfs: %s: prog=%d vers=%d proc=%d", addr, cprog, cvers, cproc)
	}
	body := h(addr, r)
	if r.err {
		return replyAccepted(xid, garbageArgs, nil)
	}
	return replyAccepted(xid, success, body)
}

func skipOpaque(r *xdrReader) {
	n := int(r.uint32())
	if n < 0 {
		r.err = true
		return
	}
	r.off += (n + 3) &^ 3
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
