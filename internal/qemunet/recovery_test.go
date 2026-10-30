package qemunet

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// The loss-recovery tests drive the server's TCP from a scripted machine
// that behaves like IRIX's: it offers SACK but no timestamps, acknowledges
// every segment it accepts at once, and loses the segments a test chooses -
// the way a guest NIC that overran its receive ring and reset loses a
// burst. Everything crosses the real stream framing and frame pump.

// tcpPeer is the machine's end of one TCP connection to the server.
type tcpPeer struct {
	t      *testing.T
	conn   net.Conn
	frames chan []byte
	wmu    sync.Mutex
	w      frameWriter

	sport, dport uint16
	sndNxt       uint32 // our next sequence number
	rcvNxt       uint32 // next in-order byte expected from the server
	got          bytes.Buffer
	ooo          map[uint32][]byte // out-of-order segments by sequence number

	// drop reports whether to lose the n'th data segment the server
	// sends (counting from 0, retransmissions included).
	drop  func(n int, seq uint32, payload []byte) bool
	nseg  int
	arriv []segArrival
}

type segArrival struct {
	at  time.Time
	seq uint32
	n   int
}

// recoveryNetwork starts a server Network and returns the machine's end of
// its stream with ARP for the guest address answered.
func recoveryNetwork(t *testing.T) (*Network, *tcpPeer) {
	t.Helper()
	conn, peer := net.Pipe()
	n, err := New(conn, Config{ServerIP: netip.AddrFrom4(testSrvIP), PrefixLen: 24, MAC: net.HardwareAddr([]byte(testSrvMAC))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close(); n.Close() })
	p := &tcpPeer{t: t, conn: peer, frames: make(chan []byte, 1024), w: frameWriter{w: peer}, ooo: map[uint32][]byte{}}
	go func() {
		r := frameReader{r: peer}
		for {
			f, err := r.read(maxFrame)
			if err != nil {
				close(p.frames)
				return
			}
			f = append([]byte(nil), f...)
			// Answer the server's ARP for the guest here, so the
			// segment loop sees only IPv4.
			if binary.BigEndian.Uint16(f[12:14]) == 0x0806 && binary.BigEndian.Uint16(f[20:22]) == 1 &&
				bytes.Equal(f[38:42], testGstIP[:]) {
				p.send(ethFrame(serverMAC, guestMAC, 0x0806, arpMsg(2, guestMAC, testGstIP, serverMAC, testSrvIP)))
				continue
			}
			p.frames <- f
		}
	}()
	return n, p
}

func (p *tcpPeer) send(frame []byte) {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if err := p.w.write(frame); err != nil {
		p.t.Errorf("peer write: %v", err)
	}
}

// segment builds and sends one TCP segment from the machine.
func (p *tcpPeer) segment(flags byte, seq, ack uint32, win uint16, opts []byte, payload []byte) {
	for len(opts)%4 != 0 {
		opts = append(opts, 1) // NOP
	}
	h := make([]byte, 20+len(opts), 20+len(opts)+len(payload))
	binary.BigEndian.PutUint16(h[0:], p.sport)
	binary.BigEndian.PutUint16(h[2:], p.dport)
	binary.BigEndian.PutUint32(h[4:], seq)
	binary.BigEndian.PutUint32(h[8:], ack)
	h[12] = byte((20 + len(opts)) / 4 << 4)
	h[13] = flags
	binary.BigEndian.PutUint16(h[14:], win)
	copy(h[20:], opts)
	h = append(h, payload...)
	pseudo := append(append(append([]byte(nil), testGstIP[:]...), testSrvIP[:]...), 0, 6, byte(len(h)>>8), byte(len(h)))
	binary.BigEndian.PutUint16(h[16:], wireChecksum(append(pseudo, h...)))
	p.send(ethFrame(serverMAC, guestMAC, 0x0800, ipv4Packet(testGstIP, testSrvIP, 6, 0, 64, h)))
}

const (
	tcpFIN = 0x01
	tcpSYN = 0x02
	tcpACK = 0x10
)

type tcpSeg struct {
	flags    byte
	seq, ack uint32
	payload  []byte
}

// next returns the next TCP segment the server sends, or ok=false after d.
func (p *tcpPeer) next(d time.Duration) (tcpSeg, bool) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case f, ok := <-p.frames:
			if !ok {
				return tcpSeg{}, false
			}
			if binary.BigEndian.Uint16(f[12:14]) != 0x0800 || f[23] != 6 {
				continue
			}
			ip := f[14:]
			ihl := int(ip[0]&15) * 4
			tl := int(binary.BigEndian.Uint16(ip[2:]))
			tcp := ip[ihl:tl]
			doff := int(tcp[12]>>4) * 4
			return tcpSeg{
				flags:   tcp[13],
				seq:     binary.BigEndian.Uint32(tcp[4:]),
				ack:     binary.BigEndian.Uint32(tcp[8:]),
				payload: append([]byte(nil), tcp[doff:]...),
			}, true
		case <-timer.C:
			return tcpSeg{}, false
		}
	}
}

// connect opens a connection to port as IRIX does: MSS 1460 and
// SACK-permitted, no window scaling and no timestamps.
func (p *tcpPeer) connect(port uint16) {
	p.t.Helper()
	p.sport, p.dport = 1020, port
	p.sndNxt = 1000
	p.segment(tcpSYN, p.sndNxt, 0, 61440, []byte{2, 4, 0x05, 0xb4, 4, 2}, nil)
	p.sndNxt++
	synack, ok := p.next(5 * time.Second)
	if !ok || synack.flags&(tcpSYN|tcpACK) != tcpSYN|tcpACK {
		p.t.Fatalf("no SYN-ACK: %+v", synack)
	}
	p.rcvNxt = synack.seq + 1
	p.ack()
}

// ack sends a cumulative ACK carrying SACK blocks for held out-of-order data.
func (p *tcpPeer) ack() {
	var opts []byte
	if len(p.ooo) > 0 {
		var blocks [][2]uint32
		for seq, b := range p.ooo {
			blocks = append(blocks, [2]uint32{seq, seq + uint32(len(b))})
		}
		// Merge adjacent blocks; report at most four.
		merged := mergeBlocks(blocks)
		if len(merged) > 4 {
			merged = merged[:4]
		}
		opts = append(opts, 1, 1, 5, byte(2+8*len(merged)))
		for _, b := range merged {
			opts = binary.BigEndian.AppendUint32(opts, b[0])
			opts = binary.BigEndian.AppendUint32(opts, b[1])
		}
	}
	p.segment(tcpACK, p.sndNxt, p.rcvNxt, 61440, opts, nil)
}

func mergeBlocks(in [][2]uint32) [][2]uint32 {
	// Insertion sort by start; tests hold few blocks.
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && int32(in[j][0]-in[j-1][0]) < 0; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
	var out [][2]uint32
	for _, b := range in {
		if len(out) > 0 && out[len(out)-1][1] == b[0] {
			out[len(out)-1][1] = b[1]
			continue
		}
		out = append(out, b)
	}
	return out
}

// receive accepts the server's data until want bytes arrived in order or
// d elapsed, acknowledging each segment it keeps at once.
func (p *tcpPeer) receive(want int, d time.Duration) {
	deadline := time.Now().Add(d)
	for p.got.Len() < want {
		left := time.Until(deadline)
		if left <= 0 {
			return
		}
		s, ok := p.next(left)
		if !ok {
			return
		}
		if len(s.payload) == 0 {
			continue
		}
		n := p.nseg
		p.nseg++
		p.arriv = append(p.arriv, segArrival{at: time.Now(), seq: s.seq, n: n})
		if p.drop != nil && p.drop(n, s.seq, s.payload) {
			continue
		}
		switch d := int32(s.seq - p.rcvNxt); {
		case d == 0:
			p.got.Write(s.payload)
			p.rcvNxt += uint32(len(s.payload))
			for {
				b, ok := p.ooo[p.rcvNxt]
				if !ok {
					break
				}
				delete(p.ooo, p.rcvNxt)
				p.got.Write(b)
				p.rcvNxt += uint32(len(b))
			}
		case d > 0:
			p.ooo[s.seq] = s.payload
		}
		p.ack()
	}
}

// warmConnection accepts a connection from the machine on the server and
// runs request/response exchanges over it until the congestion window is
// wide and the RTT estimate settled, as inst's earlier commands on the same
// connection leave it. It returns the server's end and what the machine has
// received so far.
func warmConnection(t *testing.T) (net.Conn, *tcpPeer, *bytes.Buffer) {
	t.Helper()
	srv, p := recoveryNetwork(t)
	ln, err := srv.Listen(514)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	p.connect(514)
	c, ok := <-accepted
	if !ok {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { c.Close() })
	var sent bytes.Buffer
	for i := 0; i < 100; i++ {
		msg := []byte("o?_InstProcIsDone\n")
		sent.Write(msg)
		if _, err := c.Write(msg); err != nil {
			t.Fatal(err)
		}
		p.receive(sent.Len(), 5*time.Second)
	}
	if p.got.Len() != sent.Len() {
		t.Fatalf("warm-up delivered %d of %d bytes", p.got.Len(), sent.Len())
	}
	p.nseg, p.arriv = 0, nil
	return c, p, &sent
}

// writeBurst writes n short lines one at a time, as a directory listing
// comes out of the shell, and records them in sent.
func writeBurst(t *testing.T, c net.Conn, sent *bytes.Buffer, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		msg := append(bytes.Repeat([]byte{byte('a' + i%26)}, 4+i%14), '\n')
		sent.Write(msg)
		if _, err := c.Write(msg); err != nil {
			t.Fatal(err)
		}
	}
}

// loseFirstSend loses the first transmission of every segment the
// predicate selects by its position in the burst; resends get through.
func loseFirstSend(lose func(n int) bool) func(int, uint32, []byte) bool {
	seen := map[uint32]bool{}
	return func(n int, seq uint32, _ []byte) bool {
		first := !seen[seq]
		seen[seq] = true
		return first && lose(n)
	}
}

func checkDelivered(t *testing.T, p *tcpPeer, sent *bytes.Buffer) {
	t.Helper()
	if !bytes.Equal(p.got.Bytes(), sent.Bytes()) {
		t.Fatalf("machine received %d of %d bytes", p.got.Len(), sent.Len())
	}
}

// TestTCPResumesWindowAfterRetransmit loses most of a burst of short
// writes, keeping the first few segments and the last, as a guest that
// overran its receive ring does. Once the first resend is acknowledged
// the sender must send the rest of the window in slow start (RFC 5681
// section 3.1), not one or two segments per backed-off timeout.
func TestTCPResumesWindowAfterRetransmit(t *testing.T) {
	c, p, sent := warmConnection(t)
	p.drop = loseFirstSend(func(n int) bool { return n >= 11 && n < 59 })
	start := time.Now()
	writeBurst(t, c, sent, 60)
	p.receive(sent.Len(), 3*time.Second)
	checkDelivered(t, p, sent)
	// One retransmit timeout, a few round trips of slow start, and
	// headroom for a loaded test host. A sender that stalls between
	// resends takes tens of seconds.
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("recovery took %v, want under 1.5s", d)
	}
}

// TestTCPTimeoutReturnsToNormal recovers from a lost burst, then loses
// one more segment. Every resend in the first recovery is acknowledged at
// once, so the second loss must be resent after the normal timeout, not
// one still doubled from the first recovery (RFC 6298 section 5, with
// Karn's algorithm taking a fresh RTT sample from new data).
func TestTCPTimeoutReturnsToNormal(t *testing.T) {
	c, p, sent := warmConnection(t)
	p.drop = loseFirstSend(func(n int) bool { return n >= 11 && n < 59 })
	writeBurst(t, c, sent, 60)
	p.receive(sent.Len(), 3*time.Second)
	checkDelivered(t, p, sent)

	// New data acknowledged normally gives the sender a clean RTT sample.
	writeBurst(t, c, sent, 1)
	p.receive(sent.Len(), 3*time.Second)
	checkDelivered(t, p, sent)

	// The next write is lost once; time its resend.
	p.nseg, p.arriv = 0, nil
	p.drop = loseFirstSend(func(n int) bool { return n == 0 })
	writeBurst(t, c, sent, 1)
	p.receive(sent.Len(), 5*time.Second)
	checkDelivered(t, p, sent)
	if len(p.arriv) < 2 {
		t.Fatalf("saw %d transmissions, want the segment and its resend", len(p.arriv))
	}
	if d := p.arriv[1].at.Sub(p.arriv[0].at); d > 350*time.Millisecond {
		t.Fatalf("resend came %v after the loss, want the 200ms minimum timeout, not a backed-off one", d)
	}
}

// TestTCPMinimumRetransmitTimeout loses the last two segments of a write
// on a connection whose measured RTT is well under a millisecond. Nothing
// is resent before the 200ms minimum timeout: an emulated guest can pause
// for far longer than its idle round trip, and a resend timed off that
// round trip fires spuriously.
func TestTCPMinimumRetransmitTimeout(t *testing.T) {
	c, p, sent := warmConnection(t)
	p.drop = loseFirstSend(func(n int) bool { return n >= 1 })
	// One write that goes out as three segments.
	msg := bytes.Repeat([]byte("x"), 3*1400)
	sent.Write(msg)
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	p.receive(sent.Len(), 5*time.Second)
	checkDelivered(t, p, sent)
	if len(p.arriv) < 4 {
		t.Fatalf("saw %d transmissions, want three segments and a resend", len(p.arriv))
	}
	for _, a := range p.arriv[3:] {
		if d := a.at.Sub(p.arriv[0].at); d < 190*time.Millisecond {
			t.Fatalf("segment %d resent %v after the first send, want at least 200ms", a.seq, d)
		}
	}
}
