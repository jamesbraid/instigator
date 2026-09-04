package main

import (
	"context"
	"path/filepath"

	"github.com/urfave/cli/v3"
)

// installCommands register the binary with the Service Control Manager,
// the only way to run a background service on Windows. systemd and launchd
// hosts write a unit or plist instead; the installation guide has both.
func installCommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:      "install",
			Usage:     "register as a Windows service serving the given config",
			Arguments: []cli.Argument{&cli.StringArgs{Name: "config", Min: 1, Max: 1}},
			Action: func(_ context.Context, cmd *cli.Command) error {
				abs, err := filepath.Abs(cmd.StringArgs("config")[0])
				if err != nil {
					return err
				}
				svc, err := managed(&server{configPath: abs, output: cmd.Root().Writer})
				if err != nil {
					return err
				}
				return svc.Install()
			},
		},
		{
			Name:  "uninstall",
			Usage: "remove the Windows service registration",
			Action: func(_ context.Context, cmd *cli.Command) error {
				svc, err := managed(&server{output: cmd.Root().Writer})
				if err != nil {
					return err
				}
				return svc.Uninstall()
			},
		},
	}
}
