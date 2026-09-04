package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	sdnotify "github.com/coreos/go-systemd/v22/daemon"

	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/logging"
	"github.com/jamesbraid/instigator/internal/serve"
)

// run serves until a signal stops it. The log goes to stderr because a
// caller waiting on readiness may have closed stdout, as
// systemd-notify --fork does.
func run(configPath string, verbose bool, captureDir string) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	return runUntilSignal(configPath, verbose, captureDir, os.Stderr, sig)
}

func runUntilSignal(configPath string, verbose bool, captureDir string, output io.Writer, stop <-chan os.Signal) error {
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
	s, err := serve.Start(cfg, logger, opts...)
	if err != nil {
		return err
	}

	logger.Infof("serving; stop with SIGINT/SIGTERM")
	// Whoever set NOTIFY_SOCKET waits forever if this never arrives.
	if _, err := sdnotify.SdNotify(false, sdnotify.SdNotifyReady); err != nil {
		s.Close()
		return fmt.Errorf("reporting readiness: %w", err)
	}
	<-stop
	logger.Infof("shutting down")
	// Close drains and finalizes the capture, returning an error if the
	// capture came out incomplete, so a run that cannot be trusted says so.
	return s.Close()
}
