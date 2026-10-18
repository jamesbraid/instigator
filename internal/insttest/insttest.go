// Package insttest builds small synthetic inst distributions for tests: archive
// images, .idb manifests and the products they make up. No SGI-derived bytes
// are involved.
package insttest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// Signature opens every archive image.
const Signature = "im001V630P00\x00"

// Packed returns pack(1) data for four copies of c.
func Packed(c byte) []byte { return []byte{0x1f, 0x1e, 0, 0, 0, 4, 1, 0, c, 0x08} }

// CompressedAB is compress(1) data, 12-bit block mode, for "ab".
var CompressedAB = []byte{0x1f, 0x9d, 0x8c, 0x61, 0xc4, 0x00}

// Archive returns an archive image holding records of {path, data} in order,
// and the offset of each.
func Archive(recs ...[2]string) ([]byte, []int64) {
	b := bytes.NewBufferString(Signature)
	var offs []int64
	for _, r := range recs {
		offs = append(offs, int64(b.Len()))
		b.Write([]byte{byte(len(r[0]) >> 8), byte(len(r[0]))})
		b.WriteString(r[0])
		b.WriteString(r[1])
	}
	return b.Bytes(), offs
}

// Entry is one .idb line of a product built by Dist.
type Entry struct {
	Type     byte   // 'f' when zero
	Mode     string // "0644" when empty
	Path     string
	Sub      string // product.image.subsystem
	Data     string // a file's record body
	Symval   string // a link's target
	Mach     string
	Compress bool   // Data is compress(1) output; record cmpsize()
	Missing  bool   // list the file but leave its record out of the archive
	Line     string // a raw .idb line, used instead of all of the above
}

// Dist lays products out as a distribution directory: for each product P a
// description file P, the manifest P.idb, and an archive P.<image> for every
// image its entries name.
func Dist(prods map[string][]Entry) fstest.MapFS {
	m := fstest.MapFS{}
	for prod, ents := range prods {
		images := map[string]*bytes.Buffer{}
		var idb strings.Builder
		for _, e := range ents {
			if e.Line != "" {
				idb.WriteString(e.Line + "\n")
				continue
			}
			typ, mode := e.Type, e.Mode
			if typ == 0 {
				typ = 'f'
			}
			if mode == "" {
				mode = "0644"
			}
			// Every image an entry names gets an archive, so a product
			// of links alone still has one.
			img := strings.Split(e.Sub, ".")[1]
			b := images[img]
			if b == nil {
				b = bytes.NewBufferString(Signature)
				images[img] = b
			}
			attrs := ""
			switch typ {
			case 'f':
				off := b.Len()
				if !e.Missing {
					b.Write([]byte{byte(len(e.Path) >> 8), byte(len(e.Path))})
					b.WriteString(e.Path)
					b.WriteString(e.Data)
				}
				attrs = fmt.Sprintf(" off(%d) size(%d)", off, len(e.Data))
				if e.Compress {
					attrs += fmt.Sprintf(" cmpsize(%d)", len(e.Data))
				}
			case 'l':
				attrs = " symval(" + e.Symval + ")"
			}
			if e.Mach != "" {
				attrs += " mach(" + e.Mach + ")"
			}
			fmt.Fprintf(&idb, "%c %s root sys %s src %s%s\n", typ, mode, e.Path, e.Sub, attrs)
		}
		m[prod] = &fstest.MapFile{Data: []byte("description")}
		m[prod+".idb"] = &fstest.MapFile{Data: []byte(idb.String())}
		for img, b := range images {
			m[prod+"."+img] = &fstest.MapFile{Data: b.Bytes()}
		}
	}
	return m
}

// WriteDir writes the files of fsys under dir and returns dir.
func WriteDir(t testing.TB, dir string, fsys fstest.MapFS) string {
	t.Helper()
	for name, f := range fsys {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
