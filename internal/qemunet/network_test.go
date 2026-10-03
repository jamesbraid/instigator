package qemunet

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/internal/bootp"
)

func TestNetworkQueuedDatagramsRemainDistinct(t *testing.T) {
	srv, gst := pipedNetworks(t)
	receiver, err := srv.ListenPacket(12345)
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	sender, err := gst.ListenPacket(0)
	if err != nil {
		t.Fatal(err)
	}
	defer sender.Close()
	dst := &net.UDPAddr{IP: testSrvIPAddr(), Port: 12345}
	const packets = 64
	// Stay below netstack's 32 KiB UDP receive buffer while all data is queued.
	const payloadSize = 256
	for i := range packets {
		if _, err := sender.WriteTo(bytes.Repeat([]byte{byte(i)}, payloadSize), dst); err != nil {
			t.Fatal(err)
		}
	}
	// Let every frame reuse the scratch buffer before consuming queued packets.
	deadline := time.Now().Add(5 * time.Second)
	for srv.stack.Stats().UDP.PacketsReceived.Value() < packets {
		if time.Now().After(deadline) {
			t.Fatal("datagrams did not reach the receiving stack")
		}
		time.Sleep(time.Millisecond)
	}
	if drops := srv.stack.Stats().UDP.ReceiveBufferErrors.Value(); drops != 0 {
		t.Fatalf("%d datagrams dropped before checking their data", drops)
	}
	receiver.SetReadDeadline(deadline)
	got := make([]byte, 1500)
	var seen [packets]bool
	for range packets {
		n, _, err := receiver.ReadFrom(got)
		if err != nil {
			t.Fatal(err)
		}
		// ARP resolution can reorder datagrams; each identity must survive once.
		if n != payloadSize || got[0] >= packets || !bytes.Equal(got[:n], bytes.Repeat(got[:1], payloadSize)) {
			t.Fatalf("queued datagram was overwritten: length %d, data %x", n, got[:n])
		}
		if seen[got[0]] {
			t.Fatalf("queued datagram %d appeared twice", got[0])
		}
		seen[got[0]] = true
	}
}

func TestNetworkStopsOnPeerClose(t *testing.T) {
	c, peer := net.Pipe()
	n, err := New(c, Config{ServerIP: netip.AddrFrom4(testSrvIP), PrefixLen: 24, MAC: net.HardwareAddr([]byte(testSrvMAC))})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	peer.Close()
	select {
	case <-n.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("frame pumps remained active after peer close")
	}
	if !errors.Is(n.Err(), io.EOF) {
		t.Fatalf("network error = %v, want EOF", n.Err())
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkStopsOnMalformedFrame(t *testing.T) {
	c, peer := net.Pipe()
	n, err := New(c, Config{ServerIP: netip.AddrFrom4(testSrvIP), PrefixLen: 24, MAC: net.HardwareAddr([]byte(testSrvMAC))})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	defer peer.Close()
	if _, err := peer.Write([]byte{0, 1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-n.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("frame pumps remained active after malformed frame")
	}
	if n.Err() == nil {
		t.Fatal("malformed frame did not report an error")
	}
}

type failingWriteConn struct{ net.Conn }

func (failingWriteConn) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestNetworkStopsOnWriteFailure(t *testing.T) {
	c, peer := net.Pipe()
	n, err := New(failingWriteConn{c}, Config{ServerIP: netip.AddrFrom4(testSrvIP), PrefixLen: 24, MAC: net.HardwareAddr([]byte(testSrvMAC))})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	defer peer.Close()
	pc, err := n.ListenPacket(68)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err := pc.WriteTo([]byte("packet"), &net.UDPAddr{IP: net.IPv4bcast, Port: 67}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-n.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("frame pumps remained active after write failure")
	}
	if !errors.Is(n.Err(), io.ErrClosedPipe) {
		t.Fatalf("network error = %v, want closed pipe", n.Err())
	}
}

// TestNetworkBootpEndToEnd runs the real bootp.Server on a production Network
// and drives a broadcast request from a second Network across the framed
// stream - exercising New, the frame pump, and ListenPacket together.
func TestNetworkBootpEndToEnd(t *testing.T) {
	srv, gst := pipedNetworks(t)

	pc, err := srv.ListenPacket(67)
	if err != nil {
		t.Fatalf("srv.ListenPacket(67): %v", err)
	}
	defer pc.Close()
	bs := &bootp.Server{
		ServerIP: netip.AddrFrom4(testSrvIP),
		Netmask:  netip.MustParsePrefix("10.98.0.0/24"),
		Clients: []bootp.Client{{
			Name: "guest",
			MAC:  net.HardwareAddr([]byte(testGstMAC)),
			IP:   netip.AddrFrom4(testGstIP),
		}},
	}
	go bs.Serve(pc)

	client, err := gst.ListenPacket(68)
	if err != nil {
		t.Fatalf("gst.ListenPacket(68): %v", err)
	}
	defer client.Close()
	if _, err := client.WriteTo(bootpRequest(testGstMAC, "boot"), &net.UDPAddr{IP: net.IPv4bcast, Port: 67}); err != nil {
		t.Fatalf("guest broadcast BOOTREQUEST: %v", err)
	}

	reply := make([]byte, 512)
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, _, err := client.ReadFrom(reply)
	if err != nil {
		t.Fatalf("guest never received a BOOTREPLY across the framed stream: %v", err)
	}
	reply = reply[:n]
	if reply[0] != 2 {
		t.Fatalf("op = %d, want 2 (BOOTREPLY)", reply[0])
	}
	if got := net.IP(reply[16:20]).String(); got != "10.98.0.65" {
		t.Errorf("yiaddr = %s, want 10.98.0.65", got)
	}
}
