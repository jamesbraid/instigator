//go:build windows

package rcmd

import (
	"errors"
	"os"
	"syscall"
)

// Winsock error codes (winerror.h). Go's stdlib syscall package does not export
// these as named constants on Windows, and they surface with different numeric
// values than the POSIX errnos, so match them explicitly. There is no WSAEPERM;
// a denied bind is WSAEACCES, which also maps to os.ErrPermission.
const (
	wsaeAccess    = syscall.Errno(10013) // WSAEACCES
	wsaeAddrInUse = syscall.Errno(10048) // WSAEADDRINUSE
)

func isAddrInUse(err error) bool { return errors.Is(err, wsaeAddrInUse) }

func isPermission(err error) bool {
	return errors.Is(err, wsaeAccess) || errors.Is(err, os.ErrPermission)
}
