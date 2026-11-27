package inst

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jamesbraid/instigator/internal/insttest"
)

// The vectors are the output of coreutils sum -r, which the manifest's sum()
// matches, kept here so the test needs no outside tool.
func TestSumBytes(t *testing.T) {
	counting := make([]byte, 512)
	for i := range counting {
		counting[i] = byte(i)
	}
	for _, c := range []struct {
		name string
		data []byte
		want uint16
	}{
		{"empty", nil, 0},
		{"text", []byte("hello world\n"), 3762},
		{"AAA", []byte("AAA"), 49265},
		{"every byte twice", counting, 1024},
		// AUTHORS files of the nekoware lzo (GPL) and expat 2.2.6 (MIT) packages, with
		// the sum() their manifests give.
		{"lzo AUTHORS", []byte("Authors of the LZO data compression library:\n\nMarkus F.X.J. Oberhumer.  Invented, designed and implemented LZO.\n"), 6412},
		{"expat AUTHORS", []byte("Expat is brought to you by:\n\nClark Cooper\nFred L. Drake, Jr.\nGreg Stein\nJames Clark\nKarl Waclawek\nRhodri James\nSebastian Pipping\nSteven Solie\n"), 29946},
		{"0xff past 64 KiB", bytes.Repeat([]byte{0xff}, 65537), 60412},
	} {
		if got := sumBytes(0, c.data); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
		// Piecewise, as a reader delivers it.
		var got uint16
		for rest := c.data; len(rest) > 0; rest = rest[min(len(rest), 7):] {
			got = sumBytes(got, rest[:min(len(rest), 7)])
		}
		if got != c.want {
			t.Errorf("%s in sevens: %d, want %d", c.name, got, c.want)
		}
	}
}

func TestEntrySum(t *testing.T) {
	for _, c := range []struct {
		attrs string
		want  uint16
		ok    bool
	}{
		{"sum(0)", 0, true},
		{"sum(50430)", 50430, true},
		{"sum(65535)", 65535, true},
		{"sum(65536)", 0, false},
		{"sum(-1)", 0, false},
		{"sum(0x10)", 0, false},
		{"sum()", 0, false},
		{"size(3)", 0, false},
	} {
		e := parseOne(t, "f 0644 root sys a a "+c.attrs+" p.sw.x")
		if got, ok := e.Sum(); got != c.want || ok != c.ok {
			t.Errorf("%s: %d, %v; want %d, %v", c.attrs, got, ok, c.want, c.ok)
		}
	}
}

// A mismatching record reads in full, then ends with a *ChecksumError.
// Matching records, and ones with no usable sum(), end with io.EOF.
func TestOpenChecksSum(t *testing.T) {
	sw, so := insttest.Archive(
		[2]string{"good", "AAA"}, [2]string{"bad", "AAB"}, [2]string{"none", "AAB"},
		[2]string{"junk", "AAB"}, [2]string{"z", string(insttest.CompressedAB)}, [2]string{"zbad", string(insttest.CompressedAB)})
	line := func(i int, path, attrs string) string {
		return fmt.Sprintf("f 0644 root sys %s src p.sw.base off(%d) %s\n", path, so[i], attrs)
	}
	idb := line(0, "good", "size(3) sum(49265)") + line(1, "bad", "size(3) sum(49265)") +
		line(2, "none", "size(3)") + line(3, "junk", "size(3) sum(65536)") +
		line(4, "z", "size(2) cmpsize(6) sum(32914)") + line(5, "zbad", "size(2) cmpsize(6) sum(32915)")
	d := open(t, fstest.MapFS{"p.idb": {Data: []byte(idb)}, "p.sw": {Data: sw}})
	for i, c := range []struct {
		data string
		bad  bool
	}{{"AAA", false}, {"AAB", true}, {"AAB", false}, {"AAB", false}, {"ab", false}, {"ab", true}} {
		e := d.Products[0].Entries[i]
		r, err := d.Open("p", e)
		if err != nil {
			t.Fatalf("%s: %v", e.Path, err)
		}
		b, err := io.ReadAll(r)
		if string(b) != c.data {
			t.Errorf("%s = %q, want %q", e.Path, b, c.data)
		}
		var ce *ChecksumError
		if bad := errors.As(err, &ce); bad != c.bad || (err != nil) != c.bad {
			t.Errorf("%s: err = %v, want checksum error %v", e.Path, err, c.bad)
		}
		if ce != nil && (ce.Path != e.Path || ce.Want == ce.Got) {
			t.Errorf("%s: %+v", e.Path, ce)
		}
		if err != nil && !strings.Contains(err.Error(), e.Path) {
			t.Errorf("%s: error does not name the file: %v", e.Path, err)
		}
		r.Close()
	}
}
