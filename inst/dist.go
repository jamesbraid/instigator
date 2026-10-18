// Package inst reads IRIX inst distributions: the products a distribution
// directory holds, each product's .idb manifest, and the installed bytes of
// every manifest entry.
//
// A product P is a manifest P.idb with at least one sibling archive image
// P.<image> (eoe.sw, eoe.man and so on). Everything is read through an
// fs.FS rooted at the distribution directory, so a directory on disk, a test
// fixture and a remote file tree all work the same way.
//
// An archive image is a 13-byte signature, the text "im001", a 7-byte
// version tag and a NUL, then records back to back. A record is a
// big-endian 16-bit name length, the install path, then the stored bytes:
// cmpsize() bytes of compress(1) output when cmpsize() is non-zero, else
// size() bytes verbatim. An entry's off() points at its record header.
//
// Two compression layers can sit on an entry's bytes. Dist.Open removes the
// archive's, because the manifest says it is there. A file may also be
// installed compressed, as name.z (pack) or name.Z (compress); that layer
// belongs to the installed file, so Unwrap removes it only on request.
package inst

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"sort"
	"strings"

	"github.com/jamesbraid/instigator/unixcompress"
)

const (
	sigLen    = 13 // also the offset of the first record
	sigPrefix = "im001"
)

// Product is one product of a distribution directory.
type Product struct {
	Name    string    // the manifest's base name, such as "eoe"
	Entries []Entry   // manifest entries in file order
	Bad     []BadLine // manifest lines that did not parse
}

// Dist is an open distribution directory.
type Dist struct {
	Products []Product // sorted by name

	fsys     fs.FS
	archives map[string]*archive // by file name; failures are cached too
}

type archive struct {
	f    fs.File
	r    io.ReaderAt
	size int64
	err  error
}

// OpenDist finds the products in the root of fsys and reads their
// manifests. A .idb with no sibling archive is not a product and is
// skipped. Archives are opened later, by Open, and their files must
// implement io.ReaderAt so records are read by section, never whole.
//
// Some products list no off() at all. Their records still lie back to back
// in .idb order, so walking each image from its first record and adding
// 2+len(path)+Stored() for every file entry of that image finds them; an
// explicit off() restarts the walk. Open checks the record name, so a wrong
// walk is an error, never wrong bytes.
func OpenDist(fsys fs.FS) (*Dist, error) {
	names, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("inst: reading distribution directory: %w", err)
	}
	// A product P has an archive P.<image> beside P.idb. Image names have
	// no dot, which keeps product "eoe" apart from product "eoe.x".
	hasImage := map[string]bool{}
	for _, de := range names {
		i := strings.LastIndexByte(de.Name(), '.')
		if de.IsDir() || i <= 0 {
			continue
		}
		if img := de.Name()[i+1:]; img != "" && img != "idb" {
			hasImage[de.Name()[:i]] = true
		}
	}
	d := &Dist{fsys: fsys, archives: map[string]*archive{}}
	for _, de := range names {
		name := de.Name()
		prod, ok := strings.CutSuffix(name, ".idb")
		if de.IsDir() || !ok || !hasImage[prod] {
			continue
		}
		f, err := fsys.Open(name)
		if err != nil {
			return nil, fmt.Errorf("inst: %w", err)
		}
		entries, bad, err := ParseIDB(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("inst: %s: %w", name, err)
		}
		walkOffsets(entries)
		d.Products = append(d.Products, Product{Name: prod, Entries: entries, Bad: bad})
	}
	sort.Slice(d.Products, func(i, j int) bool { return d.Products[i].Name < d.Products[j].Name })
	return d, nil
}

// walkOffsets fills in the Offset of file entries listed without off().
func walkOffsets(entries []Entry) {
	next := map[string]int64{} // per image: where the next record starts
	for i := range entries {
		e := &entries[i]
		if e.Type != 'f' {
			continue
		}
		at, ok := next[e.Image()]
		if !ok {
			at = sigLen
		}
		if e.Offset < 0 {
			e.Offset = at
		}
		next[e.Image()] = -1
		if n := int64(2 + len(e.Path)); e.Offset >= 0 && e.Stored() <= math.MaxInt64-e.Offset-n {
			next[e.Image()] = e.Offset + n + e.Stored()
		}
	}
}

// Open returns the installed bytes of e, an entry of the named product,
// with the archive's own compress(1) layer removed. A missing or unreadable
// archive is an error for this entry only. Installed .z/.Z compression is
// left in place; see Unwrap.
func (d *Dist) Open(product string, e Entry) (io.ReadCloser, error) {
	a := d.archive(product + "." + e.Image())
	if a.err != nil {
		return nil, a.err
	}
	rec, err := a.record(e)
	if err != nil {
		return nil, fmt.Errorf("inst: %s: %w", e.Path, err)
	}
	if e.CmpSize == 0 {
		return io.NopCloser(rec), nil
	}
	z, err := unixcompress.NewReader(rec)
	if err != nil {
		return nil, fmt.Errorf("inst: %s: %w", e.Path, err)
	}
	return z, nil
}

// Close closes the archive files Open has used.
func (d *Dist) Close() error {
	var errs []error
	for _, a := range d.archives {
		if a.f != nil {
			errs = append(errs, a.f.Close())
		}
	}
	d.archives = map[string]*archive{}
	return errors.Join(errs...)
}

func (d *Dist) archive(name string) *archive {
	if a, ok := d.archives[name]; ok {
		return a
	}
	a := &archive{}
	d.archives[name] = a
	f, err := d.fsys.Open(name)
	if err != nil {
		a.err = fmt.Errorf("inst: archive %s: %w", name, err)
		return a
	}
	a.f = f
	st, err := f.Stat()
	ra, ok := f.(io.ReaderAt)
	var sig [sigLen]byte
	switch {
	case err != nil:
	case !ok:
		err = errors.New("the file system's files do not implement io.ReaderAt")
	case st.Size() < sigLen:
		err = fmt.Errorf("image is %d bytes, shorter than the %d-byte signature", st.Size(), sigLen)
	default:
		if _, err = ra.ReadAt(sig[:], 0); err == nil && (string(sig[:len(sigPrefix)]) != sigPrefix || sig[sigLen-1] != 0) {
			err = fmt.Errorf("bad signature % x", sig[:])
		}
	}
	if err != nil {
		a.err = fmt.Errorf("inst: archive %s: %w", name, err)
		return a
	}
	a.r, a.size = ra, st.Size()
	return a
}

// record returns a reader over exactly the stored bytes of e's record,
// after checking that the record is named e.Path. Bounds are compared by
// subtraction so a huge off() or size() cannot wrap around.
func (a *archive) record(e Entry) (io.Reader, error) {
	off := e.Offset
	if off < sigLen {
		return nil, fmt.Errorf("no usable record offset (%d)", off)
	}
	if off > a.size-2 {
		return nil, fmt.Errorf("record offset %d is past the end of the %d-byte archive", off, a.size)
	}
	var hdr [2]byte
	if _, err := a.r.ReadAt(hdr[:], off); err != nil {
		return nil, fmt.Errorf("reading record header at %d: %w", off, err)
	}
	nlen := int64(hdr[0])<<8 | int64(hdr[1])
	if nlen > a.size-off-2 {
		return nil, fmt.Errorf("record name at %d runs past the end of the archive", off)
	}
	name := make([]byte, nlen)
	if _, err := a.r.ReadAt(name, off+2); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("reading record name at %d: %w", off, err)
	}
	if string(name) != e.Path {
		return nil, fmt.Errorf("record at %d is named %q, not %q", off, name, e.Path)
	}
	start := off + 2 + nlen
	if n := e.Stored(); n < 0 || n > a.size-start {
		return nil, fmt.Errorf("%d stored bytes at %d run past the end of the %d-byte archive", n, start, a.size)
	}
	return io.NewSectionReader(a.r, start, e.Stored()), nil
}
