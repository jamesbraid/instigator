// This file replaces the old regex marker-matching in RunShell with a real
// POSIX shell, mvdan.cc/sh/v3: IRIX inst drives its install by opening
// /bin/sh over rsh and streaming real shell grammar to it - pipes,
// subshells, redirects, trap, $? - which a hand-rolled command table can
// only ever whack-a-mole. The interpreter owns all of that grammar; this
// file supplies the small, vfs-backed set of leaf commands (dd, ls, cat)
// and file access it's allowed to reach, so the shell is real but the
// filesystem underneath it is still exactly the narrow, read-only vfs
// tree the rest of instigator serves.
package instcmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// errReadOnly is returned by the open handler for any write attempt.
var errReadOnly = errors.New("read-only filesystem")

// shellEnv is the state shared by every handler for one rsh shell
// session: FileSystem access plus the tracked working directory. cd is
// entirely hand-rolled (see callHandler's "cd" case, never mvdan/sh's
// own builtin - see its comment for why), so every handler resolves a
// path against this cwd itself rather than against the interpreter's
// own Dir, which this shell never updates.
type shellEnv struct {
	fsys FileSystem
	cwd  string // always an absolute, path.Clean'd vfs path
}

func newShellEnv(fsys FileSystem) *shellEnv {
	return &shellEnv{fsys: fsys, cwd: "/"}
}

// resolve turns a possibly-relative path into an absolute vfs path
// against cwd, collapsing "." and ".." lexically. That's the "path"
// package, not "path/filepath": vfs paths are always forward-slash,
// never host paths, so there's no OS-dependent separator to account
// for. A ".." that would climb above a disc's own root behaves as
// ordinary lexical collapsing here, not the real EFS filesystem's
// self-referential root entry (see internal/vfs's own ".." handling
// within a disc) - a difference that never shows up in inst's actual
// usage, which only cds to an absolute distribution path and reads
// forward from there.
func (e *shellEnv) resolve(p string) string {
	if p == "" {
		p = "."
	}
	if !strings.HasPrefix(p, "/") {
		p = e.cwd + "/" + p
	}
	return path.Clean(p)
}

// fsPath resolves p against cwd and strips the leading slash every
// FileSystem implementation in this codebase expects.
func (e *shellEnv) fsPath(p string) string {
	return strings.TrimPrefix(e.resolve(p), "/")
}

// baseName returns the last path component of a command name, so
// /usr/5bin/ls and /bin/ls are recognized the same as ls: inst invokes
// ls from more than one absolute path.
func baseName(cmd string) string {
	if i := strings.LastIndex(cmd, "/"); i >= 0 {
		return cmd[i+1:]
	}
	return cmd
}

// RunShell serves the shell inst opens over rsh with "exec /bin/sh": one
// mvdan/sh Runner for the life of the connection, fed one line at a time
// so state (variables, trap registrations, $?) persists across lines the
// way a real shell's does. log, when set, records every line verbatim,
// matching what the old table did for observability. A command that
// fails writes its own diagnostic to stderr and the shell keeps going -
// a real shell continues past a ';'-separated failure - matching inst's
// expectation that the trailing marker wrapper always still runs.
func RunShell(fsys FileSystem, stdin io.Reader, stdout, stderr io.Writer, log func(string)) error {
	env := newShellEnv(fsys)
	runner, err := interp.New(
		interp.StdIO(nil, stdout, stderr),
		interp.Env(expand.ListEnviron("PATH=", "HOME=/", "IFS= \t\n")),
		interp.Dir("/"),
		interp.CallHandler(callHandler(env)),
		interp.ExecHandler(execHandler(env)),
		interp.OpenHandler(openHandler(env)),
		interp.ReadDirHandler2(readDirHandler(env)),
		interp.StatHandler(statHandler(env)),
	)
	if err != nil {
		return fmt.Errorf("instcmd: shell setup: %w", err)
	}

	// LangPOSIX, not the default LangBash: it's the closer behavioral
	// match for IRIX's actual /bin/sh, and it structurally rules out
	// bash-only grammar we never want to support here - notably process
	// substitution (<(...)), which upstream's own subshell plumbing
	// backs with a real mkfifo() on the host's temp directory. Under
	// LangPOSIX the lexer never tokenizes <( as process substitution at
	// all, so that host-filesystem write is unreachable, not just
	// unused.
	parser := syntax.NewParser(syntax.Variant(syntax.LangPOSIX))
	ctx := context.Background()

	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if log != nil {
			log(line)
		}
		file, perr := parser.Parse(strings.NewReader(line), "")
		if perr != nil {
			fmt.Fprintf(stderr, "%v\n", perr)
			continue
		}
		if err := runner.Run(ctx, file); err != nil {
			var exit interp.ExitStatus
			if !errors.As(err, &exit) {
				// A command's own nonzero exit already wrote its
				// diagnostic; this branch is a runner-level failure the
				// leaf commands never produce (they only ever return
				// ExitStatus), so it's worth surfacing on its own line.
				fmt.Fprintf(stderr, "%v\n", err)
			}
		}
		if runner.Exited() {
			break
		}
	}
	return sc.Err()
}

// callHandler intercepts specific builtin names before the runner's own
// Funcs/builtin/exec dispatch, which is how a shell function is allowed
// to shadow a builtin (r.call checks Funcs before IsBuiltin), and reuses
// that same precedence here without needing to build real *syntax.Stmt
// function bodies. Every other command passes through unchanged to the
// runner's normal dispatch, ending at execHandler for anything that
// isn't itself a builtin.
func callHandler(env *shellEnv) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		hc := interp.HandlerCtx(ctx)
		switch baseName(args[0]) {
		case "echo":
			// mvdan/sh's builtin echo needs -e for any backslash
			// expansion, and even with -e has no \c support at all: it
			// falls through expand.Format's escape switch as two
			// literal characters. inst wraps every command in
			// echo 'TOKEN\c' to delimit output without a trailing
			// newline, so the default builtin cannot serve inst at all.
			shEcho(hc.Stdout, args[1:])
			return []string{":"}, nil
		case "trap":
			// Real signal delivery is meaningless here - there's no
			// terminal or OS signal source reaching an interpreted rsh
			// session - so a numbered or named signal trap is always a
			// safe no-op. EXIT/ERR and a bare query are real, so let
			// the builtin see those; mvdan/sh's own trap only accepts
			// EXIT/ERR and errors on anything else ("invalid signal
			// specification"), which would otherwise turn inst's
			// routine `trap : 2` into spurious stderr noise and a
			// nonzero $?.
			if passthroughTrap(args[1:]) {
				return args, nil
			}
			return []string{":"}, nil
		case "test", "[":
			// test/[ is handled entirely here rather than left to
			// mvdan/sh's own builtin: its -r/-w/-x operators call
			// unix.Access on the literal path with no handler
			// indirection (interp/os_unix.go), and its cd builtin does
			// the same for -x during the directory check. There is no
			// hook to redirect either to the vfs, so any path to the
			// real test/[ builtin is a hole straight through the
			// security boundary this shell exists to enforce.
			return []string{boolWord(evalTest(env, hc, args))}, nil
		case "cd":
			// Hand-rolled for the same reason test/[ is: mvdan/sh's own
			// cd builtin calls unix.Access on the real path for its -x
			// check (interp/builtin.go's changeDir), with no handler to
			// redirect it. cwd only ever moves to a path this shell can
			// already Stat through the vfs, so it can never point
			// anywhere the vfs itself couldn't already reach.
			target := "/"
			if len(args) > 1 {
				target = args[len(args)-1]
			}
			abs := env.resolve(target)
			info, err := env.fsys.Stat(strings.TrimPrefix(abs, "/"))
			if err != nil || !info.IsDir {
				fmt.Fprintf(hc.Stderr, "cd: %s: not a directory\n", target)
				return []string{"false"}, nil
			}
			env.cwd = abs
			return []string{"true"}, nil
		case "source", ".":
			// source/. probes $PATH on the real filesystem via os.Stat
			// before ever reaching the open handler (interp/builtin.go's
			// scriptFromPathDir). inst's observed corpus doesn't use
			// either; refuse outright rather than accept that probe as
			// a side effect.
			fmt.Fprintf(hc.Stderr, "%s: not supported\n", args[0])
			return []string{"false"}, nil
		default:
			return args, nil
		}
	}
}

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// passthroughTrap reports whether a trap invocation only names EXIT/ERR
// (or is a bare query), mirroring the argument parsing interp/builtin.go
// itself uses: zero args is a query, one arg is a signal spec with the
// default action restored, and two or more treat the first as the
// callback and the rest as signal specs.
func passthroughTrap(rest []string) bool {
	var sigs []string
	switch len(rest) {
	case 0:
		return true
	case 1:
		sigs = rest
	default:
		sigs = rest[1:]
	}
	for _, s := range sigs {
		if s != "EXIT" && s != "ERR" {
			return false
		}
	}
	return true
}

// shEcho implements the XSI-style echo IRIX's /bin/sh uses by default,
// no -e needed: backslash escapes are always interpreted, and \c
// suppresses everything from that point on, including the trailing
// newline. Only \c is load-bearing for inst's marker protocol; the rest
// are implemented because they're the standard XSI escape table and cost
// nothing extra to get right.
func shEcho(w io.Writer, args []string) {
	s := strings.Join(args, " ")
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		i++
		switch s[i] {
		case 'c':
			io.WriteString(w, b.String())
			return // no trailing newline: the marker ends right here
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'v':
			b.WriteByte('\v')
		case '\\':
			b.WriteByte('\\')
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	b.WriteByte('\n')
	io.WriteString(w, b.String())
}

// evalTest evaluates a test/[ invocation against the vfs. args[0] is
// "test" or "[".
func evalTest(env *shellEnv, hc interp.HandlerContext, args []string) bool {
	operands := args[1:]
	if args[0] == "[" {
		if len(operands) == 0 || operands[len(operands)-1] != "]" {
			fmt.Fprintln(hc.Stderr, "[: missing ']'")
			return false
		}
		operands = operands[:len(operands)-1]
	}
	ok, err := runTest(env, operands)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "test: %v\n", err)
	}
	return ok
}

// runTest evaluates the small subset of POSIX test(1) inst is known to
// use: existence/type checks against the vfs (cwd-relative, like every
// other path this shell resolves), and plain string/integer
// comparisons, neither of which needs anything beyond what FileSystem
// already exposes. Anything wider (-a/-o, parenthesised expressions,
// -r/-w/-x, ownership tests) is refused with an error rather than
// silently answering false, so a gap here is visible instead of quietly
// always failing.
func runTest(env *shellEnv, args []string) (bool, error) {
	switch len(args) {
	case 0:
		return false, nil
	case 1:
		return args[0] != "", nil
	case 2:
		if args[0] == "!" {
			ok, err := runTest(env, args[1:])
			return !ok, err
		}
		return unaryTest(env, args[0], args[1])
	case 3:
		return binaryTest(args[0], args[1], args[2])
	default:
		return false, fmt.Errorf("unsupported expression: %s", strings.Join(args, " "))
	}
}

func unaryTest(env *shellEnv, op, operand string) (bool, error) {
	stat := func() (FileInfo, error) { return env.fsys.Stat(env.fsPath(operand)) }
	switch op {
	case "-z":
		return operand == "", nil
	case "-n":
		return operand != "", nil
	case "-f":
		info, err := stat()
		return err == nil && !info.IsDir, nil
	case "-d":
		info, err := stat()
		return err == nil && info.IsDir, nil
	case "-e":
		_, err := stat()
		return err == nil, nil
	case "-s":
		info, err := stat()
		return err == nil && info.Size > 0, nil
	default:
		return false, fmt.Errorf("%s: not supported", op)
	}
}

func binaryTest(a, op, b string) (bool, error) {
	switch op {
	case "=", "==":
		return a == b, nil
	case "!=":
		return a != b, nil
	case "-eq", "-ne", "-lt", "-le", "-gt", "-ge":
		an, err1 := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
		bn, err2 := strconv.ParseInt(strings.TrimSpace(b), 10, 64)
		if err1 != nil || err2 != nil {
			return false, fmt.Errorf("%s %s %s: integer expression expected", a, op, b)
		}
		switch op {
		case "-eq":
			return an == bn, nil
		case "-ne":
			return an != bn, nil
		case "-lt":
			return an < bn, nil
		case "-le":
			return an <= bn, nil
		case "-gt":
			return an > bn, nil
		default: // -ge
			return an >= bn, nil
		}
	default:
		return false, fmt.Errorf("%s: not supported", op)
	}
}

// execHandler runs for any simple command that is neither a shell
// function nor a builtin - the vfs-backed leaf commands, and a refusal
// for everything else. This is the whitelist: dd, ls, and cat are the
// only external-looking programs this shell can run.
func execHandler(env *shellEnv) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		hc := interp.HandlerCtx(ctx)
		switch baseName(args[0]) {
		case "dd":
			return shDD(env, hc, args[1:])
		case "ls":
			return shLs(env, hc, args[1:])
		case "cat":
			return shCat(env, hc, args[1:])
		default:
			fmt.Fprintf(hc.Stderr, "%s: not supported\n", args[0])
			return interp.ExitStatus(1)
		}
	}
}

// shLs implements the ls forms inst is known to send: -i (inode), -n
// (numeric uid/gid - the only kind this shell has, so it's the default
// regardless), -l (long format), -g (accepted; group is always in the
// long line here, see lsLongLine), -d (the named path itself rather
// than a directory's contents). Without -l it's still just names,
// cwd-relative like everything else. inst mainly keys on the leading
// inode field for file identity, not the rest of the line.
func shLs(env *shellEnv, hc interp.HandlerContext, args []string) error {
	var flagI, flagL, flagD bool
	target := "."
	for _, a := range args {
		if strings.HasPrefix(a, "-") && len(a) > 1 {
			for _, c := range a[1:] {
				switch c {
				case 'i':
					flagI = true
				case 'l':
					flagL = true
				case 'd':
					flagD = true
				}
			}
			continue
		}
		target = a
	}

	abs := env.resolve(target)
	fsPath := strings.TrimPrefix(abs, "/")
	info, err := env.fsys.Stat(fsPath)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "ls: %s: %v\n", target, err)
		return interp.ExitStatus(1)
	}

	if flagD || !info.IsDir {
		printLsEntry(hc.Stdout, flagI, flagL, target, info)
		return nil
	}
	names, err := env.fsys.ReadDir(fsPath)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "ls: %s: %v\n", target, err)
		return interp.ExitStatus(1)
	}
	if !flagL {
		// Names only: no per-entry Stat needed, and none required - a
		// name ReadDir lists but can't itself be Stat'd (a dangling
		// entry, say) must still show up here, exactly as a plain ls
		// always did before -l/-i/-d existed.
		for _, n := range names {
			fmt.Fprintln(hc.Stdout, n)
		}
		return nil
	}
	base := strings.TrimSuffix(abs, "/")
	for _, n := range names {
		childInfo, err := env.fsys.Stat(strings.TrimPrefix(base+"/"+n, "/"))
		if err != nil {
			continue
		}
		printLsEntry(hc.Stdout, flagI, flagL, n, childInfo)
	}
	return nil
}

func printLsEntry(w io.Writer, flagI, flagL bool, name string, info FileInfo) {
	if !flagL {
		fmt.Fprintln(w, name)
		return
	}
	fmt.Fprintln(w, lsLongLine(flagI, name, info))
}

// lsLongLine formats one ls -l line: [inode] perms nlink uid gid size
// mon day time-or-year name.
func lsLongLine(withInode bool, name string, info FileInfo) string {
	var b strings.Builder
	if withInode {
		fmt.Fprintf(&b, "%d ", info.Ino)
	}
	fmt.Fprintf(&b, "%s %3d %5d %5d %8d %s %s",
		permString(info.IsDir, info.Perm), info.Nlink, info.UID, info.GID, info.Size,
		lsDate(info.Mtime), name)
	return b.String()
}

// permString renders the type + rwxrwxrwx string ls -l shows.
// instigator's vfs never sets setuid/setgid/sticky, so those bits are
// out of scope.
func permString(isDir bool, perm uint32) string {
	b := []byte("----------")
	if isDir {
		b[0] = 'd'
	}
	const bits = "rwxrwxrwx"
	for i := 0; i < 9; i++ {
		if perm&(1<<uint(8-i)) != 0 {
			b[i+1] = bits[i]
		}
	}
	return string(b)
}

// lsDate formats a modification time the way ls -l does: "Mon Day
// HH:MM" for a recent file, "Mon Day  Year" once it's more than about
// six months old (or in the future, from a clock skew or an unset
// mtime). instigator's media doesn't move, so the exact cutoff rarely
// matters; inst reads the leading inode field for identity, not this
// column.
func lsDate(t time.Time) string {
	if t.IsZero() {
		t = time.Unix(0, 0).UTC()
	}
	age := time.Since(t)
	if age > 182*24*time.Hour || age < 0 {
		return fmt.Sprintf("%s %2d  %4d", t.Format("Jan"), t.Day(), t.Year())
	}
	return fmt.Sprintf("%s %2d %02d:%02d", t.Format("Jan"), t.Day(), t.Hour(), t.Minute())
}

func shCat(env *shellEnv, hc interp.HandlerContext, args []string) error {
	if len(args) == 0 {
		if hc.Stdin != nil {
			io.Copy(hc.Stdout, hc.Stdin)
		}
		return nil
	}
	for _, p := range args {
		f, err := env.fsys.Open(env.fsPath(p))
		if err != nil {
			fmt.Fprintf(hc.Stderr, "cat: %s: %v\n", p, err)
			return interp.ExitStatus(1)
		}
		if _, err := io.Copy(hc.Stdout, io.NewSectionReader(f, 0, f.Size())); err != nil {
			return err
		}
	}
	return nil
}

// runDD covers what inst actually drives: if=<vfspath> or a piped
// stdin, bs/ibs/obs, skip/iseek, count, and oseek/seek/obs accepted but
// inert since nothing this dd writes to is ever seekable. of= is
// refused outright - this dd only ever writes to its own stdout.
func shDD(env *shellEnv, hc interp.HandlerContext, args []string) error {
	var (
		file      string
		bs        int64 = -1
		ibs, obs  int64 = 512, 512
		skip      int64
		count     int64
		haveCount bool
	)
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			fmt.Fprintf(hc.Stderr, "dd: bad operand %q\n", a)
			return interp.ExitStatus(1)
		}
		switch k {
		case "if":
			file = v
		case "of":
			fmt.Fprintln(hc.Stderr, "dd: of=: not supported (read-only)")
			return interp.ExitStatus(1)
		case "bs":
			n, err := parseSize(v)
			if err != nil {
				fmt.Fprintf(hc.Stderr, "dd: bs: %v\n", err)
				return interp.ExitStatus(1)
			}
			bs = n
		case "ibs":
			n, err := parseSize(v)
			if err != nil {
				fmt.Fprintf(hc.Stderr, "dd: ibs: %v\n", err)
				return interp.ExitStatus(1)
			}
			ibs = n
		case "obs":
			if _, err := parseSize(v); err != nil {
				fmt.Fprintf(hc.Stderr, "dd: obs: %v\n", err)
				return interp.ExitStatus(1)
			}
		case "skip", "iseek":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				fmt.Fprintf(hc.Stderr, "dd: %s: bad value %q\n", k, v)
				return interp.ExitStatus(1)
			}
			skip = n
		case "seek", "oseek":
			if n, err := strconv.ParseInt(v, 10, 64); err != nil || n < 0 {
				fmt.Fprintf(hc.Stderr, "dd: %s: bad value %q\n", k, v)
				return interp.ExitStatus(1)
			}
		case "count":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				fmt.Fprintf(hc.Stderr, "dd: count: bad value %q\n", v)
				return interp.ExitStatus(1)
			}
			count, haveCount = n, true
		default:
			fmt.Fprintf(hc.Stderr, "dd: operand %q not supported\n", k)
			return interp.ExitStatus(1)
		}
	}
	if bs > 0 {
		ibs, obs = bs, bs
	}
	_ = obs // validated above; never acted on, see the case "obs" comment

	var src io.Reader
	if file != "" {
		f, err := env.fsys.Open(env.fsPath(file))
		if err != nil {
			fmt.Fprintf(hc.Stderr, "dd: %s: %v\n", file, err)
			return interp.ExitStatus(1)
		}
		off := skip * ibs
		size := f.Size() - off
		if size < 0 {
			size = 0
		}
		src = io.NewSectionReader(f, off, size)
	} else {
		in := hc.Stdin
		if in == nil {
			in = strings.NewReader("")
		}
		if skip > 0 {
			if _, err := io.CopyN(io.Discard, in, skip*ibs); err != nil && err != io.EOF {
				fmt.Fprintf(hc.Stderr, "dd: skip: %v\n", err)
				return interp.ExitStatus(1)
			}
		}
		src = in
	}
	if haveCount {
		src = io.LimitReader(src, count*ibs)
	}

	buf := make([]byte, ibs)
	var full, partial int64
	for {
		n, err := io.ReadFull(src, buf)
		if n > 0 {
			if _, werr := hc.Stdout.Write(buf[:n]); werr != nil {
				return werr
			}
			if int64(n) == ibs {
				full++
			} else {
				partial++
			}
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			fmt.Fprintf(hc.Stderr, "dd: read: %v\n", err)
			return interp.ExitStatus(1)
		}
	}
	fmt.Fprintf(hc.Stderr, "%d+%d records in\n%d+%d records out\n",
		full, boolToInt(partial > 0), full, boolToInt(partial > 0))
	return nil
}

// openHandler backs every redirect the shell parses. Reads resolve
// through the vfs, cwd-relative like every other path here, exactly as
// dd/ls/cat do; anything with a write intent is refused before the vfs
// is even consulted, and /dev/null is synthesized rather than left to
// touch a real device node. There is no other path here: a redirect or
// test against a real host path (/etc/passwd, say) is just a miss
// against the vfs, indistinguishable from any other path that doesn't
// exist in it.
func openHandler(env *shellEnv) interp.OpenHandlerFunc {
	return func(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
		if path == "/dev/null" {
			return devNull{}, nil
		}
		if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_APPEND|os.O_TRUNC|os.O_EXCL) != 0 {
			return nil, &os.PathError{Op: "open", Path: path, Err: errReadOnly}
		}
		f, err := env.fsys.Open(env.fsPath(path))
		if err != nil {
			return nil, &os.PathError{Op: "open", Path: path, Err: err}
		}
		return &roFile{r: io.NewSectionReader(f, 0, f.Size())}, nil
	}
}

// roFile adapts a read-only vfs file to io.ReadWriteCloser, the type
// OpenHandlerFunc must return. Write and Close are both no-ops/refusals:
// nothing this shell opens is ever writable, and File carries no Close
// of its own to forward.
type roFile struct{ r *io.SectionReader }

func (f *roFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *roFile) Write([]byte) (int, error)  { return 0, errReadOnly }
func (f *roFile) Close() error               { return nil }

// devNull is a synthetic /dev/null: reads EOF immediately, writes are
// absorbed, exactly like the real device, without ever opening it.
type devNull struct{}

func (devNull) Read([]byte) (int, error)    { return 0, io.EOF }
func (devNull) Write(p []byte) (int, error) { return len(p), nil }
func (devNull) Close() error                { return nil }

// readDirHandler backs shell globbing. inst's observed corpus doesn't
// glob, but wiring it correctly costs little: each entry's metadata
// comes from the same FileSystem.Stat test/[ and StatHandler both use.
func readDirHandler(env *shellEnv) interp.ReadDirHandlerFunc2 {
	return func(ctx context.Context, path string) ([]fs.DirEntry, error) {
		abs := env.resolve(path)
		p := strings.TrimPrefix(abs, "/")
		names, err := env.fsys.ReadDir(p)
		if err != nil {
			return nil, err
		}
		base := strings.TrimSuffix(abs, "/")
		entries := make([]fs.DirEntry, len(names))
		for i, n := range names {
			info, err := env.fsys.Stat(strings.TrimPrefix(base+"/"+n, "/"))
			if err != nil {
				info = FileInfo{}
			}
			entries[i] = fs.FileInfoToDirEntry(fsInfoAdapter{name: n, info: info})
		}
		return entries, nil
	}
}

// statHandler backs any interpreter internals that call Stat, such as
// glob matching. test -f/-d/-e/-s goes through our own runTest instead,
// never mvdan/sh's built-in test/[, which this shell doesn't let run at
// all (see callHandler's "test", "[" case).
func statHandler(env *shellEnv) interp.StatHandlerFunc {
	return func(ctx context.Context, path string, followSymlinks bool) (fs.FileInfo, error) {
		abs := env.resolve(path)
		info, err := env.fsys.Stat(strings.TrimPrefix(abs, "/"))
		if err != nil {
			return nil, err
		}
		return fsInfoAdapter{name: baseName(strings.TrimSuffix(abs, "/")), info: info}, nil
	}
}

// fsInfoAdapter adapts FileInfo to io/fs.FileInfo, which is what
// interp.StatHandlerFunc and ReadDirHandlerFunc2 must return. name is
// supplied separately since FileInfo carries no name of its own - every
// caller already knows the path it stat'd.
type fsInfoAdapter struct {
	name string
	info FileInfo
}

func (a fsInfoAdapter) Name() string { return a.name }
func (a fsInfoAdapter) Size() int64  { return a.info.Size }
func (a fsInfoAdapter) Mode() fs.FileMode {
	if a.info.IsDir {
		return fs.ModeDir | fs.FileMode(a.info.Perm)
	}
	return fs.FileMode(a.info.Perm)
}
func (a fsInfoAdapter) ModTime() time.Time { return a.info.Mtime }
func (a fsInfoAdapter) IsDir() bool        { return a.info.IsDir }
func (a fsInfoAdapter) Sys() any           { return nil }
