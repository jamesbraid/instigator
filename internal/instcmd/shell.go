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
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jamesbraid/instigator/internal/capture"
	"github.com/jamesbraid/instigator/internal/logging"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// errReadOnly is returned by the open handler for any write attempt.
var errReadOnly = errors.New("read-only filesystem")

// shellEnv holds one session's VFS, working directory, logging, and
// capture state. cwd is tracked here, not via the interpreter's own Dir,
// because cd is hand-rolled - see callHandler's "cd" case.
type shellEnv struct {
	fsys   FileSystem
	cwd    string // always an absolute, path.Clean'd vfs path
	logger *logging.Logger
	sess   *capture.Session // nil when capture is off; every use is nil-safe

	// refused is set by any handler that refuses a command as policy (not
	// whitelisted, a write attempt, an operand it won't accept), so the
	// scanner loop can record the command's result as "refused" rather
	// than an indistinguishable nonzero exit. It is per-command: the loop
	// clears it before each line. Atomic because a pipeline's stages run
	// in concurrent goroutines that may each refuse.
	refused atomic.Bool
}

func newShellEnv(fsys FileSystem, logger *logging.Logger) *shellEnv {
	return &shellEnv{fsys: fsys, cwd: "/", logger: logger}
}

// resolve turns p into an absolute vfs path against cwd, using "path"
// (not "path/filepath"): vfs paths are always forward-slash, never host
// paths.
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

func (e *shellEnv) warnf(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
	e.logger.Warnf("instcmd: "+format, args...)
}

// refusef reports a capability this shell refuses as policy - not
// whitelisted, a write attempt, an operand it won't accept - and marks
// the command refused for the trace. It logs at ERROR because inst's
// marker protocol absorbs failures silently: a refused fgrep once broke
// a live install with nothing in the server log.
func (e *shellEnv) refusef(w io.Writer, format string, args ...any) {
	fmt.Fprintf(w, format+"\n", args...)
	e.logger.Errorf("instcmd: "+format, args...)
	e.refused.Store(true)
}

func stdinOr(hc interp.HandlerContext) io.Reader {
	if hc.Stdin != nil {
		return hc.Stdin
	}
	return strings.NewReader("")
}

// logServed logs the served path, and its backing image when known, at
// INFO - the raw command line stays at DEBUG. rawPath is what the leaf
// command was given (dd's if=, cat's operand).
func (e *shellEnv) logServed(rawPath string) {
	abs := e.resolve(rawPath)
	if ir, ok := e.fsys.(ImageResolver); ok {
		if r, err := ir.ResolveImage(e.fsPath(rawPath)); err == nil {
			e.logger.Infof("instcmd: served %s  <-  %s:/%s", abs, r.Image, r.Path)
			e.sess.RecordServed(abs, r.Image, r.Path)
			return
		}
	}
	e.logger.Infof("instcmd: served %s", abs)
	e.sess.RecordServed(abs, "", "")
}

// RunShell serves the shell inst opens over rsh with "exec /bin/sh": one
// mvdan/sh Runner for the life of the connection, fed one line at a time
// so state (variables, traps, $?) persists across lines like a real
// shell's does. logger records every line at DEBUG, and ERROR/WARN for
// anything a leaf command refuses or fails (see refusef). A failing
// command writes its own diagnostic and the shell keeps going, matching
// inst's expectation that the trailing marker wrapper always still runs.
func RunShell(fsys FileSystem, stdin io.Reader, stdout, stderr io.Writer, logger *logging.Logger, sess *capture.Session) error {
	// With capture on, count every EFS backing read against the current
	// command (countingFS) and every socket write - stdout and stderr both
	// bind to the rsh connection - against the command and the session.
	// countingFS keeps ImageResolver working by delegating, so logServed
	// still reports the backing image. All of this is skipped when sess is
	// nil so capture-off costs nothing.
	if sess != nil {
		fsys = countingFS{inner: fsys, sess: sess}
		stdout = sess.WrapWriter(stdout, false)
		stderr = sess.WrapWriter(stderr, true)
	}
	env := newShellEnv(fsys, logger)
	env.sess = sess
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

	// LangPOSIX matches IRIX's actual /bin/sh and blocks Bash process
	// substitution (<(...)); upstream's implementation of it would create
	// a real host FIFO, which this shell must never do.
	parser := syntax.NewParser(syntax.Variant(syntax.LangPOSIX))
	ctx := context.Background()

	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		logger.Debugf("instcmd: rsh-sh: %q", line)
		file, perr := parser.Parse(strings.NewReader(line), "")
		if perr != nil {
			fmt.Fprintf(stderr, "%v\n", perr)
			logger.Warnf("instcmd: rsh-sh: %q: parse error: %v", line, perr)
			continue
		}
		// One scanner line is one command. Clear the per-command refusal
		// flag, mark this command current so the counting wrappers
		// attribute to it, run it, then record its exit status and whether
		// a handler refused it.
		env.refused.Store(false)
		cmd := sess.BeginCommand(line)
		status := 0
		if err := runner.Run(ctx, file); err != nil {
			var exit interp.ExitStatus
			if errors.As(err, &exit) {
				status = int(exit)
			} else {
				// A command's own nonzero exit already wrote its
				// diagnostic; this branch is a runner-level failure the
				// leaf commands never produce (they only ever return
				// ExitStatus), so it's worth surfacing on its own line.
				status = -1
				fmt.Fprintf(stderr, "%v\n", err)
				logger.Errorf("instcmd: rsh-sh: %q: %v", line, err)
			}
		}
		cmd.End(status, env.refused.Load())
		if runner.Exited() {
			break
		}
	}
	return sc.Err()
}

// callHandler intercepts specific builtin names before the runner's own
// dispatch, reusing the Funcs-before-builtin precedence a real shell
// function shadowing a builtin would get (r.call checks Funcs before
// IsBuiltin). Everything else passes through to execHandler.
func callHandler(env *shellEnv) interp.CallHandlerFunc {
	return func(ctx context.Context, args []string) ([]string, error) {
		hc := interp.HandlerCtx(ctx)
		switch path.Base(args[0]) {
		case "echo":
			// mvdan/sh's builtin echo has no \c support even with -e,
			// and inst wraps every command in echo 'TOKEN\c' to delimit
			// output without a trailing newline - see shEcho.
			shEcho(hc.Stdout, args[1:])
			return []string{":"}, nil
		case "trap":
			// A numbered or named signal trap is a safe no-op - no OS
			// signal ever reaches an interpreted rsh session - but
			// mvdan/sh's trap accepts only EXIT/ERR and would turn
			// inst's routine `trap : 2` into stderr noise and a nonzero
			// $?. EXIT/ERR and a bare query are real; let the builtin
			// see those.
			if passthroughTrap(args[1:]) {
				return args, nil
			}
			return []string{":"}, nil
		case "test", "[":
			// Never mvdan/sh's own test/[: its -r/-w/-x operators call
			// unix.Access on the literal host path with no handler
			// indirection, a hole straight through the security
			// boundary this shell exists to enforce.
			if evalTest(env, hc, args) {
				return []string{"true"}, nil
			}
			return []string{"false"}, nil
		case "cd":
			// Hand-rolled for the same reason: mvdan/sh's cd calls
			// unix.Access on the real path for its -x check. cwd only
			// ever moves to a path the vfs can already Stat.
			target := "/"
			if len(args) > 1 {
				target = args[len(args)-1]
			}
			abs := env.resolve(target)
			info, err := env.fsys.Stat(strings.TrimPrefix(abs, "/"))
			if err != nil || !info.IsDir {
				env.warnf(hc.Stderr, "cd: %s: not a directory", target)
				return []string{"false"}, nil
			}
			env.cwd = abs
			return []string{"true"}, nil
		case "source", ".":
			// source/. probes $PATH on the real filesystem via os.Stat
			// before ever reaching the open handler; refuse outright.
			env.refusef(hc.Stderr, "%s: not supported", args[0])
			return []string{"false"}, nil
		default:
			return args, nil
		}
	}
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
// newline - which is what inst's marker protocol depends on, and what
// mvdan/sh's builtin echo cannot produce at all.
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

// execHandler permits only the external commands observed from inst.
func execHandler(env *shellEnv) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		hc := interp.HandlerCtx(ctx)
		switch path.Base(args[0]) {
		case "dd":
			return shDD(env, hc, args[1:])
		case "ls":
			return shLs(env, hc, args[1:])
		case "cat":
			return shCat(env, hc, args[1:])
		case "grep", "fgrep", "egrep":
			return shGrep(env, hc, path.Base(args[0]), args[1:])
		default:
			env.refusef(hc.Stderr, "%s: not supported", args[0])
			return interp.ExitStatus(1)
		}
	}
}

// shGrep implements the grep forms inst pipes .idb reads through, most
// importantly `fgrep ' mach('` to find a product's machine-conditional
// lines - without it those products read as "No valid products in
// distribution". It follows grep's exit status (0 match, 1 no match, 2
// usage/pattern error) and refuses any flag beyond -v/-i/-e rather than
// silently ignoring it. fgrep matches a fixed string, grep/egrep a regexp.
func shGrep(env *shellEnv, hc interp.HandlerContext, name string, args []string) error {
	var (
		invert, ignoreCase bool
		pattern            string
		havePattern        bool
		files              []string
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !havePattern && strings.HasPrefix(a, "-") && len(a) > 1 {
			for _, c := range a[1:] {
				switch c {
				case 'v':
					invert = true
				case 'i':
					ignoreCase = true
				case 'e':
					// -e PATTERN: the pattern is the next argument
					if i+1 < len(args) {
						i++
						pattern, havePattern = args[i], true
					}
				default:
					env.refusef(hc.Stderr, "%s: -%c: not supported", name, c)
					return interp.ExitStatus(2)
				}
			}
			continue
		}
		if !havePattern {
			pattern, havePattern = a, true
			continue
		}
		files = append(files, a)
	}
	if !havePattern {
		env.warnf(hc.Stderr, "%s: no pattern", name)
		return interp.ExitStatus(2)
	}

	var match func(string) bool
	if name == "fgrep" {
		needle := pattern
		if ignoreCase {
			needle = strings.ToLower(needle)
		}
		match = func(line string) bool {
			if ignoreCase {
				line = strings.ToLower(line)
			}
			return strings.Contains(line, needle)
		}
	} else {
		expr := pattern
		if ignoreCase {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			env.warnf(hc.Stderr, "%s: bad pattern %q: %v", name, pattern, err)
			return interp.ExitStatus(2)
		}
		match = re.MatchString
	}

	type source struct {
		name string
		r    io.Reader
	}
	var sources []source
	if len(files) == 0 {
		sources = append(sources, source{"(standard input)", stdinOr(hc)})
	} else {
		for _, p := range files {
			f, err := env.fsys.Open(env.fsPath(p))
			if err != nil {
				env.warnf(hc.Stderr, "%s: %s: %v", name, p, err)
				continue
			}
			env.logServed(p)
			sources = append(sources, source{p, io.NewSectionReader(f, 0, f.Size())})
		}
	}

	found := false
	for _, s := range sources {
		sc := bufio.NewScanner(s.r)
		sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for sc.Scan() {
			line := sc.Text()
			m := match(line)
			if invert {
				m = !m
			}
			if !m {
				continue
			}
			found = true
			if len(files) > 1 {
				fmt.Fprintf(hc.Stdout, "%s:%s\n", s.name, line)
			} else {
				fmt.Fprintln(hc.Stdout, line)
			}
		}
	}
	if found {
		return nil
	}
	return interp.ExitStatus(1)
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
		env.warnf(hc.Stderr, "ls: %s: %v", target, err)
		return interp.ExitStatus(1)
	}

	if flagD || !info.IsDir {
		if flagL {
			fmt.Fprintln(hc.Stdout, lsLongLine(flagI, target, info))
		} else {
			fmt.Fprintln(hc.Stdout, target)
		}
		return nil
	}
	names, err := env.fsys.ReadDir(fsPath)
	if err != nil {
		env.warnf(hc.Stderr, "ls: %s: %v", target, err)
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
		fmt.Fprintln(hc.Stdout, lsLongLine(flagI, n, childInfo))
	}
	return nil
}

// lsLongLine formats one ls -l line: [inode] perms nlink uid gid size
// mon day time-or-year name.
func lsLongLine(withInode bool, name string, info FileInfo) string {
	line := fmt.Sprintf("%s %3d %5d %5d %8d %s %s",
		permString(info.IsDir, info.Perm), info.Nlink, info.UID, info.GID, info.Size,
		lsDate(info.Mtime), name)
	if withInode {
		return fmt.Sprintf("%d %s", info.Ino, line)
	}
	return line
}

// permString renders file type and rwx permission columns.
func permString(isDir bool, perm uint32) string {
	b := []byte("----------")
	if isDir {
		b[0] = 'd'
	}
	const bits = "rwxrwxrwx"
	for i := 0; i < 9; i++ {
		if perm&(1<<(8-i)) != 0 {
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
		io.Copy(hc.Stdout, stdinOr(hc))
		return nil
	}
	for _, p := range args {
		f, err := env.fsys.Open(env.fsPath(p))
		if err != nil {
			env.warnf(hc.Stderr, "cat: %s: %v", p, err)
			return interp.ExitStatus(1)
		}
		env.logServed(p)
		if _, err := io.Copy(hc.Stdout, io.NewSectionReader(f, 0, f.Size())); err != nil {
			return err
		}
	}
	return nil
}

// shDD covers what inst actually drives: if=<vfspath> or a piped stdin,
// bs/ibs, skip/iseek, count. obs/oseek/seek are validated but inert -
// nothing this dd writes to is ever seekable, and output re-blocking
// can't be observed on a byte stream. of= is refused outright: this dd
// only ever writes to its own stdout.
func shDD(env *shellEnv, hc interp.HandlerContext, args []string) error {
	var (
		file      string
		bs        int64 = -1
		ibs       int64 = 512
		skip      int64
		count     int64
		haveCount bool
	)
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok {
			env.warnf(hc.Stderr, "dd: bad operand %q", a)
			return interp.ExitStatus(1)
		}
		switch k {
		case "if":
			file = v
		case "of":
			env.refusef(hc.Stderr, "dd: of=: not supported (read-only)")
			return interp.ExitStatus(1)
		case "bs":
			n, err := parseSize(v)
			if err != nil {
				env.warnf(hc.Stderr, "dd: bs: %v", err)
				return interp.ExitStatus(1)
			}
			bs = n
		case "ibs":
			n, err := parseSize(v)
			if err != nil {
				env.warnf(hc.Stderr, "dd: ibs: %v", err)
				return interp.ExitStatus(1)
			}
			ibs = n
		case "obs":
			if _, err := parseSize(v); err != nil {
				env.warnf(hc.Stderr, "dd: obs: %v", err)
				return interp.ExitStatus(1)
			}
		case "skip", "iseek":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				env.warnf(hc.Stderr, "dd: %s: bad value %q", k, v)
				return interp.ExitStatus(1)
			}
			skip = n
		case "seek", "oseek":
			if n, err := strconv.ParseInt(v, 10, 64); err != nil || n < 0 {
				env.warnf(hc.Stderr, "dd: %s: bad value %q", k, v)
				return interp.ExitStatus(1)
			}
		case "count":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n < 0 {
				env.warnf(hc.Stderr, "dd: count: bad value %q", v)
				return interp.ExitStatus(1)
			}
			count, haveCount = n, true
		default:
			env.refusef(hc.Stderr, "dd: operand %q not supported", k)
			return interp.ExitStatus(1)
		}
	}
	if bs > 0 {
		ibs = bs
	}

	var src io.Reader
	if file != "" {
		f, err := env.fsys.Open(env.fsPath(file))
		if err != nil {
			env.warnf(hc.Stderr, "dd: %s: %v", file, err)
			return interp.ExitStatus(1)
		}
		env.logServed(file)
		off := skip * ibs
		size := f.Size() - off
		if size < 0 {
			size = 0
		}
		src = io.NewSectionReader(f, off, size)
	} else {
		in := stdinOr(hc)
		if skip > 0 {
			if _, err := io.CopyN(io.Discard, in, skip*ibs); err != nil && err != io.EOF {
				fmt.Fprintf(hc.Stderr, "dd: skip: %v\n", err)
				env.logger.Errorf("instcmd: dd: skip: %v", err)
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
			env.logger.Errorf("instcmd: dd: read: %v", err)
			return interp.ExitStatus(1)
		}
	}
	p := boolToInt(partial > 0)
	fmt.Fprintf(hc.Stderr, "%d+%d records in\n%d+%d records out\n", full, p, full, p)
	return nil
}

// openHandler backs every redirect. Reads resolve through the vfs like
// dd/ls/cat; any write intent is refused before the vfs is consulted, and
// /dev/null is synthesized. A path outside the vfs (/etc/passwd, say) is
// just a miss, never a real host filesystem access.
func openHandler(env *shellEnv) interp.OpenHandlerFunc {
	return func(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
		if path == "/dev/null" {
			return devNull{}, nil
		}
		if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_APPEND|os.O_TRUNC|os.O_EXCL) != 0 {
			env.logger.Errorf("instcmd: redirect: %s: not supported (read-only)", path)
			env.refused.Store(true)
			return nil, &os.PathError{Op: "open", Path: path, Err: errReadOnly}
		}
		f, err := env.fsys.Open(env.fsPath(path))
		if err != nil {
			env.logger.Warnf("instcmd: redirect: %s: %v", path, err)
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

// countingFS wraps the FileSystem while capture is on, so every file a
// leaf command opens counts its EFS backing reads against the command
// current when it was opened. It delegates ImageResolver so logServed
// still reports the backing image; ReadDir/Stat pass straight through
// (they don't read file content, so they carry no read cost worth
// counting).
type countingFS struct {
	inner FileSystem
	sess  *capture.Session
}

func (c countingFS) Open(path string) (File, error) {
	f, err := c.inner.Open(path)
	if err != nil {
		return nil, err
	}
	return countingFile{ReaderAt: c.sess.WrapReaderAt(f), size: f.Size()}, nil
}

func (c countingFS) ReadDir(path string) ([]string, error) { return c.inner.ReadDir(path) }
func (c countingFS) Stat(path string) (FileInfo, error)    { return c.inner.Stat(path) }

func (c countingFS) ResolveImage(path string) (Resolved, error) {
	if ir, ok := c.inner.(ImageResolver); ok {
		return ir.ResolveImage(path)
	}
	return Resolved{}, ErrNotFound
}

// countingFile is an open file whose ReadAt calls are counted (via the
// wrapped ReaderAt) while keeping the original Size.
type countingFile struct {
	io.ReaderAt
	size int64
}

func (f countingFile) Size() int64 { return f.size }

// readDirHandler backs shell globbing. It is part of the sandbox, not a
// convenience: the interpreter's default handler globs the host
// filesystem with os.ReadDir, so this must resolve through the vfs even
// though inst's observed corpus never globs.
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
		name := strings.TrimSuffix(abs, "/")
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		return fsInfoAdapter{name: name, info: info}, nil
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
