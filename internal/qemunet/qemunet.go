package qemunet

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	nicID = 1

	// maxFrame bounds a single framed Ethernet frame read off the stream. It
	// caps a corrupt or hostile length prefix; ordinary frames are one MTU.
	maxFrame = 65535

	// channelQueue is how many outbound packets the link endpoint buffers
	// before writes block on the stream.
	channelQueue = 512
)

// Config describes the private segment a Network serves on.
type Config struct {
	// ServerIP is Instigator's address on the segment (10.98.0.2). It must
	// be IPv4; the private network is IPv4 only.
	ServerIP netip.Addr
	// PrefixLen is the segment's mask length (24 for a /24).
	PrefixLen int
	// MAC is Instigator's stable Ethernet address on the segment.
	MAC net.HardwareAddr
	// MTU is the link MTU; zero means 1500.
	MTU uint32
}

// Network is an unprivileged private Ethernet segment backed by a user-space
// IPv4 stack, exchanging frames with a QEMU machine over a stream. It hands
// Instigator's services the sockets they run on. It is safe for the services
// to use concurrently; it is not itself reusable after Close.
type Network struct {
	stack *stack.Stack
	ch    *channel.Endpoint
	conn  io.ReadWriteCloser
	ip    tcpip.Address

	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	done      chan struct{}
	stopOnce  sync.Once
	closeOnce sync.Once
	errMu     sync.Mutex
	stopErr   error
	closeErr  error
}

// New builds a private-network stack from cfg and attaches it to conn, which
// carries Ethernet frames in QEMU's stream framing (a four-byte big-endian
// length prefix per frame). New owns conn and closes it in Close. The two
// pump goroutines start immediately, so a frame that arrives before any
// service is listening is delivered to the stack and simply has no endpoint.
func New(conn io.ReadWriteCloser, cfg Config) (*Network, error) {
	if !cfg.ServerIP.Is4() {
		return nil, fmt.Errorf("qemunet: server IP %s is not IPv4", cfg.ServerIP)
	}
	if len(cfg.MAC) != 6 {
		return nil, fmt.Errorf("qemunet: MAC %q is not 6 bytes", cfg.MAC)
	}
	if cfg.PrefixLen < 0 || cfg.PrefixLen > 32 {
		return nil, fmt.Errorf("qemunet: prefix length %d out of range", cfg.PrefixLen)
	}
	mtu := cfg.MTU
	if mtu == 0 {
		mtu = 1500
	}

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{arp.NewProtocol, ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{udp.NewProtocol, tcp.NewProtocol},
	})
	ch := channel.New(channelQueue, mtu, tcpip.LinkAddress(cfg.MAC))
	if err := s.CreateNIC(nicID, ethernet.New(ch)); err != nil {
		s.Close()
		return nil, fmt.Errorf("qemunet: create NIC: %s", err)
	}
	addr := tcpip.AddrFrom4(cfg.ServerIP.As4())
	pa := tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr, PrefixLen: cfg.PrefixLen},
	}
	if err := s.AddProtocolAddress(nicID, pa, stack.AddressProperties{}); err != nil {
		s.Close()
		return nil, fmt.Errorf("qemunet: add address %s: %s", cfg.ServerIP, err)
	}
	// A default route out the single NIC covers the segment (for ARP-resolved
	// unicast) and the limited broadcast a BOOTP reply uses.
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})

	n := &Network{stack: s, ch: ch, conn: conn, ip: addr, done: make(chan struct{})}
	n.ctx, n.cancel = context.WithCancel(context.Background())
	n.wg.Add(2)
	go n.readLoop()
	go n.writeLoop()
	go func() {
		n.wg.Wait()
		close(n.done)
	}()
	return n, nil
}

// readLoop moves frames from the stream into the stack.
func (n *Network) readLoop() {
	defer n.wg.Done()
	r := bufio.NewReader(n.conn)
	for {
		frame, err := readFrame(r, maxFrame)
		if err != nil {
			n.stop(fmt.Errorf("read frame: %w", err))
			return
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(frame),
		})
		// ethernet.Endpoint re-parses the frame's own ethertype; the protocol
		// number here is ignored.
		n.ch.InjectInbound(header.IPv4ProtocolNumber, pkt)
		pkt.DecRef()
	}
}

// writeLoop moves frames from the stack onto the stream.
func (n *Network) writeLoop() {
	defer n.wg.Done()
	for {
		pkt := n.ch.ReadContext(n.ctx)
		if pkt == nil {
			return // Close cancelled the context
		}
		frame := stack.PayloadSince(pkt.LinkHeader())
		pkt.DecRef()
		if frame == nil {
			continue
		}
		err := writeFrame(n.conn, frame.AsSlice())
		frame.Release()
		if err != nil {
			n.stop(fmt.Errorf("write frame: %w", err))
			return
		}
	}
}

func (n *Network) stop(err error) {
	n.stopOnce.Do(func() {
		n.errMu.Lock()
		n.stopErr = err
		n.errMu.Unlock()
		n.cancel()
		n.closeErr = n.conn.Close()
	})
}

// Done closes when the frame pumps have stopped. A peer disconnect or a
// malformed frame also closes the stream and stops the other pump.
func (n *Network) Done() <-chan struct{} { return n.done }

// Err reports why the pumps stopped. It is nil after an explicit Close.
func (n *Network) Err() error {
	n.errMu.Lock()
	defer n.errMu.Unlock()
	return n.stopErr
}

// ListenPacket binds a broadcast-capable UDP conn on the segment to port
// (0 = ephemeral). bootp uses port 67, tftp port 69 and each data transfer.
func (n *Network) ListenPacket(port int) (net.PacketConn, error) {
	var wq waiter.Queue
	ep, err := n.stack.NewEndpoint(udp.ProtocolNumber, ipv4.ProtocolNumber, &wq)
	if err != nil {
		return nil, fmt.Errorf("qemunet: udp endpoint: %s", err)
	}
	ep.SocketOptions().SetBroadcast(true)
	if err := ep.Bind(tcpip.FullAddress{NIC: nicID, Port: uint16(port)}); err != nil {
		ep.Close()
		return nil, fmt.Errorf("qemunet: bind udp :%d: %s", port, err)
	}
	return gonet.NewUDPConn(&wq, ep), nil
}

// Listen binds a TCP listener on the segment to port (rsh uses 514).
func (n *Network) Listen(port int) (net.Listener, error) {
	ln, err := gonet.ListenTCP(n.stack, tcpip.FullAddress{NIC: nicID, Addr: n.ip, Port: uint16(port)}, ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("qemunet: listen tcp :%d: %w", port, err)
	}
	return ln, nil
}

// DialStderr opens the rsh stderr callback to the guest, from a reserved
// source port (512-1023) as classic rcmd requires, falling back to an
// ephemeral port. On this segment no port is privileged, so the reserved
// bind succeeds without elevation.
func (n *Network) DialStderr(ip net.IP, port int) (net.Conn, error) {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return nil, fmt.Errorf("qemunet: bad callback address %v", ip)
	}
	a = a.Unmap()
	if !a.Is4() {
		return nil, fmt.Errorf("qemunet: callback address %s is not IPv4", a)
	}
	dst := tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4(a.As4()), Port: uint16(port)}
	for local := 1023; local >= 512; local-- {
		c, err := gonet.DialTCPWithBind(n.ctx, n.stack,
			tcpip.FullAddress{NIC: nicID, Addr: n.ip, Port: uint16(local)}, dst, ipv4.ProtocolNumber)
		if err == nil {
			return c, nil
		}
	}
	c, err := gonet.DialContextTCP(n.ctx, n.stack, dst, ipv4.ProtocolNumber)
	if err != nil {
		return nil, fmt.Errorf("qemunet: dial callback %s:%d: %w", ip, port, err)
	}
	return c, nil
}

// Close stops the frame pumps, closes the stream, and tears down the stack.
func (n *Network) Close() error {
	n.stop(nil)
	<-n.done
	n.closeOnce.Do(func() {
		n.stack.Close()
		n.ch.Close()
	})
	return n.closeErr
}
