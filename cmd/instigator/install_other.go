//go:build !windows

package main

import "github.com/urfave/cli/v3"

// installCommands: only Windows needs a registration command. See the
// installation guide for the systemd unit and launchd plist.
func installCommands() []*cli.Command { return nil }
