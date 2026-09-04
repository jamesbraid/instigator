//go:build unix

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/efs/efstest"
)

// What serve promises is process behaviour - an exit code, a signal, a
// readiness datagram - so these drive the built binary.

var built struct {
	sync.Once
	path string
	err  error
}

func instigator(t *testing.T) string {
	t.Helper()
	built.Do(func() {
		// Not t.TempDir: the binary outlives the test that built it.
		dir, err := os.MkdirTemp("", "instigator")
		if err != nil {
			built.err = err
			return
		}
		built.path = filepath.Join(dir, "instigator")
		out, err := exec.Command("go", "build", "-o", built.path, ".").CombinedOutput()
		if err != nil {
			built.err = fmt.Errorf("build: %v: %s", err, out)
		}
	})
	if built.err != nil {
		t.Fatal(built.err)
	}
	return built.path
}

// serveConfig writes a config whose single set is a small image. A bootp
// port of 0 disables every listener; otherwise bootp uses that port.
func serveConfig(t *testing.T, bootp int) string {
	t.Helper()
	dir := t.TempDir()
	image := efstest.New()
	sa := image.AddFile(0o444, []byte("sa"))
	image.SetRoot(map[string]uint32{"dist": image.AddDir(map[string]uint32{"sa": sa})})
	imagePath := filepath.Join(dir, "dist.image")
	if err := os.WriteFile(imagePath, image.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	services := "services: {bootp: false, tftp: {enabled: false}, rsh: false}"
	if bootp != 0 {
		services = fmt.Sprintf("services: {tftp: {enabled: false}, rsh: false}\nports: {bootp: %d}", bootp)
	}
	path := filepath.Join(dir, "instigator.yaml")
	config := fmt.Sprintf(`
server_ip: 192.0.2.10
clients:
  - {name: octane, mac: "08:00:69:0e:af:12", ip: 127.0.0.1}
install_sets:
  - name: "6.5.30"
    layers:
      - {name: base, source: %q}
%s
`, imagePath, services)
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// serveFails runs serve with a deadline, so a server that wrongly keeps
// running is reported in seconds rather than at the test timeout.
func serveFails(t *testing.T, config string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, instigator(t), "serve", config).CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("serve kept running:\n%s", out)
	}
	if err == nil {
		t.Fatalf("serve exited zero:\n%s", out)
	}
	return string(out)
}

// A serve that cannot read its configuration, or whose port is taken,
// must fail and say which - not sit in the foreground pretending.
func TestServeExitsOnStartupFailures(t *testing.T) {
	if out := serveFails(t, "/nonexistent.yaml"); !strings.Contains(out, "nonexistent.yaml") {
		t.Errorf("missing config not named: %s", out)
	}

	held, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := held.LocalAddr().(*net.UDPAddr).Port
	if out := serveFails(t, serveConfig(t, port)); !strings.Contains(out, "bootp") {
		t.Errorf("taken port %d not named: %s", port, out)
	}
}

// Readiness is what releases systemctl start under a Type=notify unit, so
// it has to arrive once the server is serving. SIGTERM then shuts it down
// rather than killing it: the capture is finalized on the way out.
func TestServeReportsReadyThenStopsOnSIGTERM(t *testing.T) {
	// A unix socket path is limited to about a hundred bytes, which a
	// test temporary directory can exceed.
	sock := filepath.Join("/tmp", fmt.Sprintf("instigator-notify-%d", os.Getpid()))
	conn, err := net.ListenPacket("unixgram", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { conn.Close(); os.Remove(sock) }()

	cmd := exec.Command(instigator(t), "serve", serveConfig(t, 0))
	cmd.Env = append(os.Environ(), "NOTIFY_SOCKET="+sock)
	// The log goes to stderr: a supervisor waiting on readiness may have
	// closed stdout, as systemd-notify --fork does.
	log, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	buf := make([]byte, 128)
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		t.Fatalf("no readiness datagram: %v", err)
	}
	if got := string(buf[:n]); !strings.Contains(got, "READY=1") {
		t.Fatalf("datagram = %q, want READY=1", got)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(log)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("serve exited %v after SIGTERM:\n%s", err, out)
	}
	// Readiness must mean serving, not merely started.
	if !strings.Contains(string(out), "serving") {
		t.Errorf("server never logged serving:\n%s", out)
	}
}

// A caller that set NOTIFY_SOCKET is waiting to be told the server is
// serving. If that report cannot be delivered the server must not carry
// on serving unannounced: the waiter would hang until its own timeout,
// and the operator would be left guessing.
func TestServeExitsWhenReadinessCannotBeReported(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, instigator(t), "serve", serveConfig(t, 0))
	cmd.Env = append(os.Environ(), "NOTIFY_SOCKET=/nonexistent/notify.sock")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("serve kept serving with nobody able to hear it:\n%s", out)
	}
	if err == nil {
		t.Fatalf("serve exited zero after failing to report readiness:\n%s", out)
	}
	if !strings.Contains(string(out), "readiness") {
		t.Errorf("error does not name the failure: %s", out)
	}
}
