package main

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/efs/efstest"
	"github.com/jamesbraid/instigator/internal/qemunet"
)

// TestServeNetworkSocketAnswersBootp runs the whole serve command in
// private-network mode: it listens on a Unix socket, a machine stand-in
// connects and boots a BOOTP request over the QEMU stream, and the server
// answers with the configured address - end to end, unprivileged.
func TestServeNetworkSocketAnswersBootp(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "dist.image")
	image := efstest.New()
	sa := image.AddFile(0o444, []byte("sa"))
	image.SetRoot(map[string]uint32{"dist": image.AddDir(map[string]uint32{"sa": sa})})
	if err := os.WriteFile(imagePath, image.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}

	yaml := fmt.Sprintf(`
server_ip: 10.98.0.2
netmask: 10.98.0.0/24
clients:
  - {name: guest, mac: "08:00:69:12:34:56", ip: 10.98.0.65}
install_sets:
  - name: "6.5.30"
    layers:
      - {name: base, source: %q}
services:
  bootp: true
  tftp: {enabled: false}
  rsh: false
`, imagePath)
	configPath := filepath.Join(dir, "instigator.yaml")
	if err := os.WriteFile(configPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(dir, "irix-install.sock")

	stop := make(chan os.Signal, 1)
	errCh := make(chan error, 1)
	go func() { errCh <- runUntilSignal(configPath, false, "", sockPath, io.Discard, stop) }()

	// Act as the machine: connect once the socket is up, attach a guest stack.
	conn := dialWhenReady(t, sockPath, time.Now().Add(5*time.Second))
	guestMAC := net.HardwareAddr{0x08, 0x00, 0x69, 0x12, 0x34, 0x56}
	gst, err := qemunet.New(conn, qemunet.Config{
		ServerIP:  netip.AddrFrom4([4]byte{10, 98, 0, 65}),
		PrefixLen: 24,
		MAC:       guestMAC,
	})
	if err != nil {
		t.Fatalf("guest New: %v", err)
	}
	defer gst.Close()

	client, err := gst.ListenPacket(68)
	if err != nil {
		t.Fatalf("guest ListenPacket(68): %v", err)
	}
	defer client.Close()

	req := make([]byte, 300)
	req[0], req[1], req[2] = 1, 1, 6
	copy(req[28:34], guestMAC)
	if _, err := client.WriteTo(req, &net.UDPAddr{IP: net.IPv4bcast, Port: 67}); err != nil {
		t.Fatalf("guest BOOTREQUEST: %v", err)
	}

	reply := make([]byte, 512)
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := client.ReadFrom(reply); err != nil {
		t.Fatalf("no BOOTREPLY over the network socket: %v", err)
	}
	if reply[0] != 2 {
		t.Fatalf("op = %d, want BOOTREPLY", reply[0])
	}
	if got := net.IP(reply[16:20]).String(); got != "10.98.0.65" {
		t.Fatalf("yiaddr = %s, want 10.98.0.65", got)
	}

	stop <- os.Interrupt
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("runUntilSignal: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not shut down")
	}
}

func dialWhenReady(t *testing.T, path string, deadline time.Time) net.Conn {
	t.Helper()
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", path); err == nil {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("socket %s never became connectable", path)
	return nil
}
