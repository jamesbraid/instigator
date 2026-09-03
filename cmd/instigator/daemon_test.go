package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/efs/efstest"
	"github.com/jamesbraid/instigator/internal/config"
)

// serveConfig builds the smallest config serveUntilSignal accepts, with
// every listener disabled so the test binds nothing privileged.
func serveConfig(t *testing.T) *config.Config {
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
server_ip: 192.0.2.10
clients:
  - {name: octane, mac: "08:00:69:0e:af:12", ip: 127.0.0.1}
install_sets:
  - name: "6.5.30"
    layers:
      - {name: base, source: %q}
services:
  bootp: false
  tftp: {enabled: false}
  rsh: false
`, imagePath)
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestServeNotifiesDaemonParentWhenAsked: with the daemon environment set,
// the server dials the parent's listener and writes the token exactly when
// it reaches the serving state - after the tree is built and every enabled
// listener is bound - so the parent's exit code can stand for readiness.
func TestServeNotifiesDaemonParentWhenAsked(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 128)
		n, _ := conn.Read(buf)
		got <- string(buf[:n])
	}()

	t.Setenv(daemonReadyAddrEnv, ln.Addr().String())
	t.Setenv(daemonReadyTokenEnv, "sesame")

	stop := make(chan os.Signal, 1)
	stop <- os.Interrupt
	var output bytes.Buffer
	if err := serveUntilSignal(serveConfig(t), false, "", &output, stop); err != nil {
		t.Fatal(err)
	}

	select {
	case token := <-got:
		if token != "sesame" {
			t.Fatalf("parent received %q, want the token", token)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server never notified the daemon parent")
	}
}

// TestDaemonizeReturnsOnceChildReports: the parent half returns nil as soon
// as the child writes the token, while the child is still running - the
// contract that lets a caller treat exit 0 as "serving now, still up".
func TestDaemonizeReturnsOnceChildReports(t *testing.T) {
	proc, err := daemonize(os.Args[0], []string{"-test.run=TestHelperDaemonChild$"},
		[]string{"INSTIGATOR_TEST_DAEMON_CHILD=ready"})
	if err != nil {
		t.Fatalf("daemonize: %v", err)
	}
	// Kill succeeding proves the child was still alive when daemonize
	// returned; it also cleans up the helper.
	if err := proc.Kill(); err != nil {
		t.Errorf("child was not left running: %v", err)
	}
}

// TestDaemonizeFailsWhenChildDiesFirst: a child that exits before
// reporting ready is a failed startup, and the parent says so.
func TestDaemonizeFailsWhenChildDiesFirst(t *testing.T) {
	_, err := daemonize(os.Args[0], []string{"-test.run=TestHelperDaemonChild$"},
		[]string{"INSTIGATOR_TEST_DAEMON_CHILD=fail"})
	if err == nil {
		t.Fatal("daemonize returned nil for a child that died before ready")
	}
}

// TestHelperDaemonChild is not a test: daemonize's tests re-execute the
// test binary with INSTIGATOR_TEST_DAEMON_CHILD set to run it as the child
// process. "ready" reports readiness the way serve does and stays alive;
// "fail" exits nonzero without reporting, like a failed startup.
func TestHelperDaemonChild(t *testing.T) {
	switch os.Getenv("INSTIGATOR_TEST_DAEMON_CHILD") {
	case "":
		return
	case "fail":
		os.Exit(1)
	case "ready":
		notifyDaemonReady(nil)
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
}
