package serve

import (
	"errors"
	"os"
	"syscall"
)

// isBindPermission reports whether a listen was refused for lack of privilege:
// EACCES/EPERM (os.ErrPermission) on Unix, or Winsock's WSAEACCES (10013) on
// Windows, which does not satisfy os.ErrPermission.
func isBindPermission(err error) bool {
	return errors.Is(err, os.ErrPermission) || errors.Is(err, syscall.Errno(10013))
}
