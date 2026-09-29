package main

import (
	"bytes"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/efs/efstest"
	"github.com/jamesbraid/instigator/internal/qemunet"
)

type lockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "i-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

// TestServeNetworkSocketAnswersBootp runs the whole serve command over a
// Unix socket. A machine stand-in sends a BOOTP request over the QEMU stream
// and the server answers with its configured address.
func TestServeNetworkSocketAnswersBootp(t *testing.T) {
	servePrivateNetworkAnswersBootp(t, "unix", shortSocketPath(t), false)
}

func TestServeNetworkSocketExitsOnDisconnect(t *testing.T) {
	servePrivateNetworkAnswersBootp(t, "unix", shortSocketPath(t), true)
}

func TestServeNetworkTCPAnswersBootp(t *testing.T) {
	servePrivateNetworkAnswersBootp(t, "tcp", loopbackEndpoint(t), false)
}

func TestServeNetworkTCPExitsOnDisconnect(t *testing.T) {
	servePrivateNetworkAnswersBootp(t, "tcp", loopbackEndpoint(t), true)
}

func loopbackEndpoint(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	l.Close()
	return address
}

func servePrivateNetworkAnswersBootp(t *testing.T, network, endpoint string, disconnect bool) {
	t.Helper()
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
	stop := make(chan os.Signal, 1)
	errCh := make(chan error, 1)
	captureDir := ""
	if disconnect {
		captureDir = filepath.Join(dir, "capture")
	}
	var output lockedBuffer
	networkSocket, networkTCP := "", ""
	if network == "tcp" {
		networkTCP = endpoint
	} else {
		networkSocket = endpoint
	}
	go func() {
		errCh <- runUntilSignal(configPath, false, captureDir, networkSocket, networkTCP, &output, stop)
	}()

	// Act as the machine: connect once the socket is up, attach a guest stack.
	conn := dialWhenReady(t, network, endpoint, time.Now().Add(5*time.Second))
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
		select {
		case serveErr := <-errCh:
			t.Fatalf("no BOOTREPLY within 5 seconds; serve returned %v; log: %s", serveErr, output.String())
		default:
			t.Fatalf("no BOOTREPLY within 5 seconds; log: %s", output.String())
		}
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

func dialWhenReady(t *testing.T, network, endpoint string, deadline time.Time) net.Conn {
	t.Helper()
	for time.Now().Before(deadline) {
		if c, err := net.Dial(network, endpoint); err == nil {
			return c
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s %s never became connectable", network, endpoint)
	return nil
}
