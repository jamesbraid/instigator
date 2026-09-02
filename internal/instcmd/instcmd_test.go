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

func TestRunShellStreamsCommands(t *testing.T) {
	var out, errb, logbuf strings.Builder
	stdin := strings.NewReader("echo hello\nls /6.5.30/disc1/dist\ndd if=/6.5.30/disc1/dist/sa bs=512 count=1\n\ntrap \"\" 2 3\n")
	logger := logging.New(&logbuf, logging.LevelDebug)
	if err := RunShell(testFS(), stdin, &out, &errb, logger, nil); err != nil {
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

func TestShellFgrepFiltersMachLines(t *testing.T) {
	// inst reads a product's machine-conditional lines with
	// `dd if=<idb> | fgrep ' mach('`; without fgrep the pipe is empty and
	// inst reports "No valid products in distribution".
	var out, errb strings.Builder
	stdin := strings.NewReader("echo 'x mach(IP30) y\\nplain line\\nz mach(IP32)' | fgrep ' mach('\n")
	if err := RunShell(testFS(), stdin, &out, &errb, nil, nil); err != nil {
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
	if err := RunShell(testFS(), stdin, &out, &errb, nil, nil); err != nil {
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
	if err := RunShell(testFS(), stdin, &out, &errb, nil, nil); err != nil {
		t.Fatal(err)
	}
	if out.String() != "o?_InstProc1IsDone" || errb.String() != "o?_InstProc1IsDone0" {
		t.Fatalf("probe out=%q err=%q", out.String(), errb.String())
	}
}
