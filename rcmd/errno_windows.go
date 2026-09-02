//go:build windows

package rcmd

import (
	"errors"
	"os"
	"syscall"
)

// Winsock bind errors.
const (
	wsaeAccess    = syscall.Errno(10013) // WSAEACCES
	wsaeAddrInUse = syscall.Errno(10048) // WSAEADDRINUSE
)

func isAddrInUse(err error) bool { return errors.Is(err, wsaeAddrInUse) }

func isPermission(err error) bool {
	return errors.Is(err, wsaeAccess) || errors.Is(err, os.ErrPermission)
}
