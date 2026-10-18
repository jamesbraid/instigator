package unixpack

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

var update = flag.Bool("update", false, "regenerate testdata/*.z with the pack(1) port in pack_test.go")

// samples are the committed fixtures, testdata/<name>.z, each the packed
// form of its synthetic data. pack(1) refuses input of one byte value or
// none, so those fixtures are one-level trees built by single.
var samples = []struct {
	name string
	data func() []byte
}{
	{"english", func() []byte {
		return []byte(strings.Repeat("The quick brown fox jumps over the lazy dog. Pack my box with five dozen liquor jugs.\n", 40))
	}},
	{"all256", func() []byte {
		b := make([]byte, 256*5)
		for i := range b {
			b[i] = byte(i)
		}
		return b
	}},
	{"repeat", func() []byte { return bytes.Repeat([]byte{'x'}, 1000) }},
	{"one", func() []byte { return []byte{'q'} }},
	{"empty", func() []byte { return nil }},
	// A skewed distribution gives a deep tree.
	{"skewed", func() []byte {
		rng := rand.New(rand.NewSource(1))
		b := make([]byte, 20000)
		for i := range b {
			b[i] = byte(rng.ExpFloat64() * 6)
		}
		return b
	}},
}

func fixturePath(name string) string { return filepath.Join("testdata", name+".z") }

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func unpack(in []byte) ([]byte, error) {
	r, err := NewReader(bytes.NewReader(in))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestFixtures(t *testing.T) {
	for _, s := range samples {
		t.Run(s.name, func(t *testing.T) {
			if *update {
				b, err := pack(s.data())
				if errors.Is(err, errTrivial) {
					b, err = single(s.data()), nil
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fixturePath(s.name), b, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := unpack(readFixture(t, s.name))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, s.data()) {
				t.Fatalf("got %d bytes, want %d", len(got), len(s.data()))
			}
		})
	}
}

// TestAgainstGzip checks every fixture with gzip -d, which also reads
// pack(1) data, so the fixtures do not rest on this decoder alone.
func TestAgainstGzip(t *testing.T) {
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not installed")
	}
	for _, s := range samples {
		cmd := exec.Command("gzip", "-dc")
		cmd.Stdin = bytes.NewReader(readFixture(t, s.name))
		got, err := cmd.Output()
		if err != nil || !bytes.Equal(got, s.data()) {
			t.Errorf("%s: gzip -dc gave %d bytes, err %v; want %d bytes", s.name, len(got), err, len(s.data()))
		}
	}
}

func TestSmallReads(t *testing.T) {
	r, err := NewReader(iotest.OneByteReader(bytes.NewReader(readFixture(t, "english"))))
	if err != nil {
		t.Fatal(err)
	}
	if err := iotest.TestReader(r, samples[0].data()); err != nil {
		t.Fatal(err)
	}
}

// oneLevel is a one-level tree: code 0 is sym and code 1 is EOB. The stored
// length is n and the coded bits are data.
func oneLevel(sym byte, n int, data ...byte) []byte {
	return append([]byte{0x1f, 0x1e, byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n), 1, 0, sym}, data...)
}

// single is the one-level stream for d, which holds one byte value or none.
func single(d []byte) []byte {
	sym := byte(0)
	if len(d) > 0 {
		sym = d[0]
	}
	data := make([]byte, len(d)/8+1)
	data[len(d)/8] = 0x80 >> (len(d) % 8)
	return oneLevel(sym, len(d), data...)
}

func TestStreams(t *testing.T) {
	good := readFixture(t, "english")
	mut := func(f func(b []byte)) []byte {
		b := bytes.Clone(good)
		f(b)
		return b
	}
	cases := []struct {
		name string
		in   []byte
		want string // decoded output, when err is false
		err  bool
	}{
		{"empty", nil, "", true},
		{"short magic", []byte{0x1f}, "", true},
		{"short header", good[:5], "", true},
		{"bad magic", mut(func(b []byte) { b[1] = 0x8b }), "", true},
		{"zero levels", mut(func(b []byte) { b[6] = 0 }), "", true},
		{"levels 25", mut(func(b []byte) { b[6] = 25 }), "", true},
		{"levels 255", mut(func(b []byte) { b[6] = 255 }), "", true},
		{"symbol table overflow", append([]byte{0x1f, 0x1e, 0, 0, 0, 9, 2, 255, 255}, make([]byte, 600)...), "", true},
		{"no counts", good[:7], "", true},
		{"truncated counts", good[:9], "", true},
		{"truncated symbols", good[:7+int(good[6])+3], "", true},
		{"truncated data", good[:len(good)-5], "", true},
		{"stored length too big", mut(func(b []byte) { b[2], b[3], b[4], b[5] = 0, 1, 0, 0 }), "", true},
		// Input after the end-of-block symbol is never read.
		{"trailing garbage", append(bytes.Clone(good), "garbage"...), string(samples[0].data()), false},
		// Bits 0101 0000: a, EOB before the stored length, which writes a
		// byte (a zero here, past the C's table) and goes on, a, EOB.
		{"early EOB", oneLevel('a', 3, 0x50), "a\x00a", false},
		// Without a final EOB the data may end at end of input, but only
		// once exactly the stored length is out.
		{"ends at EOF", oneLevel('a', 8, 0x50), "a\x00a\x00aaaa", false},
		{"ends at EOF one short", oneLevel('a', 9, 0x50), "", true},
		{"ends at EOF one over", oneLevel('a', 7, 0x50), "", true},
	}
	for _, c := range cases {
		got, err := unpack(c.in)
		if c.err != errors.Is(err, ErrCorrupt) || !c.err && string(got) != c.want {
			t.Errorf("%s: got %q, err %v", c.name, got, err)
		}
	}
}

// Flipping any byte of the coded data yields an error or wrong output,
// never a panic, and at least one flip must be caught.
func TestCorruptBitstream(t *testing.T) {
	packed := readFixture(t, "english")
	detected := 0
	for i := 7 + int(packed[6]); i < len(packed); i++ {
		b := bytes.Clone(packed)
		b[i] ^= 0xff
		if _, err := unpack(b); err != nil {
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("err = %v", err)
			}
			detected++
		}
	}
	if detected == 0 {
		t.Fatal("no corrupted byte was detected")
	}
}

func TestIsPacked(t *testing.T) {
	if !IsPacked([]byte(Magic + "\x00")) {
		t.Error("magic not recognised")
	}
	for _, b := range [][]byte{nil, {0x1f}, {0x1f, 0x8b}, {0x1f, 0x9d}, {0x1e, 0x1f}} {
		if IsPacked(b) {
			t.Errorf("%x accepted", b)
		}
	}
}

func FuzzReader(f *testing.F) {
	files, _ := filepath.Glob("testdata/*.z")
	for _, p := range files {
		if b, err := os.ReadFile(p); err == nil {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		if r, err := NewReader(bytes.NewReader(in)); err == nil {
			io.Copy(io.Discard, io.LimitReader(r, 64<<20))
		}
	})
}
