// Package instcmd implements the constrained command table instigator's
// rshd exposes to IRIX inst. The vocabulary - dd, ls, echo, and a
// leading trap to strip - is what inst is known to issue over rsh (the
// love netboot server implements exactly this set); the first live run
// logs every command verbatim so the table can be corrected from
// observation. Nothing here is a shell: unknown commands are refused.
package instcmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
)

// ErrNotFound is returned by a FileSystem for missing paths.
var ErrNotFound = errors.New("not found")

// FileSystem is the tree commands read from.
type FileSystem interface {
	Open(path string) (File, error)
	ReadDir(path string) ([]string, error)
}

// File is an open, random-access file.
type File interface {
	io.ReaderAt
	Size() int64
}

// RunShell serves the shell inst opens over rsh with "exec /bin/sh": it
// reads a command per line from stdin and runs each, so inst can pipe its
// whole command stream through one connection. log, when set, records
// every line for observation while the command table is still being
// fitted to what inst actually sends. A command that fails writes its
// error to stderr and the shell keeps going, as a real shell does.
func RunShell(fs FileSystem, stdin io.Reader, stdout, stderr io.Writer, log func(string)) error {
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	lastStatus := 0
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if log != nil {
			log(line)
		}
		// inst brackets each command with a marker wrapper: it echoes a
		// unique token (no newline) to stdout and the same token plus the
		// command's exit status to stderr, so it can find the end of the
		// output and read $?. Honour that instead of running the wrapper's
		// shell grammar (subshells, traps, redirects).
		if marker, prefix, ok := splitMarker(line); ok {
			if strings.TrimSpace(prefix) != "" {
				lastStatus = runReport(fs, prefix, stdout, stderr)
			}
			fmt.Fprint(stdout, marker)
			fmt.Fprintf(stderr, "%s%d", marker, lastStatus)
			continue
		}
		lastStatus = runReport(fs, line, stdout, stderr)
	}
	return sc.Err()
}

// runReport runs a command, writes any error to stderr as a shell would,
// and returns the exit status inst reads through its marker.
func runReport(fs FileSystem, cmd string, stdout, stderr io.Writer) int {
	if err := Run(fs, cmd, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	return 0
}

// markerEcho matches the token in inst's `echo 'TOKEN\c'`.
var markerEcho = regexp.MustCompile(`echo '([^']*)\\c'`)

// splitMarker recognises inst's per-command marker wrapper. It returns the
// marker token and the real command that precedes it (empty for the first
// probe), or ok=false when the line is an ordinary command.
func splitMarker(line string) (marker, prefix string, ok bool) {
	i := strings.Index(line, "trap : 2")
	if i < 0 || !strings.Contains(line, "IsDone") || !strings.Contains(line, "1>&2") {
		return "", "", false
	}
	m := markerEcho.FindStringSubmatch(line[i:])
	if m == nil {
		return "", "", false
	}
	prefix = strings.TrimRight(strings.TrimSpace(line[:i]), ";")
	return m[1], prefix, true
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
