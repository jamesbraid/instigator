package sharefs

import (
	"io/fs"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const inodesUsable = true

// badNameChars are characters, beyond '/', that a name may not hold.
const badNameChars = ""

func fileStat(fi fs.FileInfo) stat {
	st := genericStat(fi)
	if s, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.mode = s.Mode
		st.nlink = uint32(s.Nlink)
		st.used = uint64(s.Blocks) * 512
		st.atime = time.Unix(s.Atim.Unix())
		st.ctime = time.Unix(s.Ctim.Unix())
		st.dev = uint64(s.Dev)
		st.ino = s.Ino
	}
	return st
}

func sameInode(fi fs.FileInfo, k key) bool {
	s, ok := fi.Sys().(*syscall.Stat_t)
	return ok && uint64(s.Dev) == k.dev && s.Ino == k.ino
}

// sysFS holds a descriptor for the share's directory, so the birth time
// statx reports can be read for paths beneath it.
type sysFS struct {
	rootfd  int
	noBtime *atomic.Bool
}

func openSys(dir string) (sysFS, error) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	return sysFS{rootfd: fd, noBtime: new(atomic.Bool)}, err
}

func (s sysFS) close() error { return unix.Close(s.rootfd) }

// generation is the file's birth time in nanoseconds, or 0 where the
// kernel or filesystem has none. openat2 with RESOLVE_BENEATH confines
// the lookup to the share as os.Root does.
func (s sysFS) generation(rel string, st stat) uint64 {
	if s.noBtime.Load() {
		return 0
	}
	fd, err := unix.Openat2(s.rootfd, rel, &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_NOFOLLOW | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS,
	})
	if err != nil {
		if err == unix.ENOSYS {
			s.noBtime.Store(true) // before Linux 5.6
		}
		return 0
	}
	defer unix.Close(fd)
	var stx unix.Statx_t
	if unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_INO|unix.STATX_BTIME, &stx) != nil ||
		stx.Mask&unix.STATX_BTIME == 0 || stx.Ino != st.ino {
		return 0
	}
	return uint64(stx.Btime.Sec)*1e9 + uint64(stx.Btime.Nsec)
}
