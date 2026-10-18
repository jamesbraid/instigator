package extract

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jamesbraid/instigator/inst"
)

// writer carries out the writes plan left pending.
type writer struct {
	root     *os.Root
	dists    []dist
	unpack   bool
	dirs     map[string]bool   // directories this run wrote, by path relative to the set root
	unpacked map[string]string // for Unpack: the suffix a written file lost, by install path
	links    map[string]string // for Unpack: where each link points, by install path
}

// held is what the set holds at a path.
type held struct {
	typ     byte
	content string // sha256 of a file, target of a link
}

// writeAll writes the pending entries, in an order that lets each land
// where it belongs, and settles their records. Entries already settled
// never reach it, so they cannot change the order.
//
// Links that other entries install beneath go first. Products are written
// in name order, so one product's files can come before another's link to
// the directory that holds them; written first, they would make a
// directory and leave no room for the link. On a running system the base
// product's links are already in place and later installs land through
// them, so these entries do too. Directories and files follow, then the
// remaining links: written last, a link can follow its target's rename by
// Unpack. Where a file and a link want the same path, the file therefore
// takes it and the link is the variant.
//
// With Unpack, a link whose target loses its suffix follows it: the suffix
// comes off the target, and off the link's own name when that ends in the
// same suffix. This applies through chains of links.
func (w *writer) writeAll(pending []*item) {
	parents := map[string]bool{}
	for _, it := range pending {
		for d := path.Dir(it.rel); d != "." && !parents[d]; d = path.Dir(d) {
			parents[d] = true
		}
		if t, ok := linkDest(it.rel, it.e.Symlink); ok && it.e.Type == 'l' {
			if _, dup := w.links[it.rel]; !dup {
				w.links[it.rel] = t
			}
		}
	}
	var first, rest, last []*item
	for _, it := range pending {
		switch {
		case it.e.Type == 'l' && parents[it.rel]:
			first = append(first, it)
		case it.e.Type == 'l':
			last = append(last, it)
		default:
			rest = append(rest, it)
		}
	}
	for _, it := range slices.Concat(first, rest, last) {
		w.write(it)
	}
}

func (w *writer) write(it *item) {
	rec := &it.rec
	rec.Path = it.rel
	switch it.e.Type {
	case 'd':
		w.dir(it)
	case 'l':
		w.symlink(it)
	case 'f':
		w.file(it)
	}
	if rec.Status == skipped || rec.Status == failed {
		rec.Path = "" // nothing was written
	}
}

// unpackOf reports the suffix the entry at rel loses, following links: a
// link loses one when its destination does and its name ends in it.
func (w *writer) unpackOf(rel string, depth int) string {
	if u, ok := w.unpacked[rel]; ok {
		return u
	}
	if t, ok := w.links[rel]; ok && depth > 0 {
		if u := w.unpackOf(t, depth-1); u != "" && strings.HasSuffix(rel, u) {
			return u
		}
	}
	return ""
}

// linkDest is the install path, relative to the set root, that a link at
// rel with target points at, resolved as a running system would: an
// absolute target cannot climb above the root. It reports false as
// cleanInstallPath does, for the root itself and for a target that leaves
// it.
func linkDest(rel, target string) (string, bool) {
	if target == "" {
		return "", false
	}
	if strings.HasPrefix(target, "/") {
		return cleanInstallPath(path.Clean(target))
	}
	return cleanInstallPath(path.Join(path.Dir(rel), target))
}

func (w *writer) dir(it *item) {
	rec := &it.rec
	if w.dirs[rec.Path] {
		rec.Status = duplicate
		return
	}
	if h, ok := w.lookup(rec.Path); ok && h.typ != 'd' {
		it.fail(fmt.Errorf("%s was already installed as type %c", rec.Path, h.typ))
		return
	}
	if err := w.root.MkdirAll(rec.Path, 0o755); err != nil {
		it.fail(err)
		return
	}
	// Keep the owner able to write so later entries can land inside.
	if err := w.root.Chmod(rec.Path, fs.FileMode(it.e.Mode&0o777)|0o700); err != nil {
		it.fail(err)
		return
	}
	w.dirs[rec.Path] = true
	rec.Status = written
}

func (w *writer) symlink(it *item) {
	rec, target := &it.rec, it.e.Symlink
	if target == "" {
		it.fail(errors.New("link entry has no symval"))
		return
	}
	dest, ok := linkDest(rec.Path, target)
	if !ok && dest != "." {
		it.settle(skipped, "link target escapes the set root; recorded only")
		return
	}
	rel := target
	if strings.HasPrefix(target, "/") {
		r, err := filepath.Rel(filepath.FromSlash(path.Dir(rec.Path)), filepath.FromSlash(dest))
		if err != nil {
			it.fail(err)
			return
		}
		rel = filepath.ToSlash(r)
	}
	// Follow a target renamed by unpacking, as writeAll describes.
	if ok && w.unpack {
		if u := w.unpackOf(dest, 40); u != "" && strings.HasSuffix(rel, u) {
			rel = strings.TrimSuffix(rel, u)
			rec.Path = strings.TrimSuffix(rec.Path, u)
		}
	}
	rec.Target = rel
	at, status, err := w.claim(rec.Path, held{typ: 'l', content: rel}, rec.Mach)
	if err != nil {
		it.fail(err)
		return
	}
	rec.Path, rec.Status = at, status
	if status == duplicate {
		return
	}
	if err := w.root.MkdirAll(path.Dir(at), 0o755); err != nil {
		it.fail(err)
		return
	}
	if err := w.root.Symlink(rel, at); err != nil {
		it.fail(err)
	}
}

// file writes a file entry. A free path takes the bytes directly. When
// the path is taken, where the file goes depends on its digest, so the
// data is read once to hash it and again only to write a variant.
func (w *writer) file(it *item) {
	rec := &it.rec
	name, in, err := w.open(it)
	if err != nil {
		it.fail(err)
		return
	}
	defer in.Close()
	mode := fs.FileMode(it.e.Mode&0o777) | 0o600
	if err := w.root.MkdirAll(path.Dir(name), 0o755); err != nil {
		it.fail(err)
		return
	}
	at, status := name, written
	sum, err := w.copyTo(name, in, mode)
	if errors.Is(err, fs.ErrExist) {
		h := sha256.New()
		if _, err = io.Copy(h, in); err == nil {
			sum = hex.EncodeToString(h.Sum(nil))
			at, status, err = w.claim(name, held{typ: 'f', content: sum}, rec.Mach)
		}
		if err == nil && status == variant {
			err = w.writeVariant(it, at, mode)
		}
	}
	if err != nil {
		it.fail(err)
		return
	}
	rec.Path, rec.Status, rec.SHA256 = at, status, sum
	if status == duplicate {
		return
	}
	if status == written && name != it.rel {
		w.unpacked[it.rel] = it.rel[len(name):]
	}
}

// writeVariant writes the entry's data again, to the variant name at.
func (w *writer) writeVariant(it *item, at string, mode fs.FileMode) error {
	_, in, err := w.open(it)
	if err != nil {
		return err
	}
	defer in.Close()
	_, err = w.copyTo(at, in, mode)
	return err
}

// open returns the installed bytes of a file entry and the path they go
// to, which with Unpack loses a removed .z or .Z suffix.
func (w *writer) open(it *item) (string, io.ReadCloser, error) {
	rc, err := w.dists[it.set].Open(it.rec.Product, *it.e)
	if err != nil || !w.unpack {
		return it.rel, rc, err
	}
	name, r, err := inst.Unwrap(it.rel, rc)
	if err != nil {
		rc.Close()
		return "", nil, err
	}
	return name, struct {
		io.Reader
		io.Closer
	}{r, rc}, nil
}

// claim decides where a file or link goes when it wants path rel. A free
// path is used as is. A path holding the same type with the same content
// (the same bytes, or the same link target) makes the entry a duplicate.
// Anything else, including a file meeting a link, is a variant written
// beside it as <rel>.<mach>, or <rel>.<first 8 hex digits of the digest>
// without a mach tag or when <rel>.<mach> would be longer than the 255
// bytes most file systems allow a name; a link's digest is that of its
// target string. A directory in the way is an error.
func (w *writer) claim(rel string, want held, mach string) (string, string, error) {
	have, taken := w.lookup(rel)
	if !taken {
		return rel, written, nil
	}
	if have == want {
		return rel, duplicate, nil
	}
	if have.typ == 'd' {
		return "", "", fmt.Errorf("%s is already installed as a directory", rel)
	}
	digest := want.content
	if want.typ == 'l' {
		sum := sha256.Sum256([]byte(want.content))
		digest = hex.EncodeToString(sum[:])
	}
	var cands []string
	if m := sanitize(mach); m != "" && len(path.Base(rel))+1+len(m) <= 255 {
		cands = append(cands, rel+"."+m)
	}
	cands = append(cands, rel+"."+digest[:8])
	for _, c := range cands {
		s, taken := w.lookup(c)
		if taken && s == want {
			return c, duplicate, nil
		}
		if !taken {
			return c, variant, nil
		}
	}
	return "", "", fmt.Errorf("no free variant name for %s", rel)
}

// lookup reports what the set already holds at rel, read from the tree
// itself: a path that runs through a link installed earlier can land on a
// file written under another name.
func (w *writer) lookup(rel string) (held, bool) {
	fi, err := w.root.Lstat(rel)
	if err != nil {
		return held{}, false
	}
	switch {
	case fi.IsDir():
		return held{typ: 'd'}, true
	case fi.Mode()&fs.ModeSymlink != 0:
		t, err := w.root.Readlink(rel)
		if err != nil {
			return held{typ: 'l'}, true
		}
		return held{typ: 'l', content: t}, true
	}
	f, err := w.root.Open(rel)
	if err != nil {
		return held{typ: 'f'}, true
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return held{typ: 'f'}, true
	}
	return held{typ: 'f', content: hex.EncodeToString(h.Sum(nil))}, true
}

// copyTo streams in to a new file at name and returns its sha256, removing
// the file on any failure. It fails with fs.ErrExist, having read
// nothing, when name is taken.
func (w *writer) copyTo(name string, in io.Reader, mode fs.FileMode) (string, error) {
	f, err := w.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), in)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = w.root.Chmod(name, mode)
	}
	if err != nil {
		w.root.Remove(name)
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// sanitize keeps a mach tag safe as a file name suffix.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_', r == '=':
			return r
		}
		return '_'
	}, s)
}
