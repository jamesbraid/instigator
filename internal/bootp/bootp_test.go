package bootp

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"
)

var (
	octaneMAC = net.HardwareAddr{0x08, 0x00, 0x69, 0x0e, 0xaf, 0x12}
	octaneIP  = netip.MustParseAddr("192.0.2.30")
	serverIP  = netip.MustParseAddr("192.0.2.10")
)

func testServer() *Server {
	return &Server{
		ServerIP: serverIP,
		Netmask:  netip.MustParsePrefix("192.0.2.0/24"),
		Clients:  []Client{{MAC: octaneMAC, IP: octaneIP}},
	}
}

// request builds a BOOTREQUEST from mac asking for file.
func request(mac net.HardwareAddr, xid uint32, file string) []byte {
	b := make([]byte, 300)
	b[0] = 1 // BOOTREQUEST
	b[1] = 1 // ethernet
	b[2] = 6
	binary.BigEndian.PutUint32(b[4:], xid)
	copy(b[28:], mac)
	copy(b[108:], file)
	return b
}

func TestReplyToKnownMAC(t *testing.T) {
	s := testServer()
	reply := s.handle(request(octaneMAC, 0xdeadbeef, "/6.5.30/disc1/stand/fx.64"))
	if reply == nil {
		t.Fatal("no reply for configured MAC")
	}
	if reply[0] != 2 {
		t.Fatalf("op = %d, want BOOTREPLY", reply[0])
	}
	if got := binary.BigEndian.Uint32(reply[4:]); got != 0xdeadbeef {
		t.Fatalf("xid = %#x", got)
	}
	if got := netip.AddrFrom4([4]byte(reply[16:20])); got != octaneIP {
		t.Fatalf("yiaddr = %s, want %s", got, octaneIP)
	}
	if got := netip.AddrFrom4([4]byte(reply[20:24])); got != serverIP {
		t.Fatalf("siaddr = %s, want %s", got, serverIP)
	}
	if !bytes.Equal(reply[28:34], octaneMAC) {
		t.Fatal("chaddr not echoed")
	}
	if got := string(bytes.TrimRight(reply[108:236], "\x00")); got != "/6.5.30/disc1/stand/fx.64" {
		t.Fatalf("file = %q", got)
	}
}

func TestIgnoresUnknownMAC(t *testing.T) {
	s := testServer()
	stranger := net.HardwareAddr{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01}
	if reply := s.handle(request(stranger, 1, "")); reply != nil {
		t.Fatal("replied to a MAC not in the client list")
	}
}

func TestIgnoresBootReply(t *testing.T) {
	s := testServer()
	req := request(octaneMAC, 1, "")
	req[0] = 2 // another server's reply must not be answered
	if reply := s.handle(req); reply != nil {
		t.Fatal("replied to a BOOTREPLY")
	}
}

func TestVendorAreaCarriesNetmask(t *testing.T) {
	s := testServer()
	reply := s.handle(request(octaneMAC, 1, ""))
	vend := reply[236:300]
	if !bytes.Equal(vend[0:4], []byte{99, 130, 83, 99}) {
		t.Fatalf("vendor magic = %v", vend[0:4])
	}
	// option 1 (subnet mask), length 4
	if vend[4] != 1 || vend[5] != 4 || !bytes.Equal(vend[6:10], []byte{255, 255, 255, 0}) {
		t.Fatalf("subnet mask option = %v", vend[4:10])
	}
	if vend[10] != 255 {
		t.Fatalf("vendor area not terminated: %v", vend[10])
	}
}

func TestServeRepliesOverUDP(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()

	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s := testServer()
	s.ReplyAddr = c.LocalAddr().(*net.UDPAddr) // tests can't broadcast
	go s.Serve(pc)

	if _, err := c.WriteTo(request(octaneMAC, 7, "fx"), pc.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 400)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n < 300 || buf[0] != 2 {
		t.Fatalf("reply %d bytes op %d", n, buf[0])
	}
}
