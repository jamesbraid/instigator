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

// newRoot builds the command tree.
func newRoot() *cli.Command {
	root := &cli.Command{
		Name:  "instigator",
		Usage: "network install server for SGI IRIX systems",
		Commands: []*cli.Command{
			{
				Name:      "check",
				Usage:     "validate install media without serving it",
				Arguments: []cli.Argument{&cli.StringArgs{Name: "config", UsageText: "<config.yaml|->", Min: 1, Max: 1}},
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "full", Usage: "also read remote media and assemble every set, as serve does"},
				},
				Action: func(_ context.Context, cmd *cli.Command) error {
					return runCheck(cmd.StringArgs("config")[0], cmd.Bool("full"))
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
					&cli.StringSliceFlag{Name: "share", Usage: "also export a host directory over NFS on the private network, as `NAME=DIR` (repeatable; needs --network-socket or --network-tcp)"},
				},
				Action: func(_ context.Context, cmd *cli.Command) error {
					return run(cmd.StringArgs("config")[0], cmd.Bool("verbose"), cmd.String("capture-dir"), cmd.String("network-socket"), cmd.String("network-tcp"), cmd.StringSlice("share"))
				},
			},
			{
				Name:      "share",
				Usage:     "export host directories, read-write, to IRIX over NFS",
				UsageText: "instigator share [flags] <name> <dir>\n   instigator share [flags] --share NAME=DIR...",
				Description: "Serves each directory as the export /<name> over NFSv2 and NFSv3, with MOUNT and\n" +
					"portmap, until interrupted. Every file shows as owned by root, and changes land\n" +
					"owned by the user running instigator.\n\n" +
					"With --network-socket or --network-tcp the shares are served on a machine's\n" +
					"private Ethernet segment instead of host sockets, at the server address on ports\n" +
					"111, 635 and 2049, with no install services. --client MAC=IP[,HOSTNAME] also gives\n" +
					"that machine its address by BOOTP and DHCP.",
				Arguments: []cli.Argument{
					&cli.StringArgs{Name: "name", Min: 0, Max: 1},
					&cli.StringArgs{Name: "dir", Min: 0, Max: 1},
				},
				Flags: []cli.Flag{
					&cli.StringSliceFlag{Name: "share", Usage: "export a host directory as /NAME, as `NAME=DIR` (repeatable; instead of or beside <name> <dir>)"},
					&cli.StringSliceFlag{Name: "listen", Value: []string{"127.0.0.1"}, Usage: "listen on `ADDR` (repeatable)"},
					&cli.IntFlag{Name: "port", Value: 2049, Usage: "serve NFS and MOUNT on `PORT`; 0 picks a free one"},
					&cli.IntFlag{Name: "portmap-port", Value: 111, Usage: "serve portmap on `PORT`; 0 serves none"},
					&cli.BoolFlag{Name: "udp", Value: true, Usage: "serve over UDP (--udp=false turns it off)"},
					&cli.BoolFlag{Name: "tcp", Value: true, Usage: "serve over TCP (--tcp=false turns it off)"},
					&cli.StringFlag{Name: "network-socket", Usage: "serve on a machine's private Ethernet segment over the QEMU stream socket at `PATH`"},
					&cli.StringFlag{Name: "network-tcp", Usage: "serve on a machine's private Ethernet segment over a loopback TCP stream at `IP:PORT`"},
					&cli.StringFlag{Name: "address", Value: "10.98.0.2/24", Usage: "the server's `ADDR/PREFIX` on the private network"},
					&cli.StringFlag{Name: "dhcp-server-id", Usage: "send `ADDR` as the DHCP server identifier instead of the --address host (needs --client)"},
					&cli.StringSliceFlag{Name: "client", Usage: "answer BOOTP and DHCP on the private network for the machine `MAC=IP[,HOSTNAME]` (repeatable; the IP defaults to 10.98.0.65 for a single client; needs --network-socket or --network-tcp)"},
					&cli.StringFlag{Name: "handle-table", Usage: "keep filehandles in a table of paths in `FILE` instead of deriving them from inodes (default on Windows, and for a share on a filesystem with no file birth times, such as an NFS mount: in the user cache directory)"},
					&cli.BoolFlag{Name: "verbose", Aliases: []string{"v"}, Usage: "log every RPC call"},
				},
				Action: func(_ context.Context, cmd *cli.Command) error {
					o, err := shareOptionsFromCommand(cmd)
					if err != nil {
						return err
					}
					return runShare(o, cmd.Root().Writer)
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

	// Repeatable flags take one value each: a comma belongs to the value,
	// as in --client MAC=IP,HOSTNAME. cli applies each command's own
	// setting to its flags, so set it on all of them.
	var noSplit func(*cli.Command)
	noSplit = func(c *cli.Command) {
		c.DisableSliceFlagSeparator = true
		for _, sub := range c.Commands {
			noSplit(sub)
		}
	}
	noSplit(root)
	return root
}

func main() {
	if err := newRoot().Run(context.Background(), os.Args); err != nil {
		logging.New(os.Stderr, logging.LevelError).Errorf("instigator: %v", err)
		os.Exit(1)
	}
}
