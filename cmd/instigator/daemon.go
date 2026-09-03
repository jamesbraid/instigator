package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/jamesbraid/instigator/internal/logging"
)

// serve --daemon turns the serve command's exit code into a readiness
// signal: the command returns 0 only once the server it launched has built
// its tree - every media source resolved and fetched - and bound every
// enabled listener, and returns 1 if that startup fails. The server itself
// keeps running after the command returns.
//
// Go cannot fork after the runtime starts, so the parent re-executes this
// binary as a detached child running the ordinary serve path, minus the
// --daemon flag. The child inherits stdout and stderr, so the server log
// and any startup error land exactly where a foreground serve would put
// them, and SIGTERM to the child shuts it down the same way. The child
// reports readiness by dialing a localhost listener the parent opens for
// the occasion and writing back a single-use token, both passed down in
// the environment; the token keeps a stray local connection from standing
// in for the server's own report.
const (
	daemonReadyAddrEnv  = "INSTIGATOR_DAEMON_READY_ADDR"
	daemonReadyTokenEnv = "INSTIGATOR_DAEMON_READY_TOKEN"
)

// errDaemonChildFailed marks a child that exited before reporting ready.
// The child has already written the real error to stderr, so the parent
// exits 1 without adding a second message on top of it.
var errDaemonChildFailed = errors.New("server exited before reporting ready")

// daemonize starts exe with args as the detached server process and
// returns once it either reports ready (nil, with the still-running
// process) or exits (errDaemonChildFailed). extraEnv exists for tests,
// which re-execute the test binary and need to steer it into the child
// role; the production caller passes nil. There is deliberately no
// deadline: fetching media over the network can honestly take a while,
// and a caller that wants a bound can put one around the whole command.
func daemonize(exe string, args []string, extraEnv []string) (*os.Process, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("daemon: readiness listener: %w", err)
	}
	defer ln.Close()
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("daemon: token: %w", err)
	}
	token := hex.EncodeToString(raw)

	cmd := exec.Command(exe, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		daemonReadyAddrEnv+"="+ln.Addr().String(),
		daemonReadyTokenEnv+"="+token,
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.SysProcAttr = daemonSysProcAttr()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("daemon: %w", err)
	}

	ready := make(chan struct{})
	go func() {
		// Accept until the true child reports; a connection that fails
		// the token check is closed and waiting resumes.
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			buf := make([]byte, len(token))
			n, _ := conn.Read(buf)
			conn.Close()
			if string(buf[:n]) == token {
				close(ready)
				return
			}
		}
	}()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	select {
	case <-ready:
		return cmd.Process, nil
	case <-exited:
		return nil, errDaemonChildFailed
	}
}

// notifyDaemonReady reports readiness to a waiting serve --daemon parent,
// if the environment names one, and is a no-op otherwise. A failed report
// is logged and the server carries on: the parent may simply be gone, and
// a running server is worth more than a delivered notification.
func notifyDaemonReady(logger *logging.Logger) {
	addr := os.Getenv(daemonReadyAddrEnv)
	if addr == "" {
		return
	}
	conn, err := net.DialTimeout("tcp", addr, 10*time.Second)
	if err != nil {
		if logger != nil {
			logger.Warnf("daemon: reporting ready: %v", err)
		}
		return
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte(os.Getenv(daemonReadyTokenEnv))); err != nil && logger != nil {
		logger.Warnf("daemon: reporting ready: %v", err)
	}
}
