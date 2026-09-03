// Command instigator is a network install server for SGI IRIX systems,
// serving install sets assembled from untouched CD images over BOOTP,
// TFTP, and rsh.
package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/logging"
	"github.com/jamesbraid/instigator/internal/serve"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  instigator serve [-v] [--capture-dir <dir>] <config.yaml>
                                          serve the configured IRIX install sets
                                          (-v: decode every packet; --capture-dir: record the run)
  instigator trace summary <capture-dir>  summarize a recorded run (regenerates summary.json)
  instigator ls <image> [path]            list an SGI CD image (volume header + EFS)
  instigator dump <image> <src> <outdir>  extract an EFS subtree to a host directory`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		args := os.Args[2:]
		verbose := false
		captureDir := ""
		for len(args) > 0 && strings.HasPrefix(args[0], "-") {
			switch {
			case args[0] == "-v" || args[0] == "--verbose":
				verbose = true
				args = args[1:]
			case args[0] == "--capture-dir":
				if len(args) < 2 {
					usage()
				}
				captureDir = args[1]
				args = args[2:]
			case strings.HasPrefix(args[0], "--capture-dir="):
				captureDir = strings.TrimPrefix(args[0], "--capture-dir=")
				args = args[1:]
			default:
				usage()
			}
		}
		if len(args) != 1 {
			usage()
		}
		if err := runServe(args[0], verbose, captureDir); err != nil {
			fmt.Fprintln(os.Stderr, "instigator:", err)
			os.Exit(1)
		}
	case "trace":
		if len(os.Args) != 4 || os.Args[2] != "summary" {
			usage()
		}
		if err := runTraceSummary(os.Stdout, os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, "instigator:", err)
			os.Exit(1)
		}
	case "ls":
		if len(os.Args) < 3 || len(os.Args) > 4 {
			usage()
		}
		path := "/"
		if len(os.Args) == 4 {
			path = os.Args[3]
		}
		if err := runLs(os.Stdout, os.Args[2], path); err != nil {
			fmt.Fprintln(os.Stderr, "instigator:", err)
			os.Exit(1)
		}
	case "dump":
		if len(os.Args) != 5 {
			usage()
		}
		if err := runDump(os.Stdout, os.Args[2], os.Args[3], os.Args[4]); err != nil {
			fmt.Fprintln(os.Stderr, "instigator:", err)
			os.Exit(1)
		}
	default:
		usage()
	}
}

func runServe(configPath string, verbose bool, captureDir string) error {
	b, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Parse(b)
	if err != nil {
		return err
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	return serveUntilSignal(cfg, verbose, captureDir, os.Stdout, sig)
}

func serveUntilSignal(
	cfg *config.Config,
	verbose bool,
	captureDir string,
	output io.Writer,
	stop <-chan os.Signal,
) error {
	// -v means decode every packet, at DEBUG; the default level is
	// INFO, which still always shows WARN/ERROR - a refused command or
	// a real failure is never hidden behind -v.
	level := logging.LevelInfo
	if verbose {
		level = logging.LevelDebug
	}
	logger := logging.New(output, level)
	opts := []serve.Option{}
	if captureDir != "" {
		opts = append(opts, serve.WithCapture(captureDir))
		logger.Infof("recording this run to %s", captureDir)
	}
	s, err := serve.Start(cfg, logger, opts...)
	if err != nil {
		return err
	}

	logger.Infof("serving; stop with SIGINT/SIGTERM")
	<-stop
	logger.Infof("shutting down")
	// Close drains, finalizes the capture, and returns a non-nil error if
	// the capture came out incomplete or a recorder write failed - surface
	// it so the run exits non-zero and the operator knows not to trust it.
	return s.Close()
}
