// Command instigator is a network install server for SGI IRIX systems,
// serving install sets assembled from untouched CD images over BOOTP,
// TFTP, and rsh.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/urfave/cli/v3"
)

func main() {
	root := &cli.Command{
		Name:  "instigator",
		Usage: "network install server for SGI IRIX systems",
		Commands: []*cli.Command{
			{
				Name:      "serve",
				Usage:     "serve the configured IRIX install sets",
				Arguments: []cli.Argument{&cli.StringArgs{Name: "config", UsageText: "<config.yaml>", Min: 1, Max: 1}},
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "decode every packet"},
					&cli.StringFlag{Name: "capture-dir", Usage: "record the run to `DIR`"},
				},
				Action: func(_ context.Context, cmd *cli.Command) error {
					return run(cmd.StringArgs("config")[0], cmd.Bool("verbose"), cmd.String("capture-dir"))
				},
			},
			{
				Name:  "trace",
				Usage: "inspect a recorded run",
				Commands: []*cli.Command{
					{
						Name:      "summary",
						Usage:     "summarize a recorded run, regenerating summary.json",
						Arguments: []cli.Argument{&cli.StringArgs{Name: "capture-dir", Min: 1, Max: 1}},
						Action: func(_ context.Context, cmd *cli.Command) error {
							return runTraceSummary(cmd.Root().Writer, cmd.StringArgs("capture-dir")[0])
						},
					},
				},
			},
			{
				Name:  "ls",
				Usage: "list an SGI CD image (volume header + EFS)",
				Arguments: []cli.Argument{
					&cli.StringArgs{Name: "image", Min: 1, Max: 1},
					&cli.StringArgs{Name: "path", Min: 0, Max: 1},
				},
				Action: func(_ context.Context, cmd *cli.Command) error {
					// The path is optional and defaults to the image root.
					path := "/"
					if given := cmd.StringArgs("path"); len(given) > 0 {
						path = given[0]
					}
					return runLs(cmd.Root().Writer, cmd.StringArgs("image")[0], path)
				},
			},
			{
				Name:  "dump",
				Usage: "extract an EFS subtree to a host directory",
				Arguments: []cli.Argument{
					&cli.StringArgs{Name: "image", Min: 1, Max: 1},
					&cli.StringArgs{Name: "src", Min: 1, Max: 1},
					&cli.StringArgs{Name: "outdir", Min: 1, Max: 1},
				},
				Action: func(_ context.Context, cmd *cli.Command) error {
					return runDump(cmd.Root().Writer, cmd.StringArgs("image")[0], cmd.StringArgs("src")[0], cmd.StringArgs("outdir")[0])
				},
			},
		},
		Writer:    os.Stdout,
		ErrWriter: os.Stderr,
	}

	if err := root.Run(context.Background(), os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "instigator:", err)
		os.Exit(1)
	}
}
