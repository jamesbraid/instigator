package unixcompress

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

var update = flag.Bool("update", false, "regenerate testdata/*.Z with compress(1)")

func random(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// lowEntropy draws from a four-symbol alphabet: it compresses well and
// fills the string table, forcing the code width to grow.
func lowEntropy(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = "acgt"[r.Intn(4)]
	}
	return b
}

func english() []byte {
	const para = "The quick brown fox jumps over the lazy dog. Pack my box with " +
		"five dozen liquor jugs. How vexingly quick daft zebras jump! " +
		"Sphinx of black quartz, judge my vow.\n"
	var b strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&b, "%d: %s", i, para)
	}
	return []byte(b.String())
}

// samples are the committed fixtures, testdata/<name>.b<width>.Z, each
// compress -b <width> of its synthetic data.
var samples = []struct {
	name  string
	width int
	data  func() []byte
}{
	{"short", 12, func() []byte { return []byte(strings.Repeat("hello, compress. ", 12)) }},
	{"english", 16, english},
	{"random", 16, func() []byte { return random(3000, 1) }},
	{"repetitive", 12, func() []byte { return lowEntropy(40000, 2) }},
	{"repetitive", 16, func() []byte { return lowEntropy(40000, 2) }},
	{"onebyte", 16, func() []byte { return []byte("x") }},
	{"empty", 16, func() []byte { return nil }},
	// A full 12-bit table that then turns incompressible, so compress
	// emits a block-mode clear code.
	{"clear", 12, func() []byte { return append(lowEntropy(30000, 6), random(20000, 7)...) }},
}

func fixturePath(name string, width int) string {
	return filepath.Join("testdata", fmt.Sprintf("%s.b%d.Z", name, width))
}

func runCompress(t *testing.T, data []byte, width int) []byte {
	t.Helper()
	cmd := exec.Command("compress", "-c", "-b", fmt.Sprint(width))
	cmd.Stdin = bytes.NewReader(data)
	out, err := cmd.Output()
	// Exit status 2 means the output grew; the stream is still complete.
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) && ee.ExitCode() == 2 {
		err = nil
	}
	if err != nil {
		t.Fatalf("compress -b %d: %v", width, err)
	}
	return out
}

func decode(z []byte) ([]byte, error) {
	r, err := NewReader(bytes.NewReader(z))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func readFixture(t *testing.T, name string, width int) []byte {
	t.Helper()
	z, err := os.ReadFile(fixturePath(name, width))
	if err != nil {
		t.Fatal(err)
	}
	return z
}

func TestFixtures(t *testing.T) {
	for _, s := range samples {
		t.Run(fmt.Sprintf("%s/b%d", s.name, s.width), func(t *testing.T) {
			if *update {
				if err := os.WriteFile(fixturePath(s.name, s.width), runCompress(t, s.data(), s.width), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := decode(readFixture(t, s.name, s.width))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, s.data()) {
				t.Fatalf("got %d bytes, want %d", len(got), len(s.data()))
			}
		})
	}
}

// TestAgainstCompress decodes compress(1) output for every sample at both
// ends of the accepted width range, plus an input too large to commit that
// clears the table at 16 bits.
func TestAgainstCompress(t *testing.T) {
	if _, err := exec.LookPath("compress"); err != nil {
		t.Skip("compress(1) not installed")
	}
	inputs := map[string][]byte{"huge": append(lowEntropy(500000, 3), random(500000, 4)...)}
	for _, s := range samples {
		inputs[s.name] = s.data()
	}
	for name, want := range inputs {
		for _, w := range []int{12, 16} {
			got, err := decode(runCompress(t, want, w))
			if err != nil || !bytes.Equal(got, want) {
				t.Errorf("%s/b%d: got %d bytes, err %v; want %d bytes", name, w, len(got), err, len(want))
			}
		}
	}
}

func TestSmallReads(t *testing.T) {
	r, err := NewReader(iotest.OneByteReader(bytes.NewReader(readFixture(t, "repetitive", 12))))
	if err != nil {
		t.Fatal(err)
	}
	if err := iotest.TestReader(r, lowEntropy(40000, 2)); err != nil {
		t.Fatal(err)
	}
}

// Hand-built streams. The header is 1f 9d and a flags byte: 0x80 is block
// mode and the low bits the maximum width. Codes are packed LSB first.
func TestStreams(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string // decoded output, when err is false
		err  bool
	}{
		{"empty", nil, "", true},
		{"one byte", []byte{0x1f}, "", true},
		{"no flags", []byte{0x1f, 0x9d}, "", true},
		{"bad magic", []byte{0x1f, 0x8b, 0x90}, "", true},
		// FreeBSD accepts maximum widths 12 to 16 only.
		{"width 0", []byte{0x1f, 0x9d, 0x80}, "", true},
		{"width 9", []byte{0x1f, 0x9d, 0x89}, "", true},
		{"width 11", []byte{0x1f, 0x9d, 0x8b}, "", true},
		{"width 17", []byte{0x1f, 0x9d, 0x91}, "", true},
		{"width 31", []byte{0x1f, 0x9d, 0x9f}, "", true},
		// 'a' then 0x1ff, far beyond the table.
		{"code beyond table", []byte{0x1f, 0x9d, 0x8c, 0x61, 0xfe, 0x03}, "", true},
		// 257 with no previous code to build it from.
		{"first code undefined", []byte{0x1f, 0x9d, 0x8c, 0x01, 0x01}, "", true},
		// A final group too short for one code still yields a code.
		{"short final group", []byte{0x1f, 0x9d, 0x8c, 'q'}, "q", false},
		{"no block mode", []byte{0x1f, 0x9d, 0x0c, 0x61, 0xc4, 0x00}, "ab", false},
	}
	for _, c := range cases {
		got, err := decode(c.in)
		if c.err != errors.Is(err, ErrCorrupt) || !c.err && string(got) != c.want {
			t.Errorf("%s: got %q, err %v", c.name, got, err)
		}
	}
}

// The format has no end marker, so a cut stream decodes as far as it goes.
// A cut on a group boundary (nine bytes of 9-bit codes) yields a prefix.
func TestTruncation(t *testing.T) {
	z := readFixture(t, "short", 12)
	got, err := decode(z[:3+9])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || !bytes.HasPrefix(samples[0].data(), got) {
		t.Fatalf("got %q, not a prefix of the original", got)
	}
}

func TestIsCompressed(t *testing.T) {
	if !IsCompressed([]byte(Magic + "\x90")) {
		t.Error("magic not recognised")
	}
	for _, b := range [][]byte{nil, {0x1f}, {0x1f, 0x8b}, {0x9d, 0x1f}} {
		if IsCompressed(b) {
			t.Errorf("%x accepted", b)
		}
	}
}

func FuzzReader(f *testing.F) {
	files, _ := filepath.Glob("testdata/*.Z")
	for _, p := range files {
		if b, err := os.ReadFile(p); err == nil {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		if r, err := NewReader(bytes.NewReader(in)); err == nil {
			// A tiny stream can expand a lot, so cap the output.
			io.Copy(io.Discard, io.LimitReader(r, 64<<20))
		}
	})
}
