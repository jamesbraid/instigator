package sharefs

import (
	"github.com/jamesbraid/instigator/nfs"
	"golang.org/x/sys/windows"
)

// errnos maps host errors that fs's own sentinels don't cover.
var errnos = []struct{ errno, err error }{
	{windows.ERROR_DIR_NOT_EMPTY, nfs.ErrNotEmpty},
	{windows.ERROR_DISK_FULL, nfs.ErrNoSpace},
	{windows.ERROR_HANDLE_DISK_FULL, nfs.ErrNoSpace},
	{windows.ERROR_FILENAME_EXCED_RANGE, nfs.ErrNameTooLong},
	{windows.ERROR_DIRECTORY, nfs.ErrNotDir},
	{windows.ERROR_NOT_SAME_DEVICE, nfs.ErrXDev},
	{windows.ERROR_INVALID_NAME, nfs.ErrInval},
}
