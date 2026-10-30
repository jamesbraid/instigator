package bootp

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
)

// DHCP message types (RFC 2132 option 53).
const (
	dhcpDiscover = 1
	dhcpOffer    = 2
	dhcpRequest  = 3
	dhcpAck      = 5
	dhcpNak      = 6
)

// LeaseSeconds is the lease every OFFER and ACK carries: one year. The
// private network has one fixed address per machine, so a lease only has to
// outlast the guest; a finite value avoids clients that mishandle the
// "infinite" 0xffffffff.
const LeaseSeconds = 365 * 24 * 60 * 60

var dhcpCookie = []byte{99, 130, 83, 99}

// dhcpMsg is the part of a client's DHCP message the server acts on.
type dhcpMsg struct {
	typ       byte
	mac       net.HardwareAddr
	xid       []byte
	flags     []byte
	ciaddr    netip.Addr
	giaddr    []byte
	requested netip.Addr // option 50
	serverID  netip.Addr // option 54
	chaddr    []byte     // all 16 bytes, echoed in replies
}

// parseDHCP reads an Ethernet BOOTREQUEST that carries the magic cookie and
// a message type option. Anything else, a truncated option included, is not
// DHCP and returns false.
func parseDHCP(req []byte) (dhcpMsg, bool) {
	var m dhcpMsg
	if len(req) < 241 || req[0] != 1 || req[1] != 1 || req[2] != 6 || !bytes.Equal(req[236:240], dhcpCookie) {
		return m, false
	}
	m.mac = net.HardwareAddr(req[28:34])
	m.xid = req[4:8]
	m.flags = req[10:12]
	m.ciaddr = netip.AddrFrom4([4]byte(req[12:16]))
	m.giaddr = req[24:28]
	m.chaddr = req[28:44]
	opts := req[240:]
	for len(opts) > 0 {
		switch opts[0] {
		case 0:
			opts = opts[1:]
			continue
		case 255:
			return m, m.typ != 0
		}
		if len(opts) < 2 || len(opts) < 2+int(opts[1]) {
			return m, false
		}
		v := opts[2 : 2+int(opts[1])]
		switch {
		case opts[0] == 53 && len(v) == 1:
			m.typ = v[0]
		case opts[0] == 50 && len(v) == 4:
			m.requested = netip.AddrFrom4([4]byte(v))
		case opts[0] == 54 && len(v) == 4:
			m.serverID = netip.AddrFrom4([4]byte(v))
		}
		opts = opts[2+len(v):]
	}
	return m, m.typ != 0
}

var dhcpTypeNames = map[byte]string{1: "DISCOVER", 2: "OFFER", 3: "REQUEST", 4: "DECLINE", 5: "ACK", 6: "NAK", 7: "RELEASE", 8: "INFORM"}

func dhcpName(t byte) string {
	if n, ok := dhcpTypeNames[t]; ok {
		return n
	}
	return "unknown"
}

// dhcpAnswer picks the reply type for a message from client c, or 0 for
// none. A REQUEST is answered ACK when it names the client's address (in
// option 50 for selecting and init-reboot, in ciaddr for renewing and
// rebinding) and NAK for any other address. One that selected another server
// is not for us. DECLINE, RELEASE and INFORM draw nothing.
func (s *Server) dhcpAnswer(m dhcpMsg, c *Client) byte {
	switch m.typ {
	case dhcpDiscover:
		return dhcpOffer
	case dhcpRequest:
		if m.serverID.IsValid() && m.serverID != s.serverID() {
			return 0
		}
		want := m.requested
		if !want.IsValid() && !m.ciaddr.IsUnspecified() {
			want = m.ciaddr
		}
		switch {
		case !want.IsValid():
			return 0
		case want == c.IP:
			return dhcpAck
		}
		return dhcpNak
	}
	return 0
}

// serverID is the server identifier replies carry: DHCPServerID, or
// ServerIP when that is unset.
func (s *Server) serverID() netip.Addr {
	if s.DHCPServerID.IsValid() {
		return s.DHCPServerID
	}
	return s.ServerIP
}

// buildDHCP builds the reply of type typ to m for client c. The message is
// deterministic: no random or time-dependent field. It is at least as long
// as a BOOTP reply because some clients drop shorter ones.
func (s *Server) buildDHCP(m dhcpMsg, c *Client, typ byte) []byte {
	rep := make([]byte, 240, packetLen)
	rep[0], rep[1], rep[2] = 2, 1, 6
	copy(rep[4:8], m.xid)
	copy(rep[10:12], m.flags)
	if typ == dhcpAck {
		copy(rep[12:16], m.ciaddr.AsSlice())
	}
	if typ != dhcpNak {
		copy(rep[16:20], c.IP.AsSlice())
		copy(rep[20:24], s.ServerIP.AsSlice())
	}
	copy(rep[24:28], m.giaddr)
	copy(rep[28:44], m.chaddr)
	copy(rep[236:240], dhcpCookie)
	opt := func(code byte, v ...byte) { rep = append(append(rep, code, byte(len(v))), v...) }
	opt(53, typ)
	opt(54, s.serverID().AsSlice()...)
	if typ != dhcpNak {
		opt(51, binary.BigEndian.AppendUint32(nil, LeaseSeconds)...)
		if s.Netmask.IsValid() {
			opt(1, net.CIDRMask(s.Netmask.Bits(), 32)...)
		}
		if c.Hostname != "" {
			opt(12, []byte(c.Hostname)...)
		}
	}
	rep = append(rep, 255)
	for len(rep) < packetLen {
		rep = append(rep, 0)
	}
	return rep
}

// serveDHCP answers one parsed DHCP message through write.
func (s *Server) serveDHCP(m dhcpMsg, write func(reply []byte, dst net.Addr) error) {
	client := s.lookupClient(m.mac)
	if client == nil {
		s.Logger.Infof("dhcp: ignoring %s from %s (not configured)", dhcpName(m.typ), m.mac)
		if s.Recorder != nil {
			s.Recorder.BootpReply("", m.mac.String(), "dhcp "+dhcpName(m.typ), "", "ignored")
		}
		return
	}
	typ := s.dhcpAnswer(m, client)
	if typ == 0 {
		s.Logger.Infof("dhcp: %s from %s (%s): no answer", dhcpName(m.typ), m.mac, client.Name)
		return
	}
	reply := s.buildDHCP(m, client, typ)

	// RFC 2131 4.1: a renewing client that owns its address gets a unicast
	// ACK. Everything else is broadcast, since a client without an address
	// cannot be reached by unicast without ARP, and a NAK always is. Replies
	// go to port 68, where a DHCP client listens.
	dst := s.ReplyAddr
	if dst == nil {
		if typ == dhcpAck && !m.ciaddr.IsUnspecified() {
			dst = &net.UDPAddr{IP: m.ciaddr.AsSlice(), Port: 68}
		} else {
			dst = &net.UDPAddr{IP: net.IPv4bcast, Port: 68}
		}
	}
	s.Logger.Infof("dhcp: %s from %s (%s): sending %s ip=%s", dhcpName(m.typ), m.mac, client.Name, dhcpName(typ), client.IP)
	result := "answered"
	if err := write(reply, dst); err != nil {
		s.Logger.Errorf("dhcp: reply to %s: %v", dst, err)
		result = "error"
	}
	if s.Recorder != nil {
		s.Recorder.BootpReply(client.Name, m.mac.String(), "dhcp "+dhcpName(m.typ), client.IP.String(), result)
	}
}
