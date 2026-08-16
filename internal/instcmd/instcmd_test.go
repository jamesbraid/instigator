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
