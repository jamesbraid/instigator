package qemunet

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/internal/tftp"
)

// memFS is an in-memory tftp.FileSystem for the functional tests.
type memFS map[string][]byte

func (m memFS) Open(path string) (tftp.File, error) {
	b, ok := m[path]
	if !ok {
		if b, ok = m["/"+path]; !ok {
			return nil, tftp.ErrNotFound
		}
	}
	return bytes.NewReader(b), nil
}

// tftpRead performs a classic in-order RRQ read (octet, 512-byte blocks) of
// name from srvIP:69 over conn, returning the assembled file.
func tftpRead(t *testing.T, conn net.PacketConn, srvIP net.IP, name string) []byte {
	t.Helper()
	rrq := []byte{0, 1}
	rrq = append(rrq, name...)
	rrq = append(rrq, 0)
	rrq = append(rrq, "octet"...)
	rrq = append(rrq, 0)
	if _, err := conn.WriteTo(rrq, &net.UDPAddr{IP: srvIP, Port: 69}); err != nil {
		t.Fatalf("tftp RRQ: %v", err)
	}

	var out []byte
	var expect uint16 = 1
	buf := make([]byte, 2048)
	for {
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			t.Fatalf("tftp read (want block %d): %v", expect, err)
		}
		if n < 4 || binary.BigEndian.Uint16(buf[0:2]) != 3 { // opDATA
			t.Fatalf("tftp: unexpected %d-byte packet op=%d", n, binary.BigEndian.Uint16(buf[0:2]))
		}
		blk := binary.BigEndian.Uint16(buf[2:4])
		if blk != expect {
			continue // out-of-order; wait for the retransmit
		}
		out = append(out, buf[4:n]...)
		ack := []byte{0, 4, byte(blk >> 8), byte(blk)}
		if _, err := conn.WriteTo(ack, from); err != nil { // ack the transfer socket
			t.Fatalf("tftp ACK: %v", err)
		}
		if n-4 < 512 {
			return out // short block ends the transfer
		}
		expect++
	}
}

// TestTftpServesOverPrivateNetwork runs the real tftp.Server on the private
// segment - including its per-transfer socket, bound on the user-space stack
// via the injected factory - and reads a known file from the guest across the
// framed stream.
func TestTftpServesOverPrivateNetwork(t *testing.T) {
	srv, gst := pipedNetworks(t)

	want := bytes.Repeat([]byte("SGI-IRIX-"), 400) // 3600 bytes, several blocks
	ts := &tftp.Server{
		FS:           memFS{"unix": want},
		ListenPacket: srv.ListenPacket,
		AllowIP:      func(a netip.Addr) bool { return a == netip.AddrFrom4(testGstIP) },
	}
	pc, err := srv.ListenPacket(69)
	if err != nil {
		t.Fatalf("srv.ListenPacket(69): %v", err)
	}
	defer pc.Close()
	go ts.Serve(pc)

	client, err := gst.ListenPacket(0)
	if err != nil {
		t.Fatalf("gst.ListenPacket(0): %v", err)
	}
	defer client.Close()

	got := tftpRead(t, client, testSrvIPAddr(), "unix")
	if !bytes.Equal(got, want) {
		t.Fatalf("tftp payload mismatch: got %d bytes, want %d", len(got), len(want))
	}
}
