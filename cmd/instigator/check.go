package main

import (
	"io"
	"os"

	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/serve"
)

// runCheck validates a configuration. Remote media are only probed unless
// full asks for the assembly serve performs at start.
func runCheck(path string, full bool) error {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return err
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return err
	}
	if full {
		return serve.Check(cfg)
	}
	return serve.CheckShallow(cfg)
}
