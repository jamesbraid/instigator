// Command instigator is a network install server for SGI IRIX systems,
// serving install sets assembled from untouched CD images over BOOTP,
// TFTP, and rsh.
package main

import (
	"context"
	"os"
	"runtime/debug"

	"github.com/jamesbraid/instigator/internal/extract"
	"github.com/jamesbraid/instigator/internal/logging"
	"github.com/urfave/cli/v3"
)

func main() {
	root := &cli.Command{
		Name:  "instigator",
		Usage: "network install server for SGI IRIX systems",
		Commands: []*cli.Command{
			{
				Name:      "check",
				Usage:     "validate and assemble install media without serving it",
				Arguments: []cli.Argument{&cli.StringArgs{Name: "config", UsageText: "<config.yaml|->", Min: 1, Max: 1}},
				Action: func(_ context.Context, cmd *cli.Command) error {
					return runCheck(cmd.StringArgs("config")[0])
				},
			},
			{
				Name:      "serve",
				Usage:     "serve the configured IRIX install sets",
				Arguments: []cli.Argument{&cli.StringArgs{Name: "config", UsageText: "<config.yaml>", Min: 1, Max: 1}},
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "decode every packet"},
					&cli.StringFlag{Name: "capture-dir", Usage: "record the run to `DIR`"},
					&cli.StringFlag{Name: "network-socket", Usage: "serve a machine's private Ethernet segment over the QEMU stream socket at `PATH` instead of the host network"},
					&cli.StringFlag{Name: "network-tcp", Usage: "serve a machine's private Ethernet segment over a loopback TCP stream at `IP:PORT`"},
				},
				Action: func(_ context.Context, cmd *cli.Command) error {
					return run(cmd.StringArgs("config")[0], cmd.Bool("verbose"), cmd.String("capture-dir"), cmd.String("network-socket"), cmd.String("network-tcp"))
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
				Name:      "extract",
				Usage:     "write the installed files of disc images, directories or configured install sets to a host directory",
				UsageText: "instigator extract [flags] <image|dir>... <outdir>\n   instigator extract [flags] <config.yaml> <outdir>",
				Arguments: []cli.Argument{
					&cli.StringArgs{Name: "args", UsageText: "<image|dir>... <outdir>", Min: 1, Max: -1},
				},
				Flags: []cli.Flag{
					&cli.StringSliceFlag{Name: "set", Usage: "config mode: extract only the install set `NAME` (repeatable; default all)"},
					&cli.StringFlag{Name: "dist", Usage: "direct mode: the distribution directory `DIR` in every source (default dist; . for the source root)"},
					&cli.StringFlag{Name: "name", Usage: "direct mode: `NAME` of the output set (default: the first source's name without extension)"},
					&cli.StringSliceFlag{Name: "path", Usage: "keep only entries at or under the install path `PATH` (repeatable)"},
					&cli.StringSliceFlag{Name: "subsystem", Usage: "keep only entries of a subsystem matching `GLOB`, such as '*.man.*' (repeatable)"},
					&cli.BoolFlag{Name: "unpack", Usage: "also remove the installed .z (pack) and .Z (compress) layers"},
					&cli.BoolFlag{Name: "manifest-only", Usage: "write only the manifest, planned from the .idb files without reading any archive"},
				},
				Action: func(_ context.Context, cmd *cli.Command) error {
					return extractCommand(cmd.Root().Writer, cmd.StringArgs("args"), cmd.StringSlice("set"), cmd.String("dist"), cmd.String("name"),
						extract.Options{Paths: cmd.StringSlice("path"), Subsystems: cmd.StringSlice("subsystem"),
							Unpack: cmd.Bool("unpack"), ManifestOnly: cmd.Bool("manifest-only")})
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

	if info, ok := debug.ReadBuildInfo(); ok {
		root.Version = info.Main.Version
	}

	if err := root.Run(context.Background(), os.Args); err != nil {
		logging.New(os.Stderr, logging.LevelError).Errorf("instigator: %v", err)
		os.Exit(1)
	}
}
