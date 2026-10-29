package sharefs

import (
	"io/fs"
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

type sysFS struct{}

func openSys(string) (sysFS, error) { return sysFS{}, nil }

func (sysFS) close() error { return nil }

// generation is the birth time fileStat read.
func (sysFS) generation(_ string, st stat) uint64 { return st.gen }
