// Package bootp answers RFC 951 BOOTP requests for explicitly configured
// clients and nobody else. SGI PROMs speak classic BOOTP, not DHCP: the
// PROM broadcasts a BOOTREQUEST carrying the boot path it wants in the
// file field, and expects a reply naming its address and the server's.
// Replies are broadcast by default because the client owns no IP address
// yet, so it cannot be reached by unicast without ARP games.
package bootp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

const packetLen = 300

// Client is one machine the server answers.
type Client struct {
	Name string
	MAC  net.HardwareAddr
	IP   netip.Addr
}

// Server answers BOOTP requests.
type Server struct {
	// ServerIP is advertised as siaddr, the address the PROM tftps from.
	ServerIP netip.Addr

	// Netmask, when valid, is offered in the RFC 1048 vendor area.
	Netmask netip.Prefix

	// Clients lists the MACs answered; everything else is ignored.
	Clients []Client

	// ReplyAddr overrides the destination of replies. Nil means the
	// limited broadcast 255.255.255.255:68.
	ReplyAddr net.Addr

	// Logf, when set, receives one line per request.
	Logf func(format string, args ...any)

	// Verbose logs every datagram received and sent, decoded.
	Verbose bool
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// Serve answers requests arriving on pc (conventionally bound to :67)
// until pc is closed. The socket needs SO_BROADCAST when replies go to
// the default broadcast address; ListenBroadcast provides that.
//
// The reply goes to the broadcast address at the request's own source
// port, not the standard 68. SGI ARCS PROMs send their BOOTREQUEST from
// an ephemeral port and listen for the reply there; a reply hardwired to
// port 68 never reaches them and the PROM retries forever. Broadcasting
// to the source port serves both the RFC client (source 68) and the SGI
// PROM (ephemeral source).
func (s *Server) Serve(pc net.PacketConn) error {
	buf := make([]byte, 1500)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if s.Verbose {
			s.logf("bootp: recv %d bytes from %s\n%s", n, src, decodeReq(buf[:n]))
		}
		if n < packetLen {
			if s.Verbose {
				s.logf("bootp: %s: runt packet (%d < %d), ignoring", src, n, packetLen)
			}
			continue
		}
		reply := s.handle(buf[:n])
		if reply == nil {
			continue
		}
		dst := s.ReplyAddr
		how := "broadcast"
		if dst == nil {
			// RFC 1542: a client that already owns an address (ciaddr set,
			// e.g. the PROM's netaddr) listens for a unicast reply to that
			// address, not a broadcast. Reply straight back to the request's
			// source when it has a real IP; broadcast only when it doesn't.
			if u, ok := src.(*net.UDPAddr); ok && u.IP != nil && !u.IP.IsUnspecified() {
				dst = u
				how = "unicast to sender"
			} else {
				port := 68
				if u, ok := src.(*net.UDPAddr); ok {
					port = u.Port
				}
				dst = &net.UDPAddr{IP: net.IPv4bcast, Port: port}
			}
		}
		if s.Verbose {
			s.logf("bootp: send %d bytes to %s (%s)\n%s", len(reply), dst, how, decodeReq(reply))
		}
		if _, err := pc.WriteTo(reply, dst); err != nil {
			s.logf("bootp: reply to %s: %v", dst, err)
		}
	}
}

// decodeReq renders the header fields of a BOOTP packet for verbose logs.
func decodeReq(p []byte) string {
	if len(p) < 236 {
		return fmt.Sprintf("  (short packet, %d bytes)", len(p))
	}
	ip := func(o int) string { return net.IP(p[o : o+4]).String() }
	mac := net.HardwareAddr(p[28:34])
	file := string(bytes.TrimRight(p[108:236], "\x00"))
	flags := binary.BigEndian.Uint16(p[10:12])
	return fmt.Sprintf("  op=%d htype=%d hlen=%d hops=%d xid=%#x flags=%#04x\n"+
		"  ciaddr=%s yiaddr=%s siaddr=%s giaddr=%s\n  chaddr=%s file=%q",
		p[0], p[1], p[2], p[3], binary.BigEndian.Uint32(p[4:8]), flags,
		ip(12), ip(16), ip(20), ip(24), mac, file)
}

// handle builds the reply for one packet, or nil when the packet is not
// a request from a configured client.
func (s *Server) handle(req []byte) []byte {
	if len(req) < packetLen {
		return nil
	}
	if req[0] != 1 { // BOOTREQUEST
		return nil
	}
	if req[1] != 1 || req[2] != 6 { // ethernet, 6-byte MAC
		return nil
	}
	mac := net.HardwareAddr(req[28:34])
	var client *Client
	for i := range s.Clients {
		if bytes.Equal(s.Clients[i].MAC, mac) {
			client = &s.Clients[i]
			break
		}
	}
	if client == nil {
		s.logf("bootp: ignoring request from %s (not configured)", mac)
		return nil
	}
	file := string(bytes.TrimRight(req[108:236], "\x00"))
	s.logf("bootp: answering %s (%s): ip=%s file=%q", mac, client.Name, client.IP, file)

	rep := make([]byte, packetLen)
	copy(rep, req[:packetLen])
	rep[0] = 2                             // BOOTREPLY
	rep[3] = 0                             // hops
	copy(rep[16:20], client.IP.AsSlice())  // yiaddr
	copy(rep[20:24], s.ServerIP.AsSlice()) // siaddr
	for i := 44; i < 108; i++ {            // sname: clear
		rep[i] = 0
	}
	// file: echo the request's path (the PROM chose it)
	// vendor area: RFC 1048 magic, subnet mask when configured, end
	vend := rep[236:packetLen]
	for i := range vend {
		vend[i] = 0
	}
	copy(vend[0:4], []byte{99, 130, 83, 99})
	w := 4
	if s.Netmask.IsValid() {
		mask := net.CIDRMask(s.Netmask.Bits(), 32)
		vend[w] = 1
		vend[w+1] = 4
		copy(vend[w+2:w+6], mask)
		w += 6
	}
	vend[w] = 255
	return rep
}

// ListenBroadcast opens the server socket on addr (":67" in production)
// with SO_BROADCAST set, so replies can go to 255.255.255.255.
func ListenBroadcast(addr string) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: setBroadcast}
	return lc.ListenPacket(context.Background(), "udp4", addr)
}

func setBroadcast(network, address string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	}); err != nil {
		return err
	}
	return serr
}
