package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	sdnotify "github.com/coreos/go-systemd/v22/daemon"

	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/logging"
	"github.com/jamesbraid/instigator/internal/qemunet"
	"github.com/jamesbraid/instigator/internal/serve"
)

// run serves until a signal stops it. The log goes to stderr because a
// caller waiting on readiness may have closed stdout, as
// systemd-notify --fork does.
func run(configPath string, verbose bool, captureDir, networkSocket string) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	return runUntilSignal(configPath, verbose, captureDir, networkSocket, os.Stderr, sig)
}

func runUntilSignal(configPath string, verbose bool, captureDir, networkSocket string, output io.Writer, stop <-chan os.Signal) error {
	b, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Parse(b)
	if err != nil {
		return err
	}

	// -v means decode every packet, at DEBUG; the default level is INFO,
	// which still always shows WARN/ERROR - a refused command or a real
	// failure is never hidden behind -v.
	level := logging.LevelInfo
	if verbose {
		level = logging.LevelDebug
	}
	logger := logging.New(output, level)
	var opts []serve.Option
	if captureDir != "" {
		opts = append(opts, serve.WithCapture(captureDir))
		logger.Infof("recording this run to %s", captureDir)
	}

	// In private-network mode Instigator owns a Unix socket the machine
	// connects to, and serves the segment on that link rather than the host.
	// The socket is created first and readiness is reported before accepting:
	// a machine brought up once Instigator is ready then connects, and only
	// then can serving begin.
	readinessSent := false
	var privateNet *qemunet.Network
	if networkSocket != "" {
		l, err := qemunet.Listen(networkSocket, qemunet.Config{
			ServerIP:  cfg.ServerIP,
			PrefixLen: cfg.Netmask.Bits(),
			MAC:       qemunet.DefaultServerMAC,
		})
		if err != nil {
			return err
		}
		defer l.Close()
		logger.Infof("private network %s: waiting for the machine to connect", networkSocket)
		if _, err := sdnotify.SdNotify(false, sdnotify.SdNotifyReady); err != nil {
			return fmt.Errorf("reporting readiness: %w", err)
		}
		readinessSent = true

		type accepted struct {
			net *qemunet.Network
			err error
		}
		ch := make(chan accepted, 1)
		go func() {
			n, err := l.Accept()
			ch <- accepted{n, err}
		}()
		select {
		case <-stop:
			logger.Infof("shutting down before the machine connected")
			return nil
		case a := <-ch:
			if a.err != nil {
				return fmt.Errorf("accepting the machine: %w", a.err)
			}
			defer a.net.Close()
			privateNet = a.net
			opts = append(opts, serve.WithNetwork(a.net))
		}
	}

	s, err := serve.Start(cfg, logger, opts...)
	if err != nil {
		return err
	}

	logger.Infof("serving; stop with SIGINT/SIGTERM")
	// Whoever set NOTIFY_SOCKET waits forever if this never arrives. In
	// private-network mode it was already sent so the machine could connect.
	if !readinessSent {
		if _, err := sdnotify.SdNotify(false, sdnotify.SdNotifyReady); err != nil {
			s.Close()
			return fmt.Errorf("reporting readiness: %w", err)
		}
	}
	if privateNet != nil {
		select {
		case <-stop:
			logger.Infof("shutting down")
		case <-privateNet.Done():
			return errors.Join(fmt.Errorf("private network disconnected: %w", privateNet.Err()), s.CloseWithReason("disconnected"))
		}
	} else {
		<-stop
		logger.Infof("shutting down")
	}
	// Close drains and finalizes the capture, returning an error if the
	// capture came out incomplete, so a run that cannot be trusted says so.
	return s.Close()
}
