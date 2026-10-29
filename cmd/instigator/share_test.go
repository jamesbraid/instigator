package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// rpc builds an ONC-RPC call with AUTH_NULL credentials.
func rpc(xid, prog, vers, proc uint32, args []byte) []byte {
	b := binary.BigEndian.AppendUint32(nil, xid)
	for _, v := range []uint32{0, 2, prog, vers, proc, 0, 0, 0, 0} {
		b = binary.BigEndian.AppendUint32(b, v)
	}
	return append(b, args...)
}

func xdrString(s string) []byte {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(s)))
	b = append(b, s...)
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}

// tcpCall sends one record-marked call and returns the reply body after
// the accepted-reply header.
func tcpCall(t *testing.T, c net.Conn, call []byte) []byte {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(append(binary.BigEndian.AppendUint32(nil, 0x80000000|uint32(len(call))), call...)); err != nil {
		t.Fatal(err)
	}
	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, binary.BigEndian.Uint32(hdr[:])&0x7fffffff)
	if _, err := io.ReadFull(c, reply); err != nil {
		t.Fatal(err)
	}
	if len(reply) < 24 || binary.BigEndian.Uint32(reply[20:]) != 0 {
		t.Fatalf("call not accepted: %x", reply)
	}
	return reply[24:]
}

// TestShareServesTheDirectory runs the share command on free ports,
// mounts over TCP, creates a file and checks it lands on the host, then
// pings NFS over UDP.
func TestShareServesTheDirectory(t *testing.T) {
	dir := t.TempDir()
	stop := make(chan os.Signal)
	type addrs struct{ udp, tcp []net.Addr }
	up := make(chan addrs, 1)
	done := make(chan error, 1)
	var log bytes.Buffer
	go func() {
		done <- serveShare(shareOptions{
			name: "src", dir: dir, listen: []string{"127.0.0.1"},
			udp: true, tcp: true, handleTable: filepath.Join(t.TempDir(), "handles"),
		}, &log, stop, func(udp, tcp []net.Addr) { up <- addrs{udp, tcp} })
	}()
	var a addrs
	select {
	case a = <-up:
	case err := <-done:
		t.Fatalf("share exited: %v", err)
	}
	if len(a.udp) != 1 || len(a.tcp) != 1 || a.udp[0].(*net.UDPAddr).Port != a.tcp[0].(*net.TCPAddr).Port {
		t.Fatalf("listeners udp %v tcp %v, want one each on one port", a.udp, a.tcp)
	}

	c, err := net.Dial("tcp", a.tcp[0].String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	mnt := tcpCall(t, c, rpc(1, 100005, 3, 1, xdrString("/src")))
	if binary.BigEndian.Uint32(mnt) != 0 {
		t.Fatalf("MNT /src status %d", binary.BigEndian.Uint32(mnt))
	}
	root := mnt[8 : 8+32]
	args := binary.BigEndian.AppendUint32(nil, 32)
	args = append(args, root...)
	args = append(args, xdrString("hello")...)
	args = binary.BigEndian.AppendUint32(args, 1) // GUARDED
	for range 6 {
		args = binary.BigEndian.AppendUint32(args, 0) // no attributes set
	}
	if st := binary.BigEndian.Uint32(tcpCall(t, c, rpc(2, 100003, 3, 8, args))); st != 0 {
		t.Fatalf("CREATE status %d", st)
	}
	if _, err := os.Stat(filepath.Join(dir, "hello")); err != nil {
		t.Fatalf("created file not on the host: %v", err)
	}

	u, err := net.Dial("udp", a.udp[0].String())
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	u.SetDeadline(time.Now().Add(5 * time.Second))
	u.Write(rpc(3, 100003, 2, 0, nil))
	buf := make([]byte, 128)
	if n, err := u.Read(buf); err != nil || n != 24 {
		t.Fatalf("UDP NULL reply %x, %v", buf[:n], err)
	}

	// Stopping must not wait for the open TCP connection to go away.
	close(stop)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("share did not stop with a client connected")
	}
}

func TestShareNeedsATransport(t *testing.T) {
	err := serveShare(shareOptions{name: "s", dir: t.TempDir(), listen: []string{"127.0.0.1"}}, io.Discard, nil, nil)
	if err == nil {
		t.Fatal("share with no transport started")
	}
}
