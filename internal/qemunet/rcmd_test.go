package qemunet

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/rcmd"
)

// TestRcmdServesOverPrivateNetwork runs the real rcmd.Server on the private
// segment, drives an rsh request from the guest with a reserved source port,
// and confirms the server dials the stderr callback back to the guest through
// the injected stack dialer - delivering the handler's output over it, all
// across the framed stream.
func TestRcmdServesOverPrivateNetwork(t *testing.T) {
	srv, gst := pipedNetworks(t)

	const marker = "hello-from-instigator\n"
	rs := &rcmd.Server{
		AllowIP:    func(a netip.Addr) bool { return a == netip.AddrFrom4(testGstIP) },
		DialStderr: srv.DialStderr,
		Handler: func(req *rcmd.Request) error {
			fmt.Fprint(req.Stderr, marker)
			return nil
		},
	}
	ln, err := srv.Listen(514)
	if err != nil {
		t.Fatalf("srv.Listen(514): %v", err)
	}
	defer ln.Close()
	go rs.Serve(ln)

	// Guest listens for the stderr callback, then makes the rsh request. The
	// primary connection is opened from a reserved source port - the same
	// reserved-source dial the server uses for its callback.
	cbLn, err := gst.Listen(0)
	if err != nil {
		t.Fatalf("guest callback Listen: %v", err)
	}
	defer cbLn.Close()
	cbPort := cbLn.Addr().(*net.TCPAddr).Port

	primary, err := gst.DialStderr(testSrvIPAddr(), 514)
	if err != nil {
		t.Fatalf("guest rsh dial :514: %v", err)
	}
	defer primary.Close()

	// rcmd protocol: stderr port, then the server calls back, then the user
	// and command fields, then a 0 ack byte.
	fmt.Fprintf(primary, "%d\x00", cbPort)

	cbAccepted, err := cbLn.Accept()
	if err != nil {
		t.Fatalf("guest never received the stderr callback: %v", err)
	}
	defer cbAccepted.Close()

	fmt.Fprintf(primary, "root\x00root\x00exec /bin/sh\x00")
	ack := make([]byte, 1)
	if _, err := io.ReadFull(primary, ack); err != nil {
		t.Fatalf("rsh ack read: %v", err)
	}
	if ack[0] != 0 {
		t.Fatalf("rsh ack = %d, want 0", ack[0])
	}

	cbAccepted.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(cbAccepted).ReadString('\n')
	if err != nil {
		t.Fatalf("read handler output over callback: %v", err)
	}
	if line != marker {
		t.Fatalf("callback delivered %q, want %q", line, marker)
	}
}
