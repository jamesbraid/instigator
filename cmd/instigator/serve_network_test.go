package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/efs/efstest"
	"github.com/jamesbraid/instigator/internal/qemunet"
	"github.com/jamesbraid/instigator/internal/status"
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

// A stream that ends inside a frame is a broken link, not the machine leaving.
func TestServeNetworkSocketFailsOnTruncatedFrame(t *testing.T) {
	endpoint := shortSocketPath(t)
	configPath := privateNetworkConfig(t, t.TempDir())
	errCh := make(chan error, 1)
	var output lockedBuffer
	go func() {
		errCh <- runUntilSignal(configPath, false, "", endpoint, "", nil, &output, nil, make(chan os.Signal, 1))
	}()
	conn := dialWhenReady(t, "unix", endpoint, time.Now().Add(5*time.Second))
	conn.Write([]byte{0, 0, 0, 100, 'x'}) // header promises 100 bytes, one follows
	conn.Close()
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "unexpected EOF") {
			t.Fatalf("runUntilSignal = %v, want an unexpected EOF failure; log: %s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop")
	}
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

// privateNetworkConfig writes a one-set config for the private network,
// with BOOTP only, and returns its path.
func privateNetworkConfig(t *testing.T, dir string) string {
	t.Helper()
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
	return configPath
}

func servePrivateNetworkAnswersBootp(t *testing.T, network, endpoint string, disconnect bool) {
	t.Helper()
	dir := t.TempDir()
	configPath := privateNetworkConfig(t, dir)
	stop := make(chan os.Signal, 1)
	errCh := make(chan error, 1)
	captureDir := ""
	if disconnect {
		captureDir = filepath.Join(dir, "capture")
	}
	var output, jsonOut lockedBuffer
	networkSocket, networkTCP := "", ""
	if network == "tcp" {
		networkTCP = endpoint
	} else {
		networkSocket = endpoint
	}
	go func() {
		errCh <- runUntilSignal(configPath, false, captureDir, networkSocket, networkTCP, nil, &output, status.New(&jsonOut), stop)
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
		// The machine leaving is the normal end of a private-network run.
		if err != nil {
			t.Fatalf("runUntilSignal: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not shut down")
	}
	wantReason, wantNetwork := "signal", "socket"
	if disconnect {
		wantReason = "guest_disconnected"
	}
	if network == "tcp" {
		wantNetwork = "tcp"
	}
	checkStatusLines(t, jsonOut.String(), wantNetwork, wantReason)
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

// A run that fails before serving still ends its --json output with the
// reason, so a caller can show why.
func TestServeJSONReportsStartupError(t *testing.T) {
	var out bytes.Buffer
	err := runUntilSignal(filepath.Join(t.TempDir(), "missing.yaml"), false, "", "", "", nil, new(bytes.Buffer), status.New(&out), nil)
	if err == nil {
		t.Fatal("runUntilSignal succeeded on a missing config")
	}
	var m map[string]any
	if jerr := json.Unmarshal(out.Bytes(), &m); jerr != nil || m["type"] != "stopped" || m["reason"] != "error" || m["error"] != err.Error() {
		t.Errorf("status output %q, want one stopped error line naming %q", out.String(), err)
	}
}

// checkStatusLines holds serve --json to its contract: only JSON objects,
// ready first, the BOOTP answer, and stopped last with the given reason.
func checkStatusLines(t *testing.T, out, network, reason string) {
	t.Helper()
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("status line %q is not JSON: %v", l, err)
		}
		if m["v"] != 1.0 {
			t.Errorf("status line without v 1: %q", l)
		}
		lines = append(lines, m)
	}
	if len(lines) < 3 || lines[0]["type"] != "ready" || lines[0]["network"] != network {
		t.Fatalf("status lines do not start with a %s ready line:\n%s", network, out)
	}
	answered := false
	for _, m := range lines {
		answered = answered || (m["type"] == "bootp" && m["result"] == "answered" && m["ip"] == "10.98.0.65")
	}
	if !answered {
		t.Errorf("no answered bootp line:\n%s", out)
	}
	if last := lines[len(lines)-1]; last["type"] != "stopped" || last["reason"] != reason {
		t.Errorf("last status line = %v, want stopped with reason %s", last, reason)
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

// Whichever pump sees a closed emulator first decides the error, so a write
// refused by the gone peer counts as the machine leaving, as an end does.
func TestMachineLeft(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("read frame: %w", io.EOF), true},
		{fmt.Errorf("write frame: %w", &os.SyscallError{Syscall: "write", Err: syscall.EPIPE}), true},
		{fmt.Errorf("read frame: %w", &os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}), true},
		{fmt.Errorf("read frame: %w", io.ErrUnexpectedEOF), false},
		{errors.New("qemunet: framed length 99999 exceeds maximum 1518"), false},
		{nil, false},
	} {
		if got := machineLeft(tc.err); got != tc.want {
			t.Errorf("machineLeft(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
