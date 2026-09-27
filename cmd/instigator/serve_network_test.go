package main

import (
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
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
	testServeNetworkSocket(t, false)
}

func TestServeNetworkSocketExitsOnDisconnect(t *testing.T) {
	testServeNetworkSocket(t, true)
}

func testServeNetworkSocket(t *testing.T, disconnect bool) {
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
	captureDir := ""
	if disconnect {
		captureDir = filepath.Join(dir, "capture")
	}
	go func() { errCh <- runUntilSignal(configPath, false, captureDir, sockPath, io.Discard, stop) }()

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
	reply := make([]byte, 512)
	deadline := time.Now().Add(5 * time.Second)
	received := false
	for time.Now().Before(deadline) {
		if _, err := client.WriteTo(req, &net.UDPAddr{IP: net.IPv4bcast, Port: 67}); err != nil {
			t.Fatalf("guest BOOTREQUEST: %v", err)
		}
		client.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		if _, _, err := client.ReadFrom(reply); err == nil {
			received = true
			break
		} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
			t.Fatalf("read BOOTREPLY over the network socket: %v", err)
		}
	}
	if !received {
		t.Fatal("no BOOTREPLY over the network socket within 5 seconds")
	}
	if reply[0] != 2 {
		t.Fatalf("op = %d, want BOOTREPLY", reply[0])
	}
	if got := net.IP(reply[16:20]).String(); got != "10.98.0.65" {
		t.Fatalf("yiaddr = %s, want 10.98.0.65", got)
	}

	if disconnect {
		if err := gst.Close(); err != nil {
			t.Fatalf("close guest network: %v", err)
		}
	} else {
		stop <- os.Interrupt
	}
	select {
	case err := <-errCh:
		if disconnect && (err == nil || !strings.Contains(err.Error(), "private network disconnected")) {
			t.Fatalf("runUntilSignal on disconnect = %v", err)
		}
		if !disconnect && err != nil {
			t.Fatalf("runUntilSignal: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not shut down")
	}
	if disconnect {
		events, err := os.ReadFile(filepath.Join(captureDir, "events.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, line := range strings.Split(string(events), "\n") {
			if strings.Contains(line, `"event":"server_stop"`) && strings.Contains(line, `"result":"disconnected"`) {
				found = true
			}
		}
		if !found {
			t.Fatalf("capture did not record disconnected server stop: %s", events)
		}
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
