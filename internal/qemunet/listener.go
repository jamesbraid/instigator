package qemunet

import (
	"fmt"
	"net"
)

// DefaultServerMAC is Instigator's stable Ethernet address on a private
// segment. It uses SGI's 08:00:69 OUI with a low byte distinct from any
// configured guest.
var DefaultServerMAC = net.HardwareAddr{0x08, 0x00, 0x69, 0x00, 0x00, 0x02}

// Listener owns the Unix stream socket a QEMU machine connects to, attaching
// its virtual Ethernet segment to Instigator. Per the private-network
// contract, Instigator creates and owns this socket; the machine connects to
// it, and disconnecting or reconnecting the machine never changes it.
type Listener struct {
	ln  *net.UnixListener
	cfg Config
}

// Listen creates and owns the Unix socket at path. It fails now - before any
// guest could boot - if the path is taken or the directory is unwritable, so
// a bad endpoint is reported at startup rather than mid-install.
func Listen(path string, cfg Config) (*Listener, error) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("qemunet: listen %s: %w", path, err)
	}
	return &Listener{ln: ln.(*net.UnixListener), cfg: cfg}, nil
}

// Accept waits for one machine to connect and returns the private Network
// attached to it. The Network owns the accepted connection and closes it in
// its own Close.
func (l *Listener) Accept() (*Network, error) {
	c, err := l.ln.Accept()
	if err != nil {
		return nil, err
	}
	n, err := New(c, l.cfg)
	if err != nil {
		c.Close()
		return nil, err
	}
	return n, nil
}

// Addr returns the socket's address.
func (l *Listener) Addr() net.Addr { return l.ln.Addr() }

// Close stops listening and removes the socket file.
func (l *Listener) Close() error { return l.ln.Close() }
