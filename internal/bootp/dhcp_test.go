package bootp

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
)

// dhcpRequestMsg builds a client message of type typ with the given
// ciaddr and options.
func dhcpRequestMsg(mac net.HardwareAddr, typ byte, ciaddr netip.Addr, opts ...[]byte) []byte {
	b := make([]byte, 548)
	b[0], b[1], b[2] = 1, 1, 6
	binary.BigEndian.PutUint32(b[4:], 0xcafe0001)
	copy(b[12:], ciaddr.AsSlice())
	copy(b[28:], mac)
	w := 236 + copy(b[236:], []byte{99, 130, 83, 99, 53, 1, typ})
	for _, o := range opts {
		w += copy(b[w:], o)
	}
	b[w] = 255
	return b
}

func opt(code byte, v ...byte) []byte { return append([]byte{code, byte(len(v))}, v...) }

// options returns the option values of a DHCP reply by code.
func options(t *testing.T, rep []byte) map[byte][]byte {
	t.Helper()
	if !bytes.Equal(rep[236:240], dhcpCookie) {
		t.Fatal("no cookie")
	}
	out := map[byte][]byte{}
	o := rep[240:]
	for len(o) > 0 && o[0] != 255 {
		if o[0] == 0 {
			o = o[1:]
			continue
		}
		out[o[0]] = o[2 : 2+int(o[1])]
		o = o[2+int(o[1]):]
	}
	return out
}

type sent struct {
	reply []byte
	dst   net.Addr
}

func dhcpServe(s *Server, req []byte) *sent {
	m, ok := parseDHCP(req)
	if !ok {
		return nil
	}
	var got *sent
	s.serveDHCP(m, func(reply []byte, dst net.Addr) error {
		got = &sent{reply, dst}
		return nil
	})
	return got
}

func dhcpTestServer() *Server {
	s := testServer()
	s.DHCP = true
	return s
}

func TestDHCPOfferFields(t *testing.T) {
	s := dhcpTestServer()
	s.Clients[0].Hostname = "origin200"
	got := dhcpServe(s, dhcpRequestMsg(octaneMAC, dhcpDiscover, netip.Addr{}))
	if got == nil {
		t.Fatal("no OFFER")
	}
	r := got.reply
	if r[0] != 2 || binary.BigEndian.Uint32(r[4:]) != 0xcafe0001 {
		t.Fatalf("op/xid wrong: %x", r[:8])
	}
	if netip.AddrFrom4([4]byte(r[16:20])) != octaneIP || netip.AddrFrom4([4]byte(r[20:24])) != serverIP {
		t.Fatalf("yiaddr %v siaddr %v", r[16:20], r[20:24])
	}
	if !bytes.Equal(r[28:34], octaneMAC) {
		t.Fatal("chaddr not echoed")
	}
	o := options(t, r)
	for code, want := range map[byte][]byte{
		53: {dhcpOffer},
		54: serverIP.AsSlice(),
		51: {0x01, 0xe1, 0x33, 0x80},
		1:  {255, 255, 255, 0},
		12: []byte("origin200"),
	} {
		if !bytes.Equal(o[code], want) {
			t.Errorf("option %d = %v, want %v", code, o[code], want)
		}
	}
	if _, ok := o[3]; ok {
		t.Error("router option present; none is configured")
	}
	if u := got.dst.(*net.UDPAddr); !u.IP.Equal(net.IPv4bcast) || u.Port != 68 {
		t.Errorf("dst = %v, want 255.255.255.255:68", u)
	}
	if len(r) < packetLen {
		t.Errorf("reply is %d bytes", len(r))
	}
}

func TestDHCPNoHostnameWithout(t *testing.T) {
	o := options(t, dhcpServe(dhcpTestServer(), dhcpRequestMsg(octaneMAC, dhcpDiscover, netip.Addr{})).reply)
	if _, ok := o[12]; ok {
		t.Error("option 12 present without a host name")
	}
}

func TestDHCPRequestOutcomes(t *testing.T) {
	other := netip.MustParseAddr("192.0.2.99")
	tests := []struct {
		name string
		req  []byte
		want byte // 0: no reply
		dst  string
	}{
		{"selecting right", dhcpRequestMsg(octaneMAC, dhcpRequest, netip.Addr{}, opt(50, octaneIP.AsSlice()...), opt(54, serverIP.AsSlice()...)), dhcpAck, "255.255.255.255:68"},
		{"selecting wrong", dhcpRequestMsg(octaneMAC, dhcpRequest, netip.Addr{}, opt(50, other.AsSlice()...), opt(54, serverIP.AsSlice()...)), dhcpNak, "255.255.255.255:68"},
		{"selecting another server", dhcpRequestMsg(octaneMAC, dhcpRequest, netip.Addr{}, opt(50, octaneIP.AsSlice()...), opt(54, 192, 0, 2, 77)), 0, ""},
		{"init-reboot right", dhcpRequestMsg(octaneMAC, dhcpRequest, netip.Addr{}, opt(50, octaneIP.AsSlice()...)), dhcpAck, "255.255.255.255:68"},
		{"init-reboot wrong", dhcpRequestMsg(octaneMAC, dhcpRequest, netip.Addr{}, opt(50, other.AsSlice()...)), dhcpNak, "255.255.255.255:68"},
		{"renewing right", dhcpRequestMsg(octaneMAC, dhcpRequest, octaneIP), dhcpAck, "192.0.2.30:68"},
		{"renewing wrong", dhcpRequestMsg(octaneMAC, dhcpRequest, other), dhcpNak, "255.255.255.255:68"},
		{"no address", dhcpRequestMsg(octaneMAC, dhcpRequest, netip.Addr{}, opt(54, serverIP.AsSlice()...)), 0, ""},
		{"decline", dhcpRequestMsg(octaneMAC, 4, netip.Addr{}, opt(50, octaneIP.AsSlice()...)), 0, ""},
		{"release", dhcpRequestMsg(octaneMAC, 7, octaneIP), 0, ""},
		{"inform", dhcpRequestMsg(octaneMAC, 8, octaneIP), 0, ""},
		{"unknown mac", dhcpRequestMsg(net.HardwareAddr{8, 0, 0x69, 9, 9, 9}, dhcpDiscover, netip.Addr{}), 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dhcpServe(dhcpTestServer(), tc.req)
			if tc.want == 0 {
				if got != nil {
					t.Fatalf("replied %v, want silence", options(t, got.reply)[53])
				}
				return
			}
			if got == nil {
				t.Fatal("no reply")
			}
			o := options(t, got.reply)
			if len(o[53]) != 1 || o[53][0] != tc.want {
				t.Fatalf("type = %v, want %d", o[53], tc.want)
			}
			if got.dst.String() != tc.dst {
				t.Errorf("dst = %s, want %s", got.dst, tc.dst)
			}
			if tc.want == dhcpNak {
				if !bytes.Equal(got.reply[16:24], make([]byte, 8)) {
					t.Error("NAK carries yiaddr or siaddr")
				}
				if _, ok := o[51]; ok {
					t.Error("NAK carries a lease")
				}
			}
		})
	}
}

func TestDHCPReplyIsDeterministic(t *testing.T) {
	req := dhcpRequestMsg(octaneMAC, dhcpDiscover, netip.Addr{})
	a := dhcpServe(dhcpTestServer(), req).reply
	b := dhcpServe(dhcpTestServer(), req).reply
	if !bytes.Equal(a, b) {
		t.Fatal("two replies to one request differ")
	}
}

func TestParseDHCPRejects(t *testing.T) {
	good := dhcpRequestMsg(octaneMAC, dhcpDiscover, netip.Addr{})
	if _, ok := parseDHCP(good); !ok {
		t.Fatal("rejected a good message")
	}
	noCookie := append([]byte(nil), good...)
	noCookie[236] = 0
	trunc := append([]byte(nil), good[:244]...)
	trunc[240], trunc[241] = 53, 9 // length past the end
	noType := dhcpRequestMsg(octaneMAC, dhcpDiscover, netip.Addr{})
	copy(noType[240:], []byte{0, 0, 0})
	reply := append([]byte(nil), good...)
	reply[0] = 2
	for name, b := range map[string][]byte{"no cookie": noCookie, "truncated option": trunc, "no type": noType, "reply": reply, "short": good[:200]} {
		if _, ok := parseDHCP(b); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDHCPOffByDefault(t *testing.T) {
	// handle mirrors Serve's legacy path, which a server with DHCP off uses
	// for every message.
	if handle(testServer(), dhcpRequestMsg(octaneMAC, dhcpDiscover, netip.Addr{})) != nil {
		t.Fatal("answered DHCP with DHCP off")
	}
}
