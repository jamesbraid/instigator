package qemunet

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/internal/bootp"
)

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
