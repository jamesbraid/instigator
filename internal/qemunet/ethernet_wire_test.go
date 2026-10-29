package qemunet

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"
	"time"
)

func ethernetWireNetwork(t *testing.T) (*Network, net.Conn) {
	t.Helper()
	conn, peer := net.Pipe()
	n, err := New(conn, Config{ServerIP: netip.AddrFrom4(testSrvIP), PrefixLen: 24, MAC: net.HardwareAddr([]byte(testSrvMAC))})
	if err != nil {
		peer.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close(); n.Close() })
	peer.SetDeadline(time.Now().Add(5 * time.Second))
	return n, peer
}

func TestARPReplyEthernetPadding(t *testing.T) {
	_, peer := ethernetWireNetwork(t)
	request := make([]byte, 60)
	copy(request[:6], bytes.Repeat([]byte{255}, 6))
	copy(request[6:12], []byte(testGstMAC))
	copy(request[12:22], []byte{8, 6, 0, 1, 8, 0, 6, 4, 0, 1})
	copy(request[22:28], []byte(testGstMAC))
	copy(request[28:32], testGstIP[:])
	copy(request[38:42], testSrvIP[:])
	writer := frameWriter{w: peer}
	if err := writer.write(request); err != nil {
		t.Fatal(err)
	}
	reader := frameReader{r: peer}
	reply, err := reader.read(maxFrame)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 42)
	copy(want[:6], []byte(testGstMAC))
	copy(want[6:12], []byte(testSrvMAC))
	copy(want[12:22], []byte{8, 6, 0, 1, 8, 0, 6, 4, 0, 2})
	copy(want[22:28], []byte(testSrvMAC))
	copy(want[28:32], testSrvIP[:])
	copy(want[32:38], []byte(testGstMAC))
	copy(want[38:42], testGstIP[:])
	if len(reply) < len(want) || !bytes.Equal(reply[:len(want)], want) {
		t.Fatalf("ARP reply addresses/header changed: %x, want prefix %x", reply, want)
	}
	checkEthernetPadding(t, reply, len(want))
}

func TestMinimalUDPEthernetPadding(t *testing.T) {
	n, peer := ethernetWireNetwork(t)
	pc, err := n.ListenPacket(67)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	payload := []byte{0xa5}
	if _, err := pc.WriteTo(payload, &net.UDPAddr{IP: net.IPv4bcast, Port: 68}); err != nil {
		t.Fatal(err)
	}
	reader := frameReader{r: peer}
	frame, err := reader.read(maxFrame)
	if err != nil {
		t.Fatal(err)
	}
	if len(frame) < 43 || binary.BigEndian.Uint16(frame[12:14]) != 0x800 {
		t.Fatalf("invalid Ethernet IPv4 frame: %x", frame)
	}
	if !bytes.Equal(frame[:6], bytes.Repeat([]byte{255}, 6)) || !bytes.Equal(frame[6:12], []byte(testSrvMAC)) {
		t.Errorf("Ethernet addresses changed: %x", frame[:12])
	}
	ip := frame[14:]
	hlen := int(ip[0]&15) * 4
	total := int(binary.BigEndian.Uint16(ip[2:4]))
	if ip[0]>>4 != 4 || hlen != 20 || total != 29 || ip[9] != 17 {
		t.Fatalf("IPv4 length/protocol changed: %x", ip[:20])
	}
	ip = ip[:total]
	if !bytes.Equal(ip[12:16], testSrvIP[:]) || !net.IP(ip[16:20]).Equal(net.IPv4bcast) {
		t.Errorf("IPv4 addresses changed: %x", ip[12:20])
	}
	if wireChecksum(ip[:hlen]) != 0 {
		t.Error("invalid IPv4 checksum")
	}
	udp := ip[hlen:]
	if binary.BigEndian.Uint16(udp[:2]) != 67 || binary.BigEndian.Uint16(udp[2:4]) != 68 || binary.BigEndian.Uint16(udp[4:6]) != 9 || !bytes.Equal(udp[8:], payload) {
		t.Fatalf("UDP ports/length/payload changed: %x", udp)
	}
	pseudo := append([]byte(nil), ip[12:20]...)
	pseudo = append(pseudo, 0, 17, 0, byte(len(udp)))
	pseudo = append(pseudo, udp...)
	if binary.BigEndian.Uint16(udp[6:8]) == 0 || wireChecksum(pseudo) != 0 {
		t.Error("invalid UDP checksum")
	}
	checkEthernetPadding(t, frame, 14+total)
}

func checkEthernetPadding(t *testing.T, frame []byte, contentLength int) {
	t.Helper()
	// Ethernet's 64-byte minimum includes the four-byte FCS, which the
	// QEMU stream backend does not carry.
	if len(frame) < 60 {
		t.Errorf("Ethernet frame has %d bytes, want at least 60 without FCS", len(frame))
	}
	if !bytes.Equal(frame[contentLength:], make([]byte, len(frame)-contentLength)) {
		t.Errorf("Ethernet padding is not zero: %x", frame[contentLength:])
	}
}
