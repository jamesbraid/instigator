package rcmd_test

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/rcmd"
)

func startServer(t *testing.T, s *rcmd.Server) net.Addr {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go s.Serve(l)
	return l.Addr()
}

// session runs one client exchange: connect, send the stderr port and
// the three strings, return the acceptance byte and everything after.
func session(t *testing.T, addr net.Addr, stderrPort int, remuser, locuser, command string) (byte, []byte) {
	t.Helper()
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "%d\x00%s\x00%s\x00%s\x00", stderrPort, remuser, locuser, command)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	r := bufio.NewReader(c)
	ack, err := r.ReadByte()
	if err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(r)
	return ack, rest
}

func echoHandler(req *rcmd.Request) error {
	fmt.Fprintf(req.Stdout, "cmd=%s user=%s", req.Command, req.LocalUser)
	return nil
}

func TestSessionRunsHandler(t *testing.T) {
	addr := startServer(t, &rcmd.Server{Handler: echoHandler, AllowHighPorts: true})
	ack, out := session(t, addr, 0, "guest", "guest", "echo hi")
	if ack != 0 {
		t.Fatalf("ack = %d, want 0", ack)
	}
	if string(out) != "cmd=echo hi user=guest" {
		t.Fatalf("out = %q", out)
	}
}

func TestStderrDialback(t *testing.T) {
	el, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer el.Close()
	errPort := el.Addr().(*net.TCPAddr).Port

	got := make(chan string, 1)
	go func() {
		ec, err := el.Accept()
		if err != nil {
			got <- "accept: " + err.Error()
			return
		}
		defer ec.Close()
		ec.SetReadDeadline(time.Now().Add(2 * time.Second))
		b, _ := io.ReadAll(ec)
		got <- string(b)
	}()

	addr := startServer(t, &rcmd.Server{
		AllowHighPorts: true,
		Handler: func(req *rcmd.Request) error {
			fmt.Fprint(req.Stderr, "warn: x")
			return nil
		},
	})
	ack, _ := session(t, addr, errPort, "guest", "guest", "x")
	if ack != 0 {
		t.Fatalf("ack = %d", ack)
	}
	if s := <-got; s != "warn: x" {
		t.Fatalf("stderr = %q", s)
	}
}

// TestStderrCallbackBeforeFields models the real rcmd protocol: the client
// sends the stderr port, waits for the server to connect back to it, and
// only then sends the user and command fields. A server that reads all
// fields before dialing stderr deadlocks such a client (IRIX inst).
func TestStderrCallbackBeforeFields(t *testing.T) {
	el, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer el.Close()
	errPort := el.Addr().(*net.TCPAddr).Port

	addr := startServer(t, &rcmd.Server{Handler: echoHandler, AllowHighPorts: true})
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// send only the stderr port, then wait for the server's callback
	fmt.Fprintf(c, "%d\x00", errPort)

	accepted := make(chan net.Conn, 1)
	go func() {
		ec, err := el.Accept()
		if err == nil {
			accepted <- ec
		}
	}()
	select {
	case ec := <-accepted:
		defer ec.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("server never connected back to the stderr port (deadlock)")
	}

	// only now send the rest, as a real client does
	fmt.Fprintf(c, "guest\x00guest\x00echo hi\x00")
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	r := bufio.NewReader(c)
	ack, err := r.ReadByte()
	if err != nil || ack != 0 {
		t.Fatalf("ack=%d err=%v", ack, err)
	}
	out, _ := io.ReadAll(r)
	if string(out) != "cmd=echo hi user=guest" {
		t.Fatalf("out=%q", out)
	}
}

func TestHandlerErrorReported(t *testing.T) {
	// rshd accepts (zero byte) before the command runs; a runtime
	// failure arrives as stderr text, which without a stderr channel
	// shares the primary connection
	addr := startServer(t, &rcmd.Server{
		AllowHighPorts: true,
		Handler: func(req *rcmd.Request) error {
			return fmt.Errorf("no such file")
		},
	})
	ack, out := session(t, addr, 0, "guest", "guest", "dd if=/absent")
	if ack != 0 {
		t.Fatalf("ack = %d, want 0 (accepted before execution)", ack)
	}
	if !strings.Contains(string(out), "no such file") {
		t.Fatalf("out = %q", out)
	}
}

func TestClientFilter(t *testing.T) {
	addr := startServer(t, &rcmd.Server{
		AllowHighPorts: true,
		AllowIP:        func(a netip.Addr) bool { return false },
		Handler:        echoHandler,
	})
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "0\x00g\x00g\x00x\x00")
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, _ := io.ReadAll(c)
	// connection must be refused: either closed outright or \x01 error
	if len(b) > 0 && b[0] == 0 {
		t.Fatalf("filtered client got acceptance: %q", b)
	}
}

func TestReservedPortEnforced(t *testing.T) {
	// AllowHighPorts unset: a client from an ephemeral port is refused
	addr := startServer(t, &rcmd.Server{Handler: echoHandler})
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "0\x00g\x00g\x00x\x00")
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, _ := io.ReadAll(c)
	if len(b) > 0 && b[0] == 0 {
		t.Fatalf("high-port client got acceptance: %q", b)
	}
}

// TestIdleTimeoutClosesSilentSession models a shell session (rsh exec
// /bin/sh): the handler blocks reading Request.Stdin, the way
// instcmd.RunShell does, waiting for the next command line. A client
// that sends the request head and then goes silent - stuck, hostile,
// or just gone - must not hold that goroutine (and the connection)
// open forever once IdleTimeout is set.
func TestIdleTimeoutClosesSilentSession(t *testing.T) {
	handlerDone := make(chan error, 1)
	addr := startServer(t, &rcmd.Server{
		AllowHighPorts: true,
		IdleTimeout:    100 * time.Millisecond,
		Handler: func(req *rcmd.Request) error {
			buf := make([]byte, 1)
			_, err := req.Stdin.Read(buf)
			handlerDone <- err
			return err
		},
	})

	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// send the full request head, then go silent - never write to
	// Stdin, as a stuck client would
	fmt.Fprintf(c, "0\x00guest\x00guest\x00exec /bin/sh\x00")

	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	ack := make([]byte, 1)
	if _, err := io.ReadFull(c, ack); err != nil {
		t.Fatalf("reading acceptance byte: %v", err)
	}
	if ack[0] != 0 {
		t.Fatalf("ack = %d, want 0", ack[0])
	}

	select {
	case err := <-handlerDone:
		if err == nil {
			t.Fatal("Stdin.Read returned nil error; want an idle timeout once the client went silent")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle session was never dropped (handler still blocked reading Stdin)")
	}

	// the server closes the connection once its Handler returns; a
	// client read now must reach a clean EOF, not hang until this
	// test's own deadline
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadAll(c); err != nil {
		t.Fatalf("connection not closed by the server after the idle timeout: %v", err)
	}
}

// TestIdleTimeoutUnsetPreservesUnboundedWait confirms the zero value
// changes nothing: a handler that blocks on Stdin for longer than what
// would be a short idle timeout keeps running when IdleTimeout is unset.
func TestIdleTimeoutUnsetPreservesUnboundedWait(t *testing.T) {
	handlerDone := make(chan error, 1)
	addr := startServer(t, &rcmd.Server{
		AllowHighPorts: true,
		Handler: func(req *rcmd.Request) error {
			buf := make([]byte, 1)
			_, err := req.Stdin.Read(buf)
			handlerDone <- err
			return nil
		},
	})

	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "0\x00guest\x00guest\x00exec /bin/sh\x00")

	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	ack := make([]byte, 1)
	if _, err := io.ReadFull(c, ack); err != nil {
		t.Fatalf("reading acceptance byte: %v", err)
	}

	// stay silent well past what TestIdleTimeoutClosesSilentSession uses
	// as its timeout; without IdleTimeout set, the handler must still be
	// waiting
	select {
	case err := <-handlerDone:
		t.Fatalf("handler returned early (err=%v) with no IdleTimeout set", err)
	case <-time.After(300 * time.Millisecond):
	}
	fmt.Fprintf(c, "x")
	if err := <-handlerDone; err != nil {
		t.Fatalf("Stdin.Read error = %v, want nil once data arrived", err)
	}
}

func TestMalformedRequestClosed(t *testing.T) {
	addr := startServer(t, &rcmd.Server{Handler: echoHandler, AllowHighPorts: true})
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// no NUL terminators at all, oversized
	c.Write([]byte(strings.Repeat("A", 5000)))
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, _ := io.ReadAll(c)
	if len(b) > 0 && b[0] == 0 {
		t.Fatal("malformed request accepted")
	}
}
