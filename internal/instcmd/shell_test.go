package instcmd

import (
	"bytes"
	"strings"
	"testing"
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

func runShell(t *testing.T, script string) (string, string) {
	t.Helper()
	var out, errb strings.Builder
	var logged []string
	if err := RunShell(shellTestFS(), strings.NewReader(script), &out, &errb, func(l string) { logged = append(logged, l) }); err != nil {
		t.Fatalf("RunShell: %v (stderr: %s)", err, errb.String())
	}
	t.Logf("logged commands: %v", logged)
	return out.String(), errb.String()
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
