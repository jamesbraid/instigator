//go:build windows

package serve

import (
	"errors"
	"os"
	"syscall"
)

// isBindPermission reports whether a listen was refused for lack of privilege.
// Winsock reports a denied bind as WSAEACCES (10013), which Go's syscall
// package does not export by name and which does not satisfy os.ErrPermission,
// so match it explicitly.
func isBindPermission(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.Errno(10013))
}
