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

	r := bufio.NewReaderSize(io.LimitReader(c, maxRequest), maxRequest)
	fields := make([]string, 4)
	for i := range fields {
		f, err := r.ReadString(0)
		if err != nil {
			s.logf("rcmd: %s: short request: %v", tcp, err)
			refuse(c, "malformed request")
			return
		}
		fields[i] = f[:len(f)-1]
	}
	errPort, err := strconv.Atoi(fields[0])
	if err != nil || errPort < 0 || errPort > 65535 {
		refuse(c, "bad stderr port")
		return
	}
	if s.Verbose {
		s.logf("rcmd: %s: stderr-port=%d remuser=%q locuser=%q command=%q",
			tcp, errPort, fields[1], fields[2], fields[3])
	}

	// Without a stderr channel, stderr shares the primary connection,
	// as rshd's dup2 onto the socket does.
	req := &Request{
		RemoteUser: fields[1],
		LocalUser:  fields[2],
		Command:    fields[3],
		Addr:       ip,
		Stdout:     c,
		Stderr:     c,
	}
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

func isAddrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }

func isPermission(err error) bool {
	return errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM)
}
