// Package instcmd implements instigator's rsh command execution for IRIX
// inst. Run is a constrained command table (dd, ls, echo) for a single
// rsh command line, the shape inst uses outside a shell session. Real
// shell grammar - pipes, subshells, redirects, trap, $? - only shows up
// when inst opens /bin/sh over rsh and streams commands to it; RunShell
// (shell.go) serves that with an actual POSIX interpreter rather than
// pattern-matching the grammar it's expected to send.
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

// FileInfo is a path's metadata: what the rsh shell needs to answer
// ls -l/-i and cd/test's file-type checks, not a full stat(2). A path
// above the real filesystem's own boundary (a synthetic tree level)
// carries plausible placeholder values instead of a real inode's.
type FileInfo struct {
	Ino   uint64
	IsDir bool
	Perm  uint32 // permission bits only, e.g. 0o755
	Nlink int
	UID   uint32
	GID   uint32
	Size  int64
	Mtime time.Time
}

// Run executes one rsh command line against fs, writing command output
// to stdout and diagnostics to stderr. Semicolon-separated segments run
// in order, mirroring the shell inst believes it is talking to.
func Run(fs FileSystem, command string, stdout, stderr io.Writer) error {
	for _, seg := range strings.Split(command, ";") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		if err := runOne(fs, seg, stdout, stderr); err != nil {
			return err
		}
	}
	return nil
}

func runOne(fs FileSystem, seg string, stdout, stderr io.Writer) error {
	args := strings.Fields(seg)
	switch args[0] {
	case "trap":
		// signal handling chatter from inst's shell habits: a no-op here
		return nil
	case "echo":
		rest := strings.TrimSpace(strings.TrimPrefix(seg, "echo"))
		fmt.Fprintln(stdout, unquote(rest))
		return nil
	case "ls":
		return runLs(fs, args[1:], stdout)
	case "dd":
		return runDD(fs, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("%s: command not supported", args[0])
	}
}

func unquote(s string) string {
	s = strings.ReplaceAll(s, `"`, "")
	return strings.ReplaceAll(s, `'`, "")
}

func runLs(fs FileSystem, args []string, stdout io.Writer) error {
	var path string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			path = a
		}
	}
	if path == "" {
		return fmt.Errorf("ls: no path")
	}
	names, err := fs.ReadDir(strings.TrimPrefix(path, "/"))
	if err != nil {
		return fmt.Errorf("ls: %s: %w", path, err)
	}
	for _, n := range names {
		fmt.Fprintln(stdout, n)
	}
	return nil
}

func runDD(fs FileSystem, args []string, stdout, stderr io.Writer) error {
	var (
		file  string
		bs    int64 = 512
		skip  int64
		count int64 = -1
	)
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			return fmt.Errorf("dd: bad operand %q", a)
		}
		switch k {
		case "if":
			file = v
		case "bs":
			n, err := parseSize(v)
			if err != nil {
				return fmt.Errorf("dd: bs: %w", err)
			}
			bs = n
		case "skip":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("dd: skip: %w", err)
			}
			skip = n
		case "count":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("dd: count: %w", err)
			}
			count = n
		default:
			// of=, conv=, ... are refused: this dd only reads
			return fmt.Errorf("dd: operand %q not supported", k)
		}
	}
	if file == "" {
		return fmt.Errorf("dd: no if=")
	}
	f, err := fs.Open(strings.TrimPrefix(file, "/"))
	if err != nil {
		return fmt.Errorf("dd: %s: %w", file, err)
	}

	off := skip * bs
	remain := f.Size() - off
	if remain < 0 {
		remain = 0
	}
	if count >= 0 && count*bs < remain {
		remain = count * bs
	}
	full := remain / bs
	partial := remain % bs

	buf := make([]byte, bs)
	for remain > 0 {
		chunk := int64(len(buf))
		if chunk > remain {
			chunk = remain
		}
		n, err := f.ReadAt(buf[:chunk], off)
		if err != nil && err != io.EOF {
			return fmt.Errorf("dd: read: %w", err)
		}
		if n == 0 {
			break
		}
		if _, err := stdout.Write(buf[:n]); err != nil {
			return err
		}
		off += int64(n)
		remain -= int64(n)
	}
	// dd's records summary on stderr, which inst's invocations expect
	// to see from a real dd
	fmt.Fprintf(stderr, "%d+%d records in\n%d+%d records out\n",
		full, boolToInt(partial > 0), full, boolToInt(partial > 0))
	return nil
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
