package qemunet

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

func TestListenPacketIPv4Wire(t *testing.T) {
	for _, tc := range []struct {
		name      string
		port      int
		broadcast bool
	}{
		{"bootp-broadcast", 67, true},
		{"tftp-control", 69, false},
		{"tftp-transfer", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, peer := net.Pipe()
			defer peer.Close()
			n, err := New(conn, Config{ServerIP: netip.AddrFrom4(testSrvIP), PrefixLen: 24, MAC: net.HardwareAddr([]byte(testSrvMAC))})
			if err != nil {
				t.Fatal(err)
			}
			defer n.Close()
			if err := n.stack.AddStaticNeighbor(nicID, ipv4.ProtocolNumber, tcpip.AddrFrom4(testGstIP), testGstMAC); err != nil {
				t.Fatal(err)
			}
			pc, err := n.ListenPacket(tc.port)
			if err != nil {
				t.Fatal(err)
			}
			defer pc.Close()
			dst := net.IP(testGstIP[:])
			if tc.broadcast {
				dst = net.IPv4bcast
			}
			payload := bytes.Repeat([]byte{0, 1, 2, 255, 128}, 61)
			if _, err := pc.WriteTo(payload, &net.UDPAddr{IP: dst, Port: 68}); err != nil {
				t.Fatal(err)
			}
			peer.SetReadDeadline(time.Now().Add(5 * time.Second))
			reader := frameReader{r: peer}
			frame, err := reader.read(maxFrame)
			if err != nil {
				t.Fatal(err)
			}
			if len(frame) < 42 || binary.BigEndian.Uint16(frame[12:14]) != 0x0800 {
				t.Fatalf("not IPv4: %x", frame)
			}
			ip := frame[14:]
			hlen := int(ip[0]&15) * 4
			if ip[0]>>4 != 4 || hlen < 20 || len(ip) != int(binary.BigEndian.Uint16(ip[2:4])) || ip[9] != 17 {
				t.Fatalf("invalid IPv4 UDP packet: %x", ip)
			}
			if got := wireChecksum(ip[:hlen]); got != 0 {
				t.Errorf("IPv4 checksum = %#x", got)
			}
			udp := ip[hlen:]
			if len(udp) != len(payload)+8 || binary.BigEndian.Uint16(udp[4:6]) != uint16(len(udp)) || !bytes.Equal(udp[8:], payload) {
				t.Fatalf("UDP payload/length changed: %x", udp)
			}
			if !net.IP(ip[12:16]).Equal(net.IP(testSrvIP[:])) || !net.IP(ip[16:20]).Equal(dst) {
				t.Errorf("addresses = %v -> %v", net.IP(ip[12:16]), net.IP(ip[16:20]))
			}
			if got := int(binary.BigEndian.Uint16(udp[:2])); got != pc.LocalAddr().(*net.UDPAddr).Port {
				t.Errorf("source port = %d, want %v", got, pc.LocalAddr())
			}
			if binary.BigEndian.Uint16(udp[2:4]) != 68 {
				t.Errorf("destination port = %d", binary.BigEndian.Uint16(udp[2:4]))
			}
			pseudo := append([]byte(nil), ip[12:20]...)
			pseudo = append(pseudo, 0, 17, byte(len(udp)>>8), byte(len(udp)))
			pseudo = append(pseudo, udp...)
			if binary.BigEndian.Uint16(udp[6:8]) == 0 || wireChecksum(pseudo) != 0 {
				t.Errorf("invalid UDP checksum: %x", udp[6:8])
			}
			if flags := binary.BigEndian.Uint16(ip[6:8]); flags != 0 {
				t.Errorf("IPv4 flags/offset = %#04x, want 0 (fragmentation permitted)", flags)
			}
		})
	}
}

func wireChecksum(p []byte) uint16 {
	var sum uint32
	for len(p) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(p))
		p = p[2:]
	}
	if len(p) != 0 {
		sum += uint32(p[0]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
