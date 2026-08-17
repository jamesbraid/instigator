package instcmd

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jamesbraid/instigator/internal/logging"
)

// shellTestFS builds a fixture with a distinctive (non-uniform) byte
// pattern, so a byte-exact check actually catches truncation or
// reordering instead of passing on any file full of one repeated byte.
func shellTestFS() *fakeFS {
	content := make([]byte, 2000)
	for i := range content {
		content[i] = byte(i % 251)
	}
	return &fakeFS{
		files: map[string][]byte{
			"6.5.30/disc1/dist/sa":          content,
			"6.5.30/disc1/dist/PRODUCT.idb": []byte("product descriptor\n"),
		},
		dirs: map[string][]string{
			"6.5.30/disc1/dist": {"sa", "PRODUCT.idb", "miniroot"},
		},
	}
}

// runShell runs a shell script and returns stdout/stderr. It logs at
// DEBUG so every command line and any refusal shows up in the test log
// (t.Logf), matching what RunShell now sends to the server's own log
// instead of the old per-line callback.
func runShell(t *testing.T, script string) (string, string) {
	t.Helper()
	out, errb, _ := runShellWithFS(t, shellTestFS(), logging.LevelDebug, script)
	return out, errb
}

// runShellWithFS is runShell generalized over the FileSystem and log
// level, for a test that needs the log itself - the served-file
// manifest, or the default (non-verbose) level - rather than just
// stdout/stderr.
func runShellWithFS(t *testing.T, fsys FileSystem, level logging.Level, script string) (out, errb, log string) {
	t.Helper()
	var outb, errbb, logbuf strings.Builder
	logger := logging.New(&logbuf, level)
	if err := RunShell(fsys, strings.NewReader(script), &outb, &errbb, logger); err != nil {
		t.Fatalf("RunShell: %v (stderr: %s)", err, errbb.String())
	}
	t.Logf("server log:\n%s", logbuf.String())
	return outb.String(), errbb.String(), logbuf.String()
}

// resolvingFS wraps a *fakeFS with ImageResolver, so a test can check
// the "served ... <- image:path" manifest line dd/cat emit when the
// underlying FileSystem can say where a path's bytes actually live.
type resolvingFS struct {
	*fakeFS
	images map[string]Resolved
}

func (f resolvingFS) ResolveImage(path string) (Resolved, error) {
	r, ok := f.images[path]
	if !ok {
		return Resolved{}, ErrNotFound
	}
	return r, nil
}

// TestShellMarkerProtocol runs inst's real wrapper - a dd, then a
// separate line with trap/subshell/$?/echo '\c' - through the actual
// interpreter, matching what a live Octane2 PROM session sends. The
// wrapped-status suffix "...IsDone0" is deliberate: trap : 2 always
// succeeds under our no-op trap handling, so $? (captured as $status
// inside the subshell) is 0 regardless of what dd did on the previous
// line, exactly as a real shell would compute it here.
func TestShellMarkerProtocol(t *testing.T) {
	script := "dd if=/6.5.30/disc1/dist/sa bs=512 count=1\n" +
		"trap : 2 ; ( status=$? ; trap '' 2 ; echo 'o?_InstProc9IsDone\\c' ; echo 'o?_InstProc9IsDone'$status'\\c' 1>&2 )\n"
	out, errb := runShell(t, script)

	if !strings.HasSuffix(out, "o?_InstProc9IsDone") {
		t.Fatalf("stdout missing trailing marker: ...%q", tail(out, 40))
	}
	if strings.HasSuffix(out, "\n") {
		t.Fatal("marker must not be newline-terminated (\\c)")
	}
	wantDD := shellTestFS().files["6.5.30/disc1/dist/sa"][:512]
	if !bytes.Equal([]byte(out[:512]), wantDD) {
		t.Fatalf("dd output before marker is not byte-exact")
	}

	if !strings.HasSuffix(errb, "o?_InstProc9IsDone0") {
		t.Fatalf("stderr missing trailing marker+status: %q", errb)
	}
	if strings.HasSuffix(errb, "\n") {
		t.Fatal("stderr marker must not be newline-terminated")
	}
}

// TestShellPipeIntoDD is inst's capability probe: a pipe feeding dd's
// stdin, which the old hand-rolled command table could never support.
func TestShellPipeIntoDD(t *testing.T) {
	out, errb := runShell(t, "echo abc|dd iseek=0\n")
	if out != "abc\n" {
		t.Fatalf("piped dd output = %q, want %q", out, "abc\n")
	}
	if !strings.Contains(errb, "0+1 records in") || !strings.Contains(errb, "0+1 records out") {
		t.Fatalf("dd records summary missing: %q", errb)
	}
}

// TestShellDDReadsVFSByteExact drives dd's normal if=<path> mode with a
// block size that doesn't divide the file evenly, over a non-uniform
// byte pattern, so truncation, off-by-one, or block reordering would
// change the hash instead of silently passing.
func TestShellDDReadsVFSByteExact(t *testing.T) {
	out, _ := runShell(t, "dd if=/6.5.30/disc1/dist/sa bs=7\n")
	want := shellTestFS().files["6.5.30/disc1/dist/sa"]
	if len(out) != len(want) {
		t.Fatalf("dd copied %d bytes, want %d", len(out), len(want))
	}
	if !bytes.Equal([]byte(out), want) {
		t.Fatal("dd output is not byte-exact")
	}
}

// TestShellDDLogsServedFileWithImage is the steady-state manifest this
// logging overhaul exists for: at the default (non-verbose) level, dd
// reading a real vfs path must log which file it served and which
// image it actually came from, not the raw "dd if=..." command line -
// that trace stays behind -v (DEBUG).
func TestShellDDLogsServedFileWithImage(t *testing.T) {
	fs := resolvingFS{
		fakeFS: shellTestFS(),
		images: map[string]Resolved{
			"6.5.30/disc1/dist/sa": {Image: "Overlay 1of3.iso", Path: "dist/sa"},
		},
	}
	_, _, log := runShellWithFS(t, fs, logging.LevelInfo, "dd if=/6.5.30/disc1/dist/sa bs=512 count=1\n")
	want := "instcmd: served /6.5.30/disc1/dist/sa  <-  Overlay 1of3.iso:/dist/sa"
	if !strings.Contains(log, want) {
		t.Fatalf("log missing served line %q:\n%s", want, log)
	}
	if strings.Contains(log, "rsh-sh:") {
		t.Fatalf("raw command trace leaked into the default-level log:\n%s", log)
	}
}

// TestShellDDLogsServedFileWithoutImageResolver checks the fallback: a
// FileSystem that can't say which image a path came from (fakeFS,
// here) still gets the served-path line, just without the mapping.
func TestShellDDLogsServedFileWithoutImageResolver(t *testing.T) {
	_, _, log := runShellWithFS(t, shellTestFS(), logging.LevelInfo, "dd if=/6.5.30/disc1/dist/sa bs=512 count=1\n")
	want := "instcmd: served /6.5.30/disc1/dist/sa"
	if !strings.Contains(log, want) {
		t.Fatalf("log missing served line %q:\n%s", want, log)
	}
	if strings.Contains(log, "<-") {
		t.Fatalf("served line has an image mapping with no ImageResolver available:\n%s", log)
	}
}

// TestShellCatLogsServedFile checks cat gets the same manifest
// treatment as dd - it is the other leaf command that actually serves
// a file's content.
func TestShellCatLogsServedFile(t *testing.T) {
	_, _, log := runShellWithFS(t, shellTestFS(), logging.LevelInfo, "cat /6.5.30/disc1/dist/PRODUCT.idb\n")
	want := "instcmd: served /6.5.30/disc1/dist/PRODUCT.idb"
	if !strings.Contains(log, want) {
		t.Fatalf("log missing served line %q:\n%s", want, log)
	}
}

// TestShellUnknownCommandRefused checks that a command outside the
// whitelist fails with a diagnostic and a nonzero exit, but - unlike the
// old table's early-return Run() - the shell keeps going afterward, as a
// real shell does for a ';'-separated command list.
func TestShellUnknownCommandRefused(t *testing.T) {
	out, errb := runShell(t, "rm -rf /\necho survived\n")
	if !strings.Contains(out, "survived") {
		t.Fatalf("shell did not continue past refused command: out=%q err=%q", out, errb)
	}
	if !strings.Contains(errb, "not supported") {
		t.Fatalf("refusal not reported to stderr: %q", errb)
	}
}

// TestShellTestBuiltin exercises test -f/-d, which inst is expected to
// send wrapped in the marker. mvdan/sh's own test/[ builtin evaluates
// these via our vfs-backed StatHandler.
func TestShellTestBuiltin(t *testing.T) {
	script := "test -f /6.5.30/disc1/dist/sa && echo FILE_YES\n" +
		"test -d /6.5.30/disc1/dist && echo DIR_YES\n" +
		"test -f /absent && echo SHOULD_NOT_APPEAR\n" +
		"[ -f /6.5.30/disc1/dist/PRODUCT.idb ] && echo BRACKET_YES\n"
	out, _ := runShell(t, script)
	for _, want := range []string{"FILE_YES", "DIR_YES", "BRACKET_YES"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in output: %q", want, out)
		}
	}
	if strings.Contains(out, "SHOULD_NOT_APPEAR") {
		t.Fatalf("test -f on a missing file reported success: %q", out)
	}
}

// TestShellNeverReachesHostFilesystem is the non-negotiable security
// check: no path, redirect, or test operator can read, write, or probe
// anything on the real host filesystem. /etc/passwd exists on the host
// but not in the vfs, so every one of these must fail exactly the way an
// absent vfs path fails - never by actually touching the host file.
func TestShellNeverReachesHostFilesystem(t *testing.T) {
	script := "cat /etc/passwd\n" +
		"echo leak > /etc/passwd\n" +
		"echo REDIRECT_EXIT=$?\n" +
		"cd /etc\n" +
		"test -r /etc/passwd && echo READABLE\n" +
		"test -w /etc/passwd && echo WRITABLE\n" +
		"test -x /etc/passwd && echo EXECUTABLE\n" +
		"echo done\n"
	out, errb := runShell(t, script)

	if strings.Contains(out, "root") || strings.Contains(out, ":0:0:") {
		t.Fatalf("host /etc/passwd content leaked into stdout: %q", out)
	}
	if !strings.Contains(out, "REDIRECT_EXIT=1") {
		t.Fatalf("write redirect to a host path did not fail with a nonzero exit: %q", out)
	}
	for _, forbidden := range []string{"READABLE", "WRITABLE", "EXECUTABLE"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("test operator reached the real host filesystem: got %q in %q", forbidden, out)
		}
	}
	if !strings.Contains(out, "done") {
		t.Fatalf("shell did not survive the refused commands: out=%q err=%q", out, errb)
	}
	if !strings.Contains(errb, "not supported") && !strings.Contains(errb, "no such") && !strings.Contains(errb, "not found") {
		t.Fatalf("no refusal/not-found diagnostic seen on stderr: %q", errb)
	}
}

// TestShellEchoBackslashC is a focused check on the exact bug the
// mvdan/sh library has: its builtin echo has no \c support at all, even
// with -e, so plain echo 'TOKEN\c' would print the literal characters
// "TOKEN\c" plus a trailing newline instead of "TOKEN" with none.
func TestShellEchoBackslashC(t *testing.T) {
	out, _ := runShell(t, "echo 'TOKEN\\c'\n")
	if out != "TOKEN" {
		t.Fatalf("echo with \\c = %q, want %q (no trailing newline, no literal \\c)", out, "TOKEN")
	}

	out2, _ := runShell(t, "echo hello world\n")
	if out2 != "hello world\n" {
		t.Fatalf("plain echo = %q, want %q", out2, "hello world\n")
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// assertLsLongLine checks the shape inst actually parses out of a long
// ls line: a leading numeric inode field and a permission string
// starting with the directory type bit.
func assertLsLongLine(t *testing.T, line string) {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) < 8 {
		t.Fatalf("long ls line has too few fields (%d): %q", len(fields), line)
	}
	if _, err := strconv.ParseUint(fields[0], 10, 64); err != nil {
		t.Fatalf("inode field %q is not a number: %v (line %q)", fields[0], err, line)
	}
	if fields[1] == "" || fields[1][0] != 'd' {
		t.Fatalf("permission field %q doesn't start with 'd': %q", fields[1], line)
	}
}

// TestShellLsProbeSequence runs the exact three probes a live Octane2
// session sends at the start of a shell, in order: inst uses these to
// stat "." for its inode/identity before it ever cds anywhere.
func TestShellLsProbeSequence(t *testing.T) {
	script := "ls -inld .\n" +
		"/usr/5bin/ls -inld .\n" +
		"ls -inlgd .\n"
	out, errb := runShell(t, script)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d ls lines, want 3: out=%q stderr=%q", len(lines), out, errb)
	}
	for _, line := range lines {
		assertLsLongLine(t, line)
	}
	if strings.Contains(errb, "not supported") || strings.Contains(errb, "not found") {
		t.Fatalf("a probe was refused instead of answered: stderr=%q", errb)
	}
}

// TestShellCdAndRelativeDD is the flow after the probes succeed: inst
// cds into the distribution, then reads a file by a bare relative name.
func TestShellCdAndRelativeDD(t *testing.T) {
	out, errb := runShell(t, "cd /6.5.30/disc1/dist\ndd if=sa bs=7\n")
	want := shellTestFS().files["6.5.30/disc1/dist/sa"]
	if len(out) != len(want) {
		t.Fatalf("relative dd after cd copied %d bytes, want %d (stderr %q)", len(out), len(want), errb)
	}
	if !bytes.Equal([]byte(out), want) {
		t.Fatal("relative dd after cd is not byte-exact")
	}
}

// TestShellLsLongFormatAfterCd checks ls -inld . reports the new
// directory once cwd has actually moved, not the one the shell started
// in.
func TestShellLsLongFormatAfterCd(t *testing.T) {
	out, errb := runShell(t, "cd /6.5.30/disc1/dist\nls -inld .\n")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d ls lines, want 1: out=%q stderr=%q", len(lines), out, errb)
	}
	assertLsLongLine(t, lines[0])
}

// TestShellCdRefusesPathOutsideVFS is the security case for this layer:
// cd only ever resolves through the vfs, so a path that doesn't exist
// there - including a real host path like /etc - fails and leaves cwd
// unchanged, proven here by a relative dd that would only succeed if cd
// had silently moved somewhere it shouldn't have.
func TestShellCdRefusesPathOutsideVFS(t *testing.T) {
	out, errb := runShell(t, "cd /etc\necho CDSTATUS=$?\ndd if=sa bs=7\necho DDSTATUS=$?\n")
	if !strings.Contains(out, "CDSTATUS=1") {
		t.Fatalf("cd to a path outside the vfs did not fail: out=%q stderr=%q", out, errb)
	}
	if !strings.Contains(out, "DDSTATUS=1") {
		t.Fatalf("cwd changed despite the failed cd: relative dd found a file anyway: out=%q stderr=%q", out, errb)
	}
}

// TestShellLsPlainStaysNameOnly guards the pre-existing behavior: ls
// without -l lists bare names, cwd-relative dirs included.
func TestShellLsPlainStaysNameOnly(t *testing.T) {
	out, errb := runShell(t, "cd /6.5.30/disc1/dist\nls .\n")
	if err := errb; err != "" && strings.Contains(err, "not supported") {
		t.Fatalf("plain ls refused: %q", errb)
	}
	for _, want := range []string{"sa", "PRODUCT.idb", "miniroot"} {
		if !strings.Contains(out, want) {
			t.Fatalf("plain ls missing %q: %q", want, out)
		}
	}
	if strings.ContainsAny(out, "0123456789") {
		t.Fatalf("plain ls (no -l) printed something numeric, expected bare names only: %q", out)
	}
}

// logLineRE matches one leveled log line: an ISO8601 timestamp, a
// level word, then the message - the same shape internal/logging's own
// tests check.
var logLineRE = regexp.MustCompile(`(?m)^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}[+-]\d{2}:\d{2} +(DEBUG|INFO|WARN|ERROR) +(.*)$`)

// TestShellRefusedCommandLogsErrorOnServerSide is the headline fix this
// package's logging overhaul exists for: instcmd refusing fgrep went
// only to inst's own stderr, never to serve.log, and that silence was
// why a live install failure took a real capture to diagnose instead of
// a log line. A command this shell refuses must now show up in the
// server's own log at ERROR - even at the default (non-verbose) level,
// where DEBUG's per-command trace is off - not just on the client.
func TestShellRefusedCommandLogsErrorOnServerSide(t *testing.T) {
	var out, errb, logbuf strings.Builder
	logger := logging.New(&logbuf, logging.LevelInfo) // default level: no -v
	script := "not-a-real-command foo\n"
	if err := RunShell(shellTestFS(), strings.NewReader(script), &out, &errb, logger); err != nil {
		t.Fatal(err)
	}

	// the client still sees the refusal on its own stderr, unchanged
	if !strings.Contains(errb.String(), "not supported") {
		t.Fatalf("client stderr missing the refusal: %q", errb.String())
	}

	// and now it also reached our own log, as an ERROR line in the
	// <ISO8601> <LEVEL> <message> shape
	var found string
	for _, m := range logLineRE.FindAllStringSubmatch(logbuf.String(), -1) {
		if m[1] == "ERROR" && strings.Contains(m[2], "not-a-real-command") {
			found = m[0]
			break
		}
	}
	if found == "" {
		t.Fatalf("no ERROR line for the refused command in the server log:\n%s", logbuf.String())
	}
	if !strings.Contains(found, "not supported") {
		t.Fatalf("ERROR line missing %q: %q", "not supported", found)
	}
}
