//go:build linux || darwin

package sharefs

import (
	"github.com/jamesbraid/instigator/nfs"
	"golang.org/x/sys/unix"
)

func diskSpace(dir string) (nfs.Space, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return nfs.Space{}, err
	}
	bs := uint64(st.Bsize)
	return nfs.Space{
		Total: st.Blocks * bs, Free: st.Bfree * bs, Avail: st.Bavail * bs,
		Files: st.Files, FreeFiles: st.Ffree, AvailFiles: st.Ffree,
	}, nil
}
