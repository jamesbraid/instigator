// Package rcmd serves the BSD rcmd protocol, the wire format behind rsh:
// the client connects to TCP port 514 from a reserved port, sends an
// ASCII stderr port number and three NUL-terminated strings (remote
// user, local user, command), and the server answers with a single zero
// byte before streaming the command's stdout on the connection. When
// the client named a stderr port, the server dials back to it for the
// stderr stream.
//
// The package carries no command semantics: the caller's Handler decides
// what a command means. IRIX inst is the intended client, so instigator
// wires a constrained command table - never a shell.
package rcmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/jamesbraid/instigator/internal/logging"
)

// maxRequest bounds the request head (port + three strings). Real inst
// commands are short; anything larger is hostile.
const maxRequest = 4096

// Request is one accepted rcmd exchange.
type Request struct {
	RemoteUser string
	LocalUser  string
	Command    string
	Addr       netip.Addr

	// Stdout is the primary connection. Stderr is the dial-back
	// connection when the client asked for one, io.Discard otherwise.
	Stdout io.Writer
	Stderr io.Writer

	// Stdin is the rest of the primary connection after the command,
	// which a shell command (rsh exec /bin/sh) reads its command stream
	// from. It carries only what the client sends next, unbounded.
	Stdin io.Reader
}

// Handler executes one command. A returned error is reported to the
// client through the protocol's error byte and message.
type Handler func(req *Request) error

// Server accepts rcmd connections.
type Server struct {
	Handler Handler

	// AllowIP filters clients; nil allows everyone.
	AllowIP func(netip.Addr) bool

	// AllowHighPorts disables the reserved-port check on the client's
	// source port. The check stays on in production - the miniroot's
	// rcmd client uses reserved ports - and off in unprivileged tests.
	AllowHighPorts bool

	// Logger, when set, receives leveled log output: DEBUG for every
	// connection and the parsed request/stderr dial-back detail, INFO
	// for each command run, WARN for a connection refused or malformed,
	// ERROR for a command the Handler refused or failed - a command
	// this side couldn't or wouldn't serve must show up here, not just
	// on the client's own stderr. A nil Logger is silent.
	Logger *logging.Logger

	// IdleTimeout bounds the time between reads on a connection: every
	// read - the request-head fields and, for a shell command, every
	// line a Handler pulls from Request.Stdin - resets it. Zero means
	// no timeout, unbounded wait, the historical behavior. Without this,
	// a shell session (rsh exec /bin/sh) blocks its handler goroutine
	// reading Stdin for as long as the client holds the connection open
	// and stays silent - stuck, hostile, or simply gone.
	IdleTimeout time.Duration

	// RejectHook, when set, is called when a connection is refused before
	// any command runs - a non-reserved source port or a client outside the
	// filter - with the client address and a short reason. It lets a caller
	// record a refusal the Handler never sees.
	RejectHook func(addr netip.Addr, reason string)

	mu     sync.Mutex
	conns  map[net.Conn]struct{} // active connections, for Shutdown to close
	closed bool                  // set by Shutdown; refuse to track new work after
	wg     sync.WaitGroup        // active handler goroutines
}

// Serve accepts connections on l (conventionally bound to :514) until l
// is closed.
func (s *Server) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			c.Close()
			continue
		}
		if s.conns == nil {
			s.conns = make(map[net.Conn]struct{})
		}
		s.conns[c] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
			}()
			s.handle(c)
		}()
	}
}

// Shutdown closes every active connection - unblocking any session sitting
// on a Stdin read - and waits up to timeout for the handler goroutines to
// finish, so their final events are emitted before the caller finalizes.
// It returns false if the timeout elapsed with handlers still running.
func (s *Server) Shutdown(timeout time.Duration) bool {
	s.mu.Lock()
	s.closed = true
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func refuse(c net.Conn, msg string) {
	c.Write(append([]byte{1}, msg+"\n"...))
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	tcp, ok := c.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return
	}
	if s.Logger.Enabled(logging.LevelDebug) {
		s.Logger.Debugf("rcmd: connection from %s", tcp)
	}
	ip, _ := netip.AddrFromSlice(tcp.IP)
	ip = ip.Unmap()
	if s.AllowIP != nil && !s.AllowIP(ip) {
		s.Logger.Warnf("rcmd: %s: denied by client filter", tcp)
		if s.RejectHook != nil {
			s.RejectHook(ip, "filter")
		}
		refuse(c, "permission denied")
		return
	}
	if !s.AllowHighPorts && (tcp.Port >= 1024 || tcp.Port < 512) {
		s.Logger.Warnf("rcmd: %s: source port not reserved", tcp)
		if s.RejectHook != nil {
			s.RejectHook(ip, "reserved_port")
		}
		refuse(c, "permission denied")
		return
	}

	// bufio over the raw connection, not a LimitReader: a shell command
	// streams its input here after the request head, so stdin must stay
	// unbounded. Each field is still capped so the head cannot run away.
	var src io.Reader = c
	if s.IdleTimeout > 0 {
		src = &idleReader{c: c, timeout: s.IdleTimeout}
	}
	r := bufio.NewReader(src)
	readField := func() (string, error) {
		var b []byte
		for len(b) <= maxRequest {
			ch, err := r.ReadByte()
			if err != nil {
				return "", err
			}
			if ch == 0 {
				return string(b), nil
			}
			b = append(b, ch)
		}
		return "", fmt.Errorf("field exceeds %d bytes", maxRequest)
	}

	// Field 1 is the stderr port. The rcmd protocol requires the server to
	// connect BACK to that port before the client sends the user and
	// command fields - a real client (IRIX inst) blocks waiting for the
	// stderr callback, so reading all four fields up front deadlocks it.
	// Read the port, dial the stderr channel, then read the rest.
	portField, err := readField()
	if err != nil {
		s.Logger.Warnf("rcmd: %s: short request (stderr port): %v", tcp, err)
		refuse(c, "malformed request")
		return
	}
	errPort, err := strconv.Atoi(portField)
	if err != nil || errPort < 0 || errPort > 65535 {
		refuse(c, "bad stderr port")
		return
	}

	// Without a stderr channel, stderr shares the primary connection,
	// as rshd's dup2 onto the socket does.
	req := &Request{Addr: ip, Stdout: c, Stderr: c}
	if errPort != 0 {
		ec, err := s.dialStderr(tcp.IP, errPort)
		if err != nil {
			s.Logger.Errorf("rcmd: %s: stderr dial-back to %d: %v", tcp, errPort, err)
			refuse(c, "cannot connect stderr")
			return
		}
		defer ec.Close()
		req.Stderr = ec
	}

	remuser, err1 := readField()
	locuser, err2 := readField()
	command, err3 := readField()
	if err1 != nil || err2 != nil || err3 != nil {
		s.Logger.Warnf("rcmd: %s: short request (fields): %v %v %v", tcp, err1, err2, err3)
		refuse(c, "malformed request")
		return
	}
	req.RemoteUser, req.LocalUser, req.Command = remuser, locuser, command
	req.Stdin = r // the rest of the connection, for a shell command
	if s.Logger.Enabled(logging.LevelDebug) {
		s.Logger.Debugf("rcmd: %s: stderr-port=%d remuser=%q locuser=%q command=%q",
			tcp, errPort, remuser, locuser, command)
	}

	s.Logger.Infof("rcmd: %s: user=%s/%s command=%q", tcp, req.RemoteUser, req.LocalUser, req.Command)
	if s.Handler == nil {
		refuse(c, "no handler")
		return
	}
	if _, err := c.Write([]byte{0}); err != nil {
		return
	}
	if err := s.Handler(req); err != nil {
		fmt.Fprintf(req.Stderr, "%v\n", err)
		s.Logger.Errorf("rcmd: %s: %q: %v", tcp, req.Command, err)
	}
}

// dialStderr connects back to the client's stderr port, preferring a
// reserved local source port: classic rcmd clients verify the stderr
// connection originates below 1024. Binding there needs privilege, so
// an ephemeral port is the fallback.
//
// Timed and counted under Verbose because a burst of concurrent
// dial-backs - inst opens several shell sessions together to read more
// than one distribution's product descriptions - was a suspect for
// dropped connections. Measured (rcmd_test.go's
// TestConcurrentStderrDialbackDoesNotDegrade): at the concurrency a real
// burst actually reaches (a handful of sessions inside the same
// second), the scan resolves in well under a millisecond; even stressed
// to 100 simultaneous dial-backs, far past anything observed in the
// field, it stays under 100ms with zero failures. That doesn't match a
// multi-second "Lost connection" - this logging exists so a real
// recurrence, captured with -v, can either confirm or rule this out
// with actual numbers instead of another guess.
func (s *Server) dialStderr(ip net.IP, port int) (net.Conn, error) {
	dst := &net.TCPAddr{IP: ip, Port: port}
	t0 := time.Now()
	attempts := 0
	for local := 1023; local >= 512; local-- {
		attempts++
		d := net.Dialer{LocalAddr: &net.TCPAddr{Port: local}}
		c, err := d.Dial("tcp", dst.String())
		if err == nil {
			if s.Logger.Enabled(logging.LevelDebug) {
				s.Logger.Debugf("rcmd: stderr dial-back to %s: local=%d attempts=%d took=%v",
					dst, local, attempts, time.Since(t0))
			}
			return c, nil
		}
		if isAddrInUse(err) || isPermission(err) {
			if isPermission(err) {
				break // unprivileged: stop scanning, fall through
			}
			continue
		}
		return nil, err
	}
	if s.Logger.Enabled(logging.LevelDebug) {
		s.Logger.Debugf("rcmd: stderr dial-back to %s: reserved range exhausted, attempts=%d took=%v, falling back to ephemeral",
			dst, attempts, time.Since(t0))
	}
	return net.Dial("tcp", dst.String())
}

// idleReader resets c's read deadline before every Read, so timeout
// bounds the gap between reads rather than the connection's whole
// lifetime - a client that keeps sending, however slowly, stays
// connected; one that goes silent gets the next Read failed out from
// under it once timeout has passed with nothing arriving.
type idleReader struct {
	c       net.Conn
	timeout time.Duration
}

func (r *idleReader) Read(p []byte) (int, error) {
	if err := r.c.SetReadDeadline(time.Now().Add(r.timeout)); err != nil {
		return 0, err
	}
	return r.c.Read(p)
}

func isAddrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }

func isPermission(err error) bool {
	return errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM)
}
