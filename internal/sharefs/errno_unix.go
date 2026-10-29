//go:build linux || darwin

package sharefs

import (
	"syscall"

	"github.com/jamesbraid/instigator/nfs"
)

// errnos maps host errors that fs's own sentinels don't cover.
var errnos = []struct{ errno, err error }{
	{syscall.EPERM, nfs.ErrPerm},
	{syscall.EACCES, nfs.ErrAccess},
	{syscall.EEXIST, nfs.ErrExist},
	{syscall.ENOENT, nfs.ErrNotFound},
	{syscall.ENOTDIR, nfs.ErrNotDir},
	{syscall.EISDIR, nfs.ErrIsDir},
	{syscall.EINVAL, nfs.ErrInval},
	{syscall.ENOSPC, nfs.ErrNoSpace},
	{syscall.EDQUOT, nfs.ErrQuota},
	{syscall.ENAMETOOLONG, nfs.ErrNameTooLong},
	{syscall.ENOTEMPTY, nfs.ErrNotEmpty},
	{syscall.EFBIG, nfs.ErrFBig},
	{syscall.EXDEV, nfs.ErrXDev},
	{syscall.EROFS, nfs.ErrROFS},
}
