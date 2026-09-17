package qemunet

import (
	"net"
	"net/netip"
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip"
)

// The functional tests run the real services (bootp, tftp, rsh) on two
// production Networks joined by an in-memory stream - one addressed as
// Instigator, one as the QEMU guest. Everything crosses the real QEMU stream
// framing and frame pump, unprivileged, with no QEMU, TAP, or root.

var (
	testSrvMAC = tcpip.LinkAddress([]byte{0x08, 0x00, 0x69, 0x00, 0x00, 0x02})
	testGstMAC = tcpip.LinkAddress([]byte{0x08, 0x00, 0x69, 0x12, 0x34, 0x56})
	testSrvIP  = [4]byte{10, 98, 0, 2}
	testGstIP  = [4]byte{10, 98, 0, 65}
)

func testSrvIPAddr() net.IP { return net.IP(testSrvIP[:]) }

// pipedNetworks returns two production Networks joined by an in-memory stream,
// one addressed as Instigator and one as the QEMU guest.
func pipedNetworks(t *testing.T) (srv, gst *Network) {
	t.Helper()
	c1, c2 := net.Pipe()
	srv, err := New(c1, Config{ServerIP: netip.AddrFrom4(testSrvIP), PrefixLen: 24, MAC: net.HardwareAddr([]byte(testSrvMAC))})
	if err != nil {
		t.Fatalf("New(srv): %v", err)
	}
	gst, err = New(c2, Config{ServerIP: netip.AddrFrom4(testGstIP), PrefixLen: 24, MAC: net.HardwareAddr([]byte(testGstMAC))})
	if err != nil {
		srv.Close()
		t.Fatalf("New(gst): %v", err)
	}
	t.Cleanup(func() { gst.Close(); srv.Close() })
	return srv, gst
}

// bootpRequest builds a 300-byte RFC 951 BOOTREQUEST for mac asking for file.
func bootpRequest(mac tcpip.LinkAddress, file string) []byte {
	p := make([]byte, 300)
	p[0] = 1 // BOOTREQUEST
	p[1] = 1 // htype ethernet
	p[2] = 6 // hlen
	copy(p[28:34], []byte(mac))
	copy(p[108:236], file)
	copy(p[236:240], []byte{99, 130, 83, 99}) // RFC 1048 magic cookie
	p[240] = 255                              // end
	return p
}
