package sharefs

import (
	"github.com/jamesbraid/instigator/nfs"
	"golang.org/x/sys/windows"
)

func diskSpace(dir string) (nfs.Space, error) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return nfs.Space{}, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return nfs.Space{}, err
	}
	return nfs.Space{Total: total, Free: free, Avail: avail}, nil
}
