package qemunet

import (
	"net"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/internal/bootp"
)

// TestListenerAcceptsAndServes drives the whole endpoint path: Instigator
// listens on a Unix socket, a machine connects, and the accepted Network
// carries a real BOOTP exchange.
func TestListenerAcceptsAndServes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "irix-install.sock")
	l, err := Listen(path, Config{ServerIP: netip.AddrFrom4(testSrvIP), PrefixLen: 24, MAC: net.HardwareAddr([]byte(testSrvMAC))})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer l.Close()

	gstCh := make(chan *Network, 1)
	errCh := make(chan error, 1)
	go func() {
		c, derr := net.Dial("unix", path)
		if derr != nil {
			errCh <- derr
			return
		}
		g, gerr := New(c, Config{ServerIP: netip.AddrFrom4(testGstIP), PrefixLen: 24, MAC: net.HardwareAddr([]byte(testGstMAC))})
		if gerr != nil {
			errCh <- gerr
			return
		}
		gstCh <- g
	}()

	srv, err := l.Accept()
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	defer srv.Close()

	var gst *Network
	select {
	case gst = <-gstCh:
		defer gst.Close()
	case err := <-errCh:
		t.Fatalf("guest connect: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("guest never connected")
	}

	pc, err := srv.ListenPacket(67)
	if err != nil {
		t.Fatalf("srv.ListenPacket(67): %v", err)
	}
	defer pc.Close()
	bs := &bootp.Server{
		ServerIP: netip.AddrFrom4(testSrvIP),
		Netmask:  netip.MustParsePrefix("10.98.0.0/24"),
		Clients:  []bootp.Client{{Name: "guest", MAC: net.HardwareAddr([]byte(testGstMAC)), IP: netip.AddrFrom4(testGstIP)}},
	}
	go bs.Serve(pc)

	client, err := gst.ListenPacket(68)
	if err != nil {
		t.Fatalf("gst.ListenPacket(68): %v", err)
	}
	defer client.Close()
	if _, err := client.WriteTo(bootpRequest(testGstMAC, "boot"), &net.UDPAddr{IP: net.IPv4bcast, Port: 67}); err != nil {
		t.Fatalf("guest BOOTREQUEST: %v", err)
	}
	reply := make([]byte, 512)
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := client.ReadFrom(reply); err != nil {
		t.Fatalf("guest never received a BOOTREPLY over the Unix socket: %v", err)
	}
	if reply[0] != 2 || net.IP(reply[16:20]).String() != "10.98.0.65" {
		t.Fatalf("bad reply: op=%d yiaddr=%s", reply[0], net.IP(reply[16:20]))
	}
}

func TestUnixListenerCanReopenAfterClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install.sock")
	cfg := Config{ServerIP: netip.AddrFrom4(testSrvIP), PrefixLen: 24, MAC: net.HardwareAddr([]byte(testSrvMAC))}
	first, err := Listen(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Listen(path, cfg)
	if err != nil {
		t.Fatalf("reopen after Close: %v", err)
	}
	defer second.Close()
}

func TestListenTCPRejectsNonLoopback(t *testing.T) {
	cfg := Config{ServerIP: netip.AddrFrom4(testSrvIP), PrefixLen: 24, MAC: net.HardwareAddr([]byte(testSrvMAC))}
	for _, address := range []string{"0.0.0.0:12345", "192.0.2.1:12345", "localhost:12345", "127.0.0.1:0"} {
		if l, err := ListenTCP(address, cfg); err == nil {
			l.Close()
			t.Errorf("ListenTCP(%q) accepted a non-loopback or unusable address", address)
		}
	}
}
