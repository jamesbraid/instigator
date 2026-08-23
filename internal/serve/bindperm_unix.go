//go:build unix

package serve

import (
	"errors"
	"os"
)

// isBindPermission reports whether a listen was refused for lack of privilege.
// On Unix that is EACCES or EPERM, both of which satisfy os.ErrPermission.
func isBindPermission(err error) bool {
	return errors.Is(err, os.ErrPermission)
}
