// Package instcmd executes the shell session IRIX inst opens over rsh.
// inst runs "exec /bin/sh" and streams real shell grammar to it - pipes,
// subshells, trap, $? - so RunShell (shell.go) serves an actual POSIX
// interpreter wired to a constrained read-only command set rather than
// pattern-matching the grammar inst is expected to send.
package instcmd

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned by a FileSystem for missing paths.
var ErrNotFound = errors.New("not found")

// FileSystem is the tree commands read from.
type FileSystem interface {
	Open(path string) (File, error)
	ReadDir(path string) ([]string, error)
	Stat(path string) (FileInfo, error)
}

// File is an open, random-access file.
type File interface {
	io.ReaderAt
	Size() int64
}

// Resolved is what ImageResolver reports: Image is the backing image's
// filename, Path is the location within that image's own filesystem.
type Resolved struct {
	Image string
	Path  string
}

// ImageResolver is optionally implemented by a FileSystem that can say
// which image file and in-image path a served path came from, for the
// steady-state served-file manifest line dd and cat emit at INFO. A
// FileSystem that doesn't back onto named media (a test double, say)
// need not implement it; a leaf command that wants it type-asserts and
// only logs the manifest line when it can.
type ImageResolver interface {
	ResolveImage(path string) (Resolved, error)
}

// FileInfo is a path's metadata: what the rsh shell needs to answer
// ls -l/-i and cd/test's file-type checks, not a full stat(2). A path
// above the real filesystem's own boundary (a synthetic tree level)
// carries plausible placeholder values instead of a real inode's.
type FileInfo struct {
	Ino   uint64
	IsDir bool
	Perm  uint32 // permission bits incl. setuid/setgid/sticky, e.g. 0o4755
	Nlink int
	UID   uint32
	GID   uint32
	Size  int64
	Mtime time.Time
}

func parseSize(s string) (int64, error) {
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "k"):
		mult, s = 1024, s[:len(s)-1]
	case strings.HasSuffix(s, "b"):
		mult, s = 512, s[:len(s)-1]
	case strings.HasSuffix(s, "w"):
		mult, s = 2, s[:len(s)-1]
	case strings.HasSuffix(s, "m"):
		mult, s = 1024*1024, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	return n * mult, nil
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
