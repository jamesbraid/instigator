// Command instigator is a network install server for SGI IRIX systems,
// serving untouched CD images over BOOTP, TFTP, and rsh.
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/serve"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  instigator serve <config.yaml>   serve IRIX netinstalls from CD images
  instigator ls <image> [path]     list an SGI CD image (volume header + EFS)`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "serve":
		if len(os.Args) != 3 {
			usage()
		}
		if err := runServe(os.Args[2]); err != nil {
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
	default:
		usage()
	}
}

func runServe(configPath string) error {
	b, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	cfg, err := config.Parse(b)
	if err != nil {
		return err
	}
	logger := log.New(os.Stdout, "", log.LstdFlags)
	s, err := serve.Start(cfg, logger.Printf)
	if err != nil {
		return err
	}
	defer s.Close()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	logger.Printf("serving; stop with SIGINT/SIGTERM")
	<-sig
	logger.Printf("shutting down")
	return nil
}
