package main

import "syscall"

// daemonSysProcAttr: Windows has no sessions to detach into; the child is
// an ordinary process and console signal handling stays as exec sets it.
func daemonSysProcAttr() *syscall.SysProcAttr {
	return nil
}
