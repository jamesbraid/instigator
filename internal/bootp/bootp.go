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

	"github.com/jamesbraid/instigator/internal/capture"
	"github.com/jamesbraid/instigator/internal/logging"
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

	// Logger, when set, receives leveled log output: DEBUG for every
	// datagram decoded, INFO for each request answered or ignored,
	// ERROR for a reply that failed to send. A nil Logger is silent.
	Logger *logging.Logger

	// Recorder, when set, receives a bootp_reply record for each request
	// answered or ignored. A nil Recorder is a no-op.
	Recorder *capture.Recorder
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
		if s.Logger.Enabled(logging.LevelDebug) {
			s.Logger.Debugf("bootp: recv %d bytes from %s\n%s", n, src, decodeReq(buf[:n]))
		}
		if n < packetLen {
			if s.Logger.Enabled(logging.LevelDebug) {
				s.Logger.Debugf("bootp: %s: runt packet (%d < %d), ignoring", src, n, packetLen)
			}
			continue
		}
		mac, file, ok := parseRequest(buf[:n])
		if !ok {
			// a non-request datagram is just noise
			continue
		}
		client := s.lookupClient(mac)
		if client == nil {
			// a well-formed request from a MAC we don't serve is a
			// meaningful "ignored"
			s.Logger.Infof("bootp: ignoring request from %s (not configured)", mac)
			if s.Recorder != nil {
				s.Recorder.BootpReply("", mac.String(), file, "", "ignored")
			}
			continue
		}
		s.Logger.Infof("bootp: answering %s (%s): ip=%s file=%q", mac, client.Name, client.IP, file)
		reply := s.buildReply(buf[:n], client)

		dst := s.ReplyAddr
		how := "broadcast"
		if dst == nil {
			// RFC 1542: a client that already owns an address (ciaddr set,
			// e.g. the PROM's netaddr) listens for a unicast reply to that
			// address, not a broadcast. Reply straight back to the request's
			// source when it has a real IP; broadcast only when it doesn't.
			u, _ := src.(*net.UDPAddr)
			switch {
			case u != nil && u.IP != nil && !u.IP.IsUnspecified():
				dst, how = u, "unicast to sender"
			case u != nil:
				dst = &net.UDPAddr{IP: net.IPv4bcast, Port: u.Port}
			default:
				dst = &net.UDPAddr{IP: net.IPv4bcast, Port: 68}
			}
		}
		if s.Logger.Enabled(logging.LevelDebug) {
			s.Logger.Debugf("bootp: send %d bytes to %s (%s)\n%s", len(reply), dst, how, decodeReq(reply))
		}
		result := "answered"
		if _, err := pc.WriteTo(reply, dst); err != nil {
			s.Logger.Errorf("bootp: reply to %s: %v", dst, err)
			result = "error"
		}
		if s.Recorder != nil {
			s.Recorder.BootpReply(client.Name, mac.String(), file, client.IP.String(), result)
		}
	}
}

// parseRequest validates a BOOTP packet and extracts the client MAC and
// the boot file it asked for. ok is false for anything that is not a
// well-formed ethernet BOOTREQUEST, so both handle and the Serve recorder
// classify a datagram the same way.
func parseRequest(req []byte) (mac net.HardwareAddr, file string, ok bool) {
	if len(req) < packetLen || req[0] != 1 || req[1] != 1 || req[2] != 6 {
		return nil, "", false
	}
	return net.HardwareAddr(req[28:34]), string(bytes.TrimRight(req[108:236], "\x00")), true
}

// lookupClient returns the configured client with the given MAC, or nil.
func (s *Server) lookupClient(mac net.HardwareAddr) *Client {
	for i := range s.Clients {
		if bytes.Equal(s.Clients[i].MAC, mac) {
			return &s.Clients[i]
		}
	}
	return nil
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

// buildReply turns a validated BOOTREQUEST into the BOOTREPLY for client.
func (s *Server) buildReply(req []byte, client *Client) []byte {
	rep := make([]byte, packetLen)
	copy(rep, req[:packetLen])
	rep[0] = 2                             // BOOTREPLY
	rep[3] = 0                             // hops
	copy(rep[16:20], client.IP.AsSlice())  // yiaddr
	copy(rep[20:24], s.ServerIP.AsSlice()) // siaddr
	clear(rep[44:108])                     // sname
	// file: echo the request's path (the PROM chose it)
	// vendor area: RFC 1048 magic, subnet mask when configured, end
	vend := rep[236:packetLen]
	clear(vend)
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
		serr = enableBroadcast(fd)
	}); err != nil {
		return err
	}
	return serr
}
