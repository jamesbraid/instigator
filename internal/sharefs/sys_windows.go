package sharefs

import (
	"io/fs"
	"syscall"
	"time"
)

// Windows has no inode numbers os.Lstat reports, so a share keeps a
// path table.
const inodesUsable = false

// badNameChars are characters, beyond '/', that a name may not hold:
// '\' is a separator here too, and ':' names an alternate data stream.
const badNameChars = `\:`

func fileStat(fi fs.FileInfo) stat {
	st := genericStat(fi)
	if d, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		st.atime = time.Unix(0, d.LastAccessTime.Nanoseconds())
	}
	return st
}

func sameInode(fs.FileInfo, key) bool { return false }

type sysFS struct{}

func openSys(string) (sysFS, error) { return sysFS{}, nil }

func (sysFS) close() error { return nil }

func (sysFS) generation(string, stat) uint64 { return 0 }
