package qemunet

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
)

// DefaultServerMAC is Instigator's stable Ethernet address on a private
// segment. It uses SGI's 08:00:69 OUI with a low byte distinct from any
// configured guest.
var DefaultServerMAC = net.HardwareAddr{0x08, 0x00, 0x69, 0x00, 0x00, 0x02}

// Listener owns the stream endpoint a QEMU machine connects to, attaching
// its virtual Ethernet segment to Instigator. Instigator creates the endpoint
// before the machine connects. The serving command exits if it disconnects.
type Listener struct {
	ln  net.Listener
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
	return &Listener{ln: ln, cfg: cfg}, nil
}

// ListenTCP accepts one QEMU stream connection on a loopback address.
// An explicit port keeps the address stable across separate installer and
// machine commands. Remote interfaces and ephemeral ports are refused.
func ListenTCP(address string, cfg Config) (*Listener, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("qemunet: invalid TCP address %q: %w", address, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("qemunet: TCP address must use a loopback IP: %q", address)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("qemunet: TCP address needs a port from 1 to 65535: %q", address)
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("qemunet: listen %s: %w", address, err)
	}
	return &Listener{ln: ln, cfg: cfg}, nil
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

// Close stops listening and removes a Unix socket file, if present.
func (l *Listener) Close() error { return l.ln.Close() }
