package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	sdnotify "github.com/coreos/go-systemd/v22/daemon"
	"github.com/kardianos/service"

	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/logging"
	"github.com/jamesbraid/instigator/internal/serve"
)

// server is the serve command in the shape a service manager drives.
// Start checks what it can quickly - the configuration and the ports -
// and leaves the rest to a goroutine, because assembling the sets fetches
// the media, minutes for a full release, and a manager expects Start back
// in seconds. Readiness is announced with sd_notify once serving.
type server struct {
	configPath string
	verbose    bool
	captureDir string
	output     io.Writer

	running *serve.Servers
	stopped chan struct{}
	failed  error
}

func (s *server) Start(service.Service) error {
	cfg, err := s.config()
	if err != nil {
		return err
	}
	if err := checkPorts(cfg); err != nil {
		return err
	}
	s.stopped = make(chan struct{})
	go s.serve(cfg)
	return nil
}

// Stop drains and finalizes the capture, reporting an error if it came
// out incomplete, so a run that cannot be trusted says so.
func (s *server) Stop(service.Service) error {
	if s.stopped == nil {
		return nil
	}
	<-s.stopped
	if s.failed != nil {
		return s.failed
	}
	if s.running == nil {
		return nil
	}
	return s.running.Close()
}

func (s *server) config() (*config.Config, error) {
	b, err := os.ReadFile(s.configPath)
	if err != nil {
		return nil, err
	}
	return config.Parse(b)
}

// serve assembles the sets and starts serving. A failure here is kept
// for Stop, the manager having already been told the start succeeded.
func (s *server) serve(cfg *config.Config) {
	defer close(s.stopped)

	// -v means decode every packet, at DEBUG; the default level is INFO,
	// which still always shows WARN/ERROR - a refused command or a real
	// failure is never hidden behind -v.
	level := logging.LevelInfo
	if s.verbose {
		level = logging.LevelDebug
	}
	logger := logging.New(s.output, level)
	var opts []serve.Option
	if s.captureDir != "" {
		opts = append(opts, serve.WithCapture(s.captureDir))
		logger.Infof("recording this run to %s", s.captureDir)
	}
	running, err := serve.Start(cfg, logger, opts...)
	if err != nil {
		logger.Errorf("%v", err)
		s.failed = err
		return
	}
	s.running = running
	logger.Infof("serving")
	// A failure to report readiness leaves a Type=notify unit waiting for
	// its own timeout, so it is an error rather than a warning.
	if _, err := sdnotify.SdNotify(false, sdnotify.SdNotifyReady); err != nil {
		logger.Errorf("reporting readiness to the service manager: %v", err)
	}
}

// checkPorts reports whether the enabled services can bind, the failure
// an operator hits most often after a bad configuration. The listeners
// bind later, so this closes what it opens.
func checkPorts(cfg *config.Config) error {
	for _, p := range []struct {
		enabled bool
		network string
		port    int
		name    string
	}{
		{cfg.Services.BOOTP, "udp4", cfg.Ports.BOOTP, "bootp"},
		{cfg.Services.TFTP.Enabled, "udp4", cfg.Ports.TFTP, "tftp"},
		{cfg.Services.RSH, "tcp4", cfg.Ports.RSH, "rsh"},
	} {
		if !p.enabled {
			continue
		}
		c, err := net.ListenPacket(p.network, fmt.Sprintf(":%d", p.port))
		if p.network == "tcp4" {
			var ln net.Listener
			if ln, err = net.Listen(p.network, fmt.Sprintf(":%d", p.port)); err == nil {
				ln.Close()
			}
		} else if err == nil {
			c.Close()
		}
		if err != nil {
			return fmt.Errorf("%s: %w", p.name, err)
		}
	}
	return nil
}

// managed wraps the server for the platform's service manager.
func managed(s *server) (service.Service, error) {
	return service.New(s, &service.Config{
		Name:        "instigator",
		DisplayName: "instigator IRIX install server",
		Description: "Serves IRIX network installs from SGI CD images over BOOTP, TFTP, and rsh.",
		Arguments:   []string{"serve", s.configPath},
	})
}

// run serves until told to stop. A service manager drives Start and Stop
// itself - the handshake the Windows Service Control Manager requires -
// and a terminal waits for a signal.
func run(configPath string, verbose bool, captureDir string) error {
	s := &server{configPath: configPath, verbose: verbose, captureDir: captureDir, output: os.Stdout}
	if !service.Interactive() {
		svc, err := managed(s)
		if err != nil {
			return err
		}
		return svc.Run()
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	return runUntilSignal(s, sig)
}

// runUntilSignal is run's terminal half, and what the tests drive.
func runUntilSignal(s *server, stop <-chan os.Signal) error {
	if err := s.Start(nil); err != nil {
		return err
	}
	<-stop
	return s.Stop(nil)
}
