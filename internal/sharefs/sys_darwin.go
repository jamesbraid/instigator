package sharefs

import (
	"io/fs"
	"os"
	"syscall"
	"time"
)

const inodesUsable = true

// badNameChars are characters, beyond '/', that a name may not hold.
const badNameChars = ""

func fileStat(fi fs.FileInfo) stat {
	st := genericStat(fi)
	if s, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.mode = uint32(s.Mode)
		st.nlink = uint32(s.Nlink)
		st.used = uint64(s.Blocks) * 512
		st.atime = time.Unix(s.Atimespec.Unix())
		st.ctime = time.Unix(s.Ctimespec.Unix())
		st.dev = uint64(s.Dev)
		st.ino = s.Ino
		st.gen = uint64(s.Birthtimespec.Nano())
	}
	return st
}

func sameInode(fi fs.FileInfo, k key) bool {
	s, ok := fi.Sys().(*syscall.Stat_t)
	return ok && uint64(s.Dev) == k.dev && s.Ino == k.ino
}

type sysFS struct{ birth bool }

func openSys(dir string) (sysFS, error) {
	fi, err := os.Lstat(dir)
	if err != nil {
		return sysFS{}, err
	}
	return sysFS{birth: fileStat(fi).gen != 0}, nil
}

// hasBirthTime reports whether the share's filesystem keeps birth times,
// judged by its root directory.
func (s sysFS) hasBirthTime() bool { return s.birth }

func (sysFS) close() error { return nil }

// generation is the birth time fileStat read.
func (sysFS) generation(_ string, st stat) uint64 { return st.gen }
