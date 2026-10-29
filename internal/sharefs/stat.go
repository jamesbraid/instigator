package sharefs

import "io/fs"

// genericStat describes a file from what fs.FileInfo alone carries; the
// platform's fileStat fills in the rest where it can.
func genericStat(fi fs.FileInfo) stat {
	m := fi.Mode()
	mode := uint32(m.Perm())
	if m&fs.ModeSetgid != 0 {
		mode |= 0o2000
	}
	if m&fs.ModeSticky != 0 {
		mode |= 0o1000
	}
	switch {
	case m.IsDir():
		mode |= 0o040000
	case m&fs.ModeSymlink != 0:
		mode |= 0o120000
	default:
		mode |= 0o100000
	}
	size := uint64(max(fi.Size(), 0))
	return stat{
		mode:  mode,
		nlink: 1,
		size:  size,
		used:  (size + 4095) &^ 4095,
		atime: fi.ModTime(),
		mtime: fi.ModTime(),
		ctime: fi.ModTime(),
	}
}
