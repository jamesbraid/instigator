package instcmd

import (
	"bytes"
	"strings"
	"testing"
)

type fakeFS struct {
	files map[string][]byte
	dirs  map[string][]string
}

func (f *fakeFS) Open(path string) (File, error) {
	b, ok := f.files[path]
	if !ok {
		return nil, ErrNotFound
	}
	return &memFile{bytes.NewReader(b), int64(len(b))}, nil
}

func (f *fakeFS) ReadDir(path string) ([]string, error) {
	names, ok := f.dirs[path]
	if !ok {
		return nil, ErrNotFound
	}
	return names, nil
}

type memFile struct {
	*bytes.Reader
	size int64
}

func (m *memFile) Size() int64 { return m.size }

func testFS() *fakeFS {
	return &fakeFS{
		files: map[string][]byte{
			"6.5.30/disc1/dist/sa": bytes.Repeat([]byte("S"), 1500),
		},
		dirs: map[string][]string{
			"6.5.30/disc1/dist": {"sa", "miniroot", "IRIXpatch"},
		},
	}
}

func run(t *testing.T, command string) (string, string, error) {
	t.Helper()
	var out, errb strings.Builder
	err := Run(testFS(), command, &out, &errb)
	return out.String(), errb.String(), err
}

func TestDDStreamsFile(t *testing.T) {
	out, _, err := run(t, "dd if=/6.5.30/disc1/dist/sa bs=512")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1500 || out[0] != 'S' {
		t.Fatalf("dd copied %d bytes", len(out))
	}
}

func TestDDSkipAndCount(t *testing.T) {
	out, _, err := run(t, "dd if=/6.5.30/disc1/dist/sa bs=512 skip=1 count=1")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 512 {
		t.Fatalf("dd skip/count copied %d bytes, want 512", len(out))
	}
}

func TestDDBlockSizeSuffix(t *testing.T) {
	out, _, err := run(t, "dd if=/6.5.30/disc1/dist/sa bs=1k count=1")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1024 {
		t.Fatalf("dd bs=1k count=1 copied %d bytes, want 1024", len(out))
	}
}

func TestDDMissingFile(t *testing.T) {
	_, _, err := run(t, "dd if=/absent bs=512")
	if err == nil {
		t.Fatal("dd of missing file succeeded")
	}
}

func TestLsListsDirectory(t *testing.T) {
	out, _, err := run(t, "ls /6.5.30/disc1/dist")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sa", "miniroot", "IRIXpatch"} {
		if !strings.Contains(out, want) {
			t.Fatalf("ls output missing %q:\n%s", want, out)
		}
	}
}

func TestEcho(t *testing.T) {
	out, _, err := run(t, "echo hello world")
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello world\n" {
		t.Fatalf("echo = %q", out)
	}
}

func TestTrapPrefixIgnored(t *testing.T) {
	out, _, err := run(t, `trap "" 2 3; echo ok`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "ok\n" {
		t.Fatalf("out = %q", out)
	}
}

func TestUnknownCommandRejected(t *testing.T) {
	_, _, err := run(t, "rm -rf /")
	if err == nil {
		t.Fatal("unknown command accepted")
	}
}

func TestRunShellStreamsCommands(t *testing.T) {
	var out, errb strings.Builder
	var logged []string
	stdin := strings.NewReader("echo hello\nls /6.5.30/disc1/dist\ndd if=/6.5.30/disc1/dist/sa bs=512 count=1\n\ntrap \"\" 2 3\n")
	if err := RunShell(testFS(), stdin, &out, &errb, func(l string) { logged = append(logged, l) }); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "hello\n") {
		t.Fatalf("echo output missing: %q", out.String())
	}
	if !strings.Contains(out.String(), "sa") {
		t.Fatalf("ls output missing sa: %q", out.String())
	}
	if len(logged) != 4 { // echo, ls, dd, trap (blank line skipped)
		t.Fatalf("logged %d commands, want 4: %v", len(logged), logged)
	}
}

func TestRunShellContinuesPastUnknownCommand(t *testing.T) {
	var out, errb strings.Builder
	stdin := strings.NewReader("rm -rf /\necho survived\n")
	if err := RunShell(testFS(), stdin, &out, &errb, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "survived") {
		t.Fatalf("shell aborted on unknown command; out=%q err=%q", out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "not supported") {
		t.Fatalf("unknown command not reported to stderr: %q", errb.String())
	}
}

func TestRunShellMarkerProtocol(t *testing.T) {
	var out, errb strings.Builder
	// a real command, then inst's marker wrapper referencing its status
	stdin := strings.NewReader(
		"dd if=/6.5.30/disc1/dist/sa bs=512 count=1\n" +
			"trap : 2 ; ( status=$? ; trap '' 2 ; echo 'o?_InstProc9IsDone\\c' ; echo 'o?_InstProc9IsDone'$status'\\c' 1>&2 )\n")
	if err := RunShell(testFS(), stdin, &out, &errb, nil); err != nil {
		t.Fatal(err)
	}
	// stdout: the dd data (512 bytes of 'S') then the marker with no newline
	if !strings.HasSuffix(out.String(), "o?_InstProc9IsDone") {
		t.Fatalf("stdout missing trailing marker: ...%q", out.String()[max(0, out.Len()-40):])
	}
	if strings.HasSuffix(out.String(), "\n") {
		t.Fatal("marker must not be newline-terminated (\\c)")
	}
	// stderr: dd's records summary, then the marker + status 0 with no
	// trailing newline - inst scans stderr for the marker, so preceding
	// diagnostics are fine as long as marker+status ends the stream.
	if !strings.HasSuffix(errb.String(), "o?_InstProc9IsDone0") {
		t.Fatalf("stderr missing trailing marker+status: %q", errb.String())
	}
	if strings.HasSuffix(errb.String(), "\n") {
		t.Fatal("stderr marker must not be newline-terminated")
	}
}

func TestSplitMarkerFirstProbe(t *testing.T) {
	// the first probe has no real command before the wrapper
	var out, errb strings.Builder
	stdin := strings.NewReader(
		"trap : 2 ; ( status=$? ; trap '' 2 ; echo 'o?_InstProc1IsDone\\c' ; echo 'o?_InstProc1IsDone'$status'\\c' 1>&2 )\n")
	if err := RunShell(testFS(), stdin, &out, &errb, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != "o?_InstProc1IsDone" || errb.String() != "o?_InstProc1IsDone0" {
		t.Fatalf("probe out=%q err=%q", out.String(), errb.String())
	}
}
