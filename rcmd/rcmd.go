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
	"syscall"
	"time"
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

	// Logf, when set, receives one line per connection and command.
	Logf func(format string, args ...any)

	// Verbose logs every connection, the parsed request, and the
	// stderr dial-back.
	Verbose bool

	// IdleTimeout bounds the time between reads on a connection: every
	// read - the request-head fields and, for a shell command, every
	// line a Handler pulls from Request.Stdin - resets it. Zero means
	// no timeout, unbounded wait, the historical behavior. Without this,
	// a shell session (rsh exec /bin/sh) blocks its handler goroutine
	// reading Stdin for as long as the client holds the connection open
	// and stays silent - stuck, hostile, or simply gone.
	IdleTimeout time.Duration
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
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
		go s.handle(c)
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
	if s.Verbose {
		s.logf("rcmd: connection from %s", tcp)
	}
	ip, _ := netip.AddrFromSlice(tcp.IP)
	ip = ip.Unmap()
	if s.AllowIP != nil && !s.AllowIP(ip) {
		s.logf("rcmd: %s: denied by client filter", tcp)
		refuse(c, "permission denied")
		return
	}
	if !s.AllowHighPorts && (tcp.Port >= 1024 || tcp.Port < 512) {
		s.logf("rcmd: %s: source port not reserved", tcp)
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
		s.logf("rcmd: %s: short request (stderr port): %v", tcp, err)
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
			s.logf("rcmd: %s: stderr dial-back to %d: %v", tcp, errPort, err)
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
		s.logf("rcmd: %s: short request (fields): %v %v %v", tcp, err1, err2, err3)
		refuse(c, "malformed request")
		return
	}
	req.RemoteUser, req.LocalUser, req.Command = remuser, locuser, command
	req.Stdin = r // the rest of the connection, for a shell command
	if s.Verbose {
		s.logf("rcmd: %s: stderr-port=%d remuser=%q locuser=%q command=%q",
			tcp, errPort, remuser, locuser, command)
	}

	s.logf("rcmd: %s: user=%s/%s command=%q", tcp, req.RemoteUser, req.LocalUser, req.Command)
	if s.Handler == nil {
		refuse(c, "no handler")
		return
	}
	if _, err := c.Write([]byte{0}); err != nil {
		return
	}
	if err := s.Handler(req); err != nil {
		fmt.Fprintf(req.Stderr, "%v\n", err)
		s.logf("rcmd: %s: %q: %v", tcp, req.Command, err)
	}
}

// dialStderr connects back to the client's stderr port, preferring a
// reserved local source port: classic rcmd clients verify the stderr
// connection originates below 1024. Binding there needs privilege, so
// an ephemeral port is the fallback.
func (s *Server) dialStderr(ip net.IP, port int) (net.Conn, error) {
	dst := &net.TCPAddr{IP: ip, Port: port}
	for local := 1023; local >= 512; local-- {
		d := net.Dialer{LocalAddr: &net.TCPAddr{Port: local}}
		c, err := d.Dial("tcp", dst.String())
		if err == nil {
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
