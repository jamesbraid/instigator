package instcmd

import (
	"bytes"
	"hash/fnv"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/internal/logging"
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

// ReadDir special-cases the root ("", from a leading-slash path with
// nothing after it): a fixture never lists it explicitly, but the real
// vfs.Tree always treats the root as a directory of its top-level
// names, so a shell test starting its cwd at "/" needs the same here.
func (f *fakeFS) ReadDir(path string) ([]string, error) {
	if path == "" {
		return f.topLevel(), nil
	}
	names, ok := f.dirs[path]
	if !ok {
		return nil, ErrNotFound
	}
	return names, nil
}

func (f *fakeFS) topLevel() []string {
	seen := map[string]bool{}
	var names []string
	add := func(p string) {
		seg := strings.SplitN(p, "/", 2)[0]
		if !seen[seg] {
			seen[seg] = true
			names = append(names, seg)
		}
	}
	for p := range f.files {
		add(p)
	}
	for p := range f.dirs {
		add(p)
	}
	sort.Strings(names)
	return names
}

// Stat synthesizes plausible metadata from the same files/dirs maps
// Open/ReadDir already use: real values aren't the point of this test
// double, a stable inode and correct IsDir are.
func (f *fakeFS) Stat(path string) (FileInfo, error) {
	if path == "" {
		return FileInfo{Ino: 2, IsDir: true, Perm: 0o755, Nlink: 2, Size: 512, Mtime: fakeMtime}, nil
	}
	if _, ok := f.dirs[path]; ok {
		return FileInfo{Ino: fakeIno(path), IsDir: true, Perm: 0o755, Nlink: 2, Size: 512, Mtime: fakeMtime}, nil
	}
	if b, ok := f.files[path]; ok {
		return FileInfo{Ino: fakeIno(path), IsDir: false, Perm: 0o644, Nlink: 1, Size: int64(len(b)), Mtime: fakeMtime}, nil
	}
	return FileInfo{}, ErrNotFound
}

var fakeMtime = time.Date(2026, time.January, 2, 15, 4, 0, 0, time.UTC)

// fakeIno derives a stable, non-zero, path-distinct inode number so
// tests can assert "an inode was reported" and "two different paths
// don't collide" without a fixture having to assign one by hand.
func fakeIno(path string) uint64 {
	h := fnv.New32a()
	h.Write([]byte(path))
	return uint64(h.Sum32())%1_000_000 + 1
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
	var out, errb, logbuf strings.Builder
	stdin := strings.NewReader("echo hello\nls /6.5.30/disc1/dist\ndd if=/6.5.30/disc1/dist/sa bs=512 count=1\n\ntrap \"\" 2 3\n")
	logger := logging.New(&logbuf, logging.LevelDebug)
	if err := RunShell(testFS(), stdin, &out, &errb, logger); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "hello\n") {
		t.Fatalf("echo output missing: %q", out.String())
	}
	if !strings.Contains(out.String(), "sa") {
		t.Fatalf("ls output missing sa: %q", out.String())
	}
	logged := strings.Count(logbuf.String(), "instcmd: rsh-sh: ")
	if logged != 4 { // echo, ls, dd, trap (blank line skipped)
		t.Fatalf("logged %d commands, want 4:\n%s", logged, logbuf.String())
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

func TestShellFgrepFiltersMachLines(t *testing.T) {
	// inst reads a product's machine-conditional lines with
	// `dd if=<idb> | fgrep ' mach('`; without fgrep the pipe is empty and
	// inst reports "No valid products in distribution".
	var out, errb strings.Builder
	stdin := strings.NewReader("echo 'x mach(IP30) y\\nplain line\\nz mach(IP32)' | fgrep ' mach('\n")
	if err := RunShell(testFS(), stdin, &out, &errb, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "x mach(IP30) y") || !strings.Contains(got, "z mach(IP32)") {
		t.Fatalf("fgrep dropped matching lines: %q", got)
	}
	if strings.Contains(got, "plain line") {
		t.Fatalf("fgrep kept a non-matching line: %q", got)
	}
}

func TestShellGrepExitStatus(t *testing.T) {
	// inst keys on the pipe's exit status: fgrep exits 1 when nothing
	// matched, 0 when something did.
	var out, errb strings.Builder
	stdin := strings.NewReader(
		"echo 'nothing here' | fgrep ' mach(' ; echo \"miss=$?\"\n" +
			"echo 'has mach( in it' | fgrep ' mach(' ; echo \"hit=$?\"\n")
	if err := RunShell(testFS(), stdin, &out, &errb, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "miss=1") {
		t.Fatalf("fgrep with no match must exit 1: %q", got)
	}
	if !strings.Contains(got, "hit=0") {
		t.Fatalf("fgrep with a match must exit 0: %q", got)
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
