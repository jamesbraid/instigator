package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	sdnotify "github.com/coreos/go-systemd/v22/daemon"

	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/logging"
	"github.com/jamesbraid/instigator/internal/qemunet"
	"github.com/jamesbraid/instigator/internal/serve"
	"github.com/jamesbraid/instigator/nfs"
)

// run serves until a signal stops it. The log goes to stderr because a
// caller waiting on readiness may have closed stdout, as
// systemd-notify --fork does.
func run(configPath string, verbose bool, captureDir, networkSocket, networkTCP string, shares []string) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	return runUntilSignal(configPath, verbose, captureDir, networkSocket, networkTCP, shares, os.Stderr, sig)
}

func runUntilSignal(configPath string, verbose bool, captureDir, networkSocket, networkTCP string, shares []string, output io.Writer, stop <-chan os.Signal) error {
	endpoint := privateEndpoint{socket: networkSocket, tcp: networkTCP}
	if err := endpoint.check(); err != nil {
		return err
	}
	shareSpecs, err := parseShareSpecs(shares)
	if err != nil {
		return err
	}
	if len(shareSpecs) > 0 && !endpoint.enabled() {
		return fmt.Errorf("--share needs --network-socket or --network-tcp: shares are served on the private network (instigator share serves host sockets)")
	}
	b, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	cfg, err := config.ParseIn(b, filepath.Dir(configPath))
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

	// In private-network mode Instigator owns a stream endpoint the machine
	// connects to, and serves the segment on that link rather than the host.
	// The endpoint is created first and readiness is reported before accepting:
	// a machine brought up once Instigator is ready then connects, and only
	// then can serving begin.
	readinessSent := false
	var privateNet *qemunet.Network
	var shareErr <-chan error // nil, and so never ready, without shares
	if endpoint.enabled() {
		networkCfg := qemunet.Config{
			ServerIP:  cfg.ServerIP,
			PrefixLen: cfg.Netmask.Bits(),
			MAC:       qemunet.DefaultServerMAC,
		}
		// Open the shares before announcing readiness, so a bad directory
		// fails before the machine is started.
		var shareFS nfs.FS
		var shareDesc string
		if len(shareSpecs) > 0 {
			var closer io.Closer
			shareFS, closer, shareDesc, err = openShares(shareSpecs, "")
			if err != nil {
				return err
			}
			defer closer.Close()
		}
		l, err := endpoint.listen(networkCfg)
		if err != nil {
			return err
		}
		defer l.Close()
		logger.Infof("private network %s: waiting for the machine to connect", l.Addr())
		if _, err := sdnotify.SdNotify(false, sdnotify.SdNotifyReady); err != nil {
			return fmt.Errorf("reporting readiness: %w", err)
		}
		readinessSent = true

		n, err := acceptNetwork(l, stop)
		if err != nil {
			return err
		}
		if n == nil {
			logger.Infof("shutting down before the machine connected")
			return nil
		}
		defer n.Close()
		privateNet = n
		opts = append(opts, serve.WithNetwork(n))
		if shareFS != nil {
			p, err := serveSharesOn(n, shareFS, logger)
			if err != nil {
				return err
			}
			defer p.Close()
			shareErr = p.Err()
			logger.Infof("share: %s on %s over the private network, portmap, MOUNT and NFS on ports %d, %d and %d over udp and tcp",
				shareDesc, cfg.ServerIP, privatePortmapPort, privateMountPort, privateNFSPort)
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
		case err := <-shareErr:
			return errors.Join(err, s.Close())
		}
	} else {
		<-stop
		logger.Infof("shutting down")
	}
	// Close drains and finalizes the capture, returning an error if the
	// capture came out incomplete, so a run that cannot be trusted says so.
	return s.Close()
}
