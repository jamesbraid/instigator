//go:build unix

package main

import "syscall"

// daemonSysProcAttr detaches the server child into its own session, so a
// signal aimed at the parent's terminal group - the Ctrl-C that follows
// serve --daemon in a script - does not take the server with it. SIGTERM
// sent to the server's own pid shuts it down exactly as before.
func daemonSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
