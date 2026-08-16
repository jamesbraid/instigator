// Command instigator is a network install server for SGI IRIX systems,
// serving untouched CD images over BOOTP, TFTP, and rsh.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/logging"
	"github.com/jamesbraid/instigator/internal/serve"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  instigator serve [-v] <config.yaml>     serve IRIX netinstalls from CD images (-v: decode every packet)
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
		if len(args) > 0 && (args[0] == "-v" || args[0] == "--verbose") {
			verbose = true
			args = args[1:]
		}
		if len(args) != 1 {
			usage()
		}
		if err := runServe(args[0], verbose); err != nil {
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

func runServe(configPath string, verbose bool) error {
	b, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Parse(b)
	if err != nil {
		return err
	}
	// -v means decode every packet, at DEBUG; the default level is
	// INFO, which still always shows WARN/ERROR - a refused command or
	// a real failure is never hidden behind -v.
	level := logging.LevelInfo
	if verbose {
		level = logging.LevelDebug
	}
	logger := logging.New(os.Stdout, level)
	// The operator's PROM/Inst> commands are console UX, not server log
	// content, so they go to stdout directly rather than through the
	// leveled logger - see serve.WithInstructions.
	s, err := serve.Start(cfg, logger, serve.WithInstructions(os.Stdout))
	if err != nil {
		return err
	}
	defer s.Close()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	logger.Infof("serving; stop with SIGINT/SIGTERM")
	<-sig
	logger.Infof("shutting down")
	return nil
}
