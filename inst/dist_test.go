package inst

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jamesbraid/instigator/internal/insttest"
	"github.com/jamesbraid/instigator/unixcompress"
	"github.com/jamesbraid/instigator/unixpack"
)

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func open(t *testing.T, fsys fs.FS) *Dist {
	t.Helper()
	d, err := OpenDist(fsys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func fixture() fstest.MapFS {
	m := insttest.Dist(map[string][]insttest.Entry{
		"eoe": {
			{Path: "usr/a", Sub: "eoe.sw.base", Data: "AAA"},
			{Path: "usr/b", Sub: "eoe.sw.base", Data: "BBBB"},
			{Path: "usr/man/a.1", Sub: "eoe.man.manpages", Data: "man!"},
			{Path: "usr/doc", Sub: "eoe.sw.base", Data: string(insttest.CompressedAB), Compress: true},
			{Path: "usr/def", Sub: "eoe.sw", Data: "zz"}, // no image named: sw
			{Line: "garbage line"},
		},
		"app": {{Path: "opt/x", Sub: "app.sw.base", Data: "xx"}},
	})
	m["orphan.idb"] = &fstest.MapFile{Data: []byte("f 0644 root sys a src orphan.sw.x off(13) size(1)\n")}
	m["eoe.x.idb"] = &fstest.MapFile{Data: []byte("f 0644 root sys a src eoe.x.sw.x off(13) size(1)\n")}
	m["README"] = &fstest.MapFile{Data: []byte("hi")}
	return m
}

func TestOpenDist(t *testing.T) {
	d := open(t, fixture())
	ps := d.Products
	// orphan has no archive, and eoe's images are not eoe.x's, so neither
	// is a product.
	if len(ps) != 2 || ps[0].Name != "app" || ps[1].Name != "eoe" {
		t.Fatalf("products = %+v", ps)
	}
	eoe := ps[1]
	if len(eoe.Bad) != 1 || eoe.Bad[0].Text != "garbage line" {
		t.Errorf("bad = %+v", eoe.Bad)
	}
	// Reads cross images, and the archive's own compression comes off.
	for i, want := range []string{"AAA", "BBBB", "man!", "ab", "zz"} {
		r, err := d.Open("eoe", eoe.Entries[i])
		if err != nil {
			t.Fatalf("%s: %v", eoe.Entries[i].Path, err)
		}
		if got := readAll(t, r); got != want {
			t.Errorf("%s = %q, want %q", eoe.Entries[i].Path, got, want)
		}
		r.Close()
	}
	if d := open(t, fstest.MapFS{"x.idb": {}, "y": {}}); len(d.Products) != 0 {
		t.Errorf("products without archives = %+v", d.Products)
	}
}

// A missing archive fails its own entries, every time, and no others.
func TestMissingArchive(t *testing.T) {
	fsys := fixture()
	delete(fsys, "eoe.man")
	d := open(t, fsys)
	eoe := d.Products[1]
	for range 2 {
		if _, err := d.Open("eoe", eoe.Entries[2]); err == nil || !strings.Contains(err.Error(), "eoe.man") {
			t.Errorf("missing archive: err = %v", err)
		}
	}
	if _, err := d.Open("eoe", eoe.Entries[0]); err != nil {
		t.Errorf("entry in a present archive: %v", err)
	}
}

// noReaderAt hides ReadAt, which Open needs to read records by section.
type noReaderAt struct{ fs.FS }

type plainFile struct{ fs.File }

func (n noReaderAt) Open(name string) (fs.File, error) {
	f, err := n.FS.Open(name)
	return plainFile{f}, err
}

func (n noReaderAt) ReadDir(name string) ([]fs.DirEntry, error) { return fs.ReadDir(n.FS, name) }

func TestOpenWithoutReaderAt(t *testing.T) {
	d := open(t, noReaderAt{fixture()})
	if _, err := d.Open("eoe", d.Products[1].Entries[1]); err == nil || !strings.Contains(err.Error(), "ReaderAt") {
		t.Errorf("err = %v, want one naming ReaderAt", err)
	}
}

// Some products list no off(). Their records are found by walking each
// image in .idb order; an explicit off() restarts the walk, and a walk
// that lands on the wrong record is an error, never the wrong bytes.
func TestOffsetsWithoutOff(t *testing.T) {
	sw, so := insttest.Archive([2]string{"usr/a", "AAA"}, [2]string{"usr/bb", ""}, [2]string{"usr/c", "CC"},
		[2]string{"x", "1"}, [2]string{"y", "22"}, [2]string{"z", "333"})
	man, mo := insttest.Archive([2]string{"usr/man/a.1", "man!"}, [2]string{"usr/man/b.1", "page"})
	noOff := func(path, sub string, size int) string {
		return fmt.Sprintf("f 0644 root sys %s src %s size(%d) f(123456789)\n", path, sub, size)
	}
	idb := "d 0755 root sys usr src p.sw.base\n" +
		noOff("usr/a", "p.sw.base", 3) +
		noOff("usr/man/a.1", "p.man.pages", 4) +
		"l 0777 root sys usr/l src p.sw.base symval(a)\n" +
		noOff("usr/bb", "p.sw.base", 0) +
		noOff("usr/man/b.1", "p.man.pages", 4) +
		noOff("usr/c", "p.sw.base", 2) +
		noOff("x", "p.sw.base", 5) + // wrong size: the walk misses y
		noOff("y", "p.sw.base", 2) +
		fmt.Sprintf("f 0644 root sys z src p.sw.base off(%d) size(3)\n", so[5]) +
		noOff("extra", "p.sw.base", 0)
	d := open(t, fstest.MapFS{"p.idb": {Data: []byte(idb)}, "p.sw": {Data: sw}, "p.man": {Data: man}})
	p := d.Products[0]
	cases := []struct {
		off  int64
		want string // "" for no record or an error; "-" not opened
	}{
		{-1, ""}, {so[0], "AAA"}, {mo[0], "man!"}, {-1, ""}, {so[1], ""}, {mo[1], "page"}, {so[2], "CC"},
		{so[3], "-"}, {so[3] + 2 + 1 + 5, ""}, {so[5], "333"},
		{int64(len(sw)), ""}, // the walk resumes after z, at the end
	}
	for i, c := range cases {
		e := p.Entries[i]
		if got := e.Offset; got != c.off {
			t.Errorf("%s: offset %d, want %d", e.Path, got, c.off)
		}
		if e.Type != 'f' || e.Path == "usr/bb" || c.want == "-" {
			continue
		}
		r, err := d.Open("p", e)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s: opened a record that is not its own", e.Path)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", e.Path, err)
		}
		if got := readAll(t, r); got != c.want {
			t.Errorf("%s = %q, want %q", e.Path, got, c.want)
		}
	}
}

func TestUnwrap(t *testing.T) {
	packed, z := insttest.Packed('A'), insttest.CompressedAB
	cases := []struct {
		name, wantName, wantData string
		in                       []byte
	}{
		{"man.z", "man", "AAAA", packed},
		{"notes.Z", "notes", "ab", z},
		// Without matching magic the data passes through as installed.
		{"plain.z", "plain.z", "not packed", []byte("not packed")},
		{"plain.Z", "plain.Z", "not compressed", []byte("not compressed")},
		{"wrong.Z", "wrong.Z", string(packed), packed},
		{"wrong.z", "wrong.z", string(z), z},
		{"file", "file", string(packed), packed},
		{"short.z", "short.z", "\x1f", []byte{0x1f}},
		{"empty.Z", "empty.Z", "", nil},
	}
	for _, c := range cases {
		name, out, err := Unwrap(c.name, bytes.NewReader(c.in))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := readAll(t, out); name != c.wantName || got != c.wantData {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, name, got, c.wantName, c.wantData)
		}
	}
	if _, _, err := Unwrap("x.z", bytes.NewReader([]byte{0x1f, 0x1e, 0})); !errors.Is(err, unixpack.ErrCorrupt) {
		t.Errorf("corrupt pack: err = %v", err)
	}
	if _, _, err := Unwrap("x.Z", bytes.NewReader([]byte{0x1f, 0x9d})); !errors.Is(err, unixcompress.ErrCorrupt) {
		t.Errorf("corrupt compress: err = %v", err)
	}
}

// bounded fails any read of the archive p.sw outside its signature and
// [lo, hi), to show a record is read by section and the archive never whole.
type bounded struct {
	fstest.MapFS
	lo, hi int64
}

type boundedFile struct {
	fs.File
	b *bounded
}

func (b *bounded) Open(name string) (fs.File, error) {
	f, err := b.MapFS.Open(name)
	if err != nil || name != "p.sw" {
		return f, err
	}
	return boundedFile{f, b}, nil
}

func (f boundedFile) ReadAt(p []byte, off int64) (int, error) {
	if end := off + int64(len(p)); end > 13 && (off < f.b.lo || end > f.b.hi) {
		return 0, fmt.Errorf("read of %d bytes at %d outside [%d,%d)", len(p), off, f.b.lo, f.b.hi)
	}
	return f.File.(io.ReaderAt).ReadAt(p, off)
}

func TestRecordBounds(t *testing.T) {
	data, offs := insttest.Archive([2]string{"usr/pad", strings.Repeat("x", 5000)},
		[2]string{"usr/share/a", "hello"}, [2]string{"usr/pad2", strings.Repeat("y", 5000)})
	end, off := int64(len(data)), offs[1]
	cases := []struct {
		name, path string
		off        int64
		attrs      string
		want       string // stored bytes; "" for an error
	}{
		{"stored", "usr/share/a", off, "size(5)", "hello"},
		{"name mismatch", "usr/share/b", off, "size(5)", ""},
		{"inside signature", "usr/share/a", 3, "size(5)", ""},
		{"offset at end", "usr/share/a", end, "size(5)", ""},
		{"offset past end", "usr/pad2", end + 100, "size(5)", ""},
		{"size past end", "usr/pad2", offs[2], "size(5001)", ""},
		{"size to end", "usr/pad2", offs[2], "size(5000)", strings.Repeat("y", 5000)},
		// Values near the int64 limit must not wrap past the checks.
		{"huge size", "usr/share/a", off, fmt.Sprintf("size(%d)", int64(math.MaxInt64)), ""},
		{"huge cmpsize", "usr/share/a", off, fmt.Sprintf("size(5) cmpsize(%d)", int64(math.MaxInt64-10)), ""},
		{"huge offset", "usr/share/a", math.MaxInt64, "size(5)", ""},
		{"huge offset minus one", "usr/share/a", math.MaxInt64 - 1, "size(5)", ""},
	}
	for _, c := range cases {
		idb := fmt.Sprintf("f 0644 root sys %s src p.sw.base off(%d) %s\n", c.path, c.off, c.attrs)
		b := &bounded{MapFS: fstest.MapFS{"p.idb": {Data: []byte(idb)}, "p.sw": {Data: data}}, lo: 0, hi: end}
		if c.want != "" {
			b.lo, b.hi = c.off, c.off+2+int64(len(c.path))+int64(len(c.want))
		}
		d := open(t, b)
		r, err := d.Open("p", d.Products[0].Entries[0])
		if c.want == "" {
			if err == nil || !strings.Contains(err.Error(), c.path) {
				t.Errorf("%s: err = %v, want one naming %s", c.name, err, c.path)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := readAll(t, r); got != c.want {
			t.Errorf("%s: got %q", c.name, got)
		}
	}
}

func TestBadArchive(t *testing.T) {
	idb := []byte("f 0644 root sys a src p.sw.base off(13) size(1)\n")
	for _, s := range []string{"", "im001", "xx001V630P00\x00rest", "im001V630P00Xrest", insttest.Signature + "\xff\xffa"} {
		d := open(t, fstest.MapFS{"p.idb": {Data: idb}, "p.sw": {Data: []byte(s)}})
		if _, err := d.Open("p", d.Products[0].Entries[0]); err == nil {
			t.Errorf("%q: want error", s)
		}
	}
}
