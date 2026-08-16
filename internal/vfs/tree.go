package vfs

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jamesbraid/instigator/efs"
)

// ErrNotFound is returned for paths that do not resolve.
var ErrNotFound = fmt.Errorf("not found")

// MediaSet names a directory of CD images served under one media name.
type MediaSet struct {
	Name string
	Dir  string

	// DiscNames overrides the slugged serve name per image filename.
	DiscNames map[string]string
}

// Tree is the assembled serve tree: /<media>/<disc>/<efs path>. All
// protocol servers read through it.
type Tree struct {
	medias    map[string]map[string]*Disc  // media -> disc slug -> disc
	files     map[string]map[string]string // media -> disc slug -> image filename
	unions    map[string]*union            // union name -> merged dist layers
	synthetic map[string][]byte            // top-level generated files (the runbook)
}

// File is an open random-access file from the tree.
type File interface {
	io.ReaderAt
	Size() int64
}

// slug turns an image filename into a serve name: lowercase, extension
// dropped, runs of non-alphanumerics collapsed to one dash.
func slug(filename string) string {
	base := strings.TrimSuffix(filename, filepath.Ext(filename))
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
	}
	return b.String()
}

// BuildTree opens every image in every media set. Image files are
// recognized by extension (.iso, .img, .image) or by parsing; files
// that fail to parse as SGI media are skipped with an error only when
// nothing at all could be served.
func BuildTree(sets []MediaSet) (*Tree, error) {
	t := &Tree{
		medias: map[string]map[string]*Disc{},
		files:  map[string]map[string]string{},
	}
	for _, set := range sets {
		if _, dup := t.medias[set.Name]; dup {
			return nil, fmt.Errorf("media %q defined twice", set.Name)
		}
		discs := map[string]*Disc{}
		names := map[string]string{}
		entries, err := os.ReadDir(set.Dir)
		if err != nil {
			t.Close()
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			d, err := OpenImage(filepath.Join(set.Dir, e.Name()))
			if err != nil {
				// not SGI media; leave a trace and move on
				continue
			}
			name := set.DiscNames[e.Name()]
			if name == "" {
				name = slug(e.Name())
			}
			if _, dup := discs[name]; dup {
				d.Close()
				t.Close()
				return nil, fmt.Errorf("media %q: disc name %q maps to two images", set.Name, name)
			}
			discs[name] = d
			names[name] = e.Name()
		}
		if len(discs) == 0 {
			t.Close()
			return nil, fmt.Errorf("media %q: no SGI CD images found in %s", set.Name, set.Dir)
		}
		t.medias[set.Name] = discs
		t.files[set.Name] = names
	}
	return t, nil
}

// Close releases every opened image.
func (t *Tree) Close() error {
	for _, discs := range t.medias {
		for _, d := range discs {
			d.Close()
		}
	}
	for _, u := range t.unions {
		u.close()
	}
	return nil
}

// DiscMap reports media -> disc slug -> image filename, for startup
// logging and PROM hints.
func (t *Tree) DiscMap() map[string]map[string]string { return t.files }

// resolve splits a tree path into the disc and the path within it.
func (t *Tree) resolve(path string) (*Disc, string, error) {
	parts := strings.SplitN(strings.Trim(path, "/"), "/", 3)
	if len(parts) < 2 {
		return nil, "", fmt.Errorf("%s: %w", path, ErrNotFound)
	}
	discs, ok := t.medias[parts[0]]
	if !ok {
		return nil, "", fmt.Errorf("media %q: %w", parts[0], ErrNotFound)
	}
	d, ok := discs[parts[1]]
	if !ok {
		return nil, "", fmt.Errorf("disc %q: %w", parts[1], ErrNotFound)
	}
	rest := "/"
	if len(parts) == 3 {
		rest = parts[2]
	}
	return d, rest, nil
}

// Open opens a file by tree path, following symlinks within the disc.
func (t *Tree) Open(path string) (File, error) {
	if c, ok := t.synthetic[strings.Trim(path, "/")]; ok {
		return &bytesFile{r: bytes.NewReader(c), size: int64(len(c))}, nil
	}
	parts := strings.SplitN(strings.Trim(path, "/"), "/", 2)
	if u, ok := t.unions[parts[0]]; ok {
		rest := ""
		if len(parts) == 2 {
			rest = parts[1]
		}
		f, err := u.open(rest)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return f, nil
	}
	d, rest, err := t.resolve(path)
	if err != nil {
		return nil, err
	}
	node, err := d.lookupFollow(rest, 8)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, ErrNotFound)
	}
	if !node.IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	return &efsFile{fs: d.FS(), node: node}, nil
}

// ReadDir lists names at any level: media sets at the root, discs
// within a media, EFS directories below.
func (t *Tree) ReadDir(path string) ([]string, error) {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		names := sortedKeys(t.medias)
		names = append(names, sortedKeys(t.unions)...)
		for n := range t.synthetic {
			names = append(names, n)
		}
		sort.Strings(names)
		return names, nil
	}
	if u, ok := t.unions[strings.SplitN(trimmed, "/", 2)[0]]; ok {
		rest := ""
		if i := strings.IndexByte(trimmed, '/'); i >= 0 {
			rest = trimmed[i+1:]
		}
		return u.readdir(rest)
	}
	parts := strings.SplitN(trimmed, "/", 3)
	discs, ok := t.medias[parts[0]]
	if !ok {
		return nil, fmt.Errorf("media %q: %w", parts[0], ErrNotFound)
	}
	if len(parts) == 1 {
		return sortedKeys(discs), nil
	}
	d, ok := discs[parts[1]]
	if !ok {
		return nil, fmt.Errorf("disc %q: %w", parts[1], ErrNotFound)
	}
	rest := "/"
	if len(parts) == 3 {
		rest = parts[2]
	}
	node, err := d.lookupFollow(rest, 8)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, ErrNotFound)
	}
	ents, err := d.FS().ReadDir(node)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name)
	}
	sort.Strings(names)
	return names, nil
}

// lookupFollow resolves an EFS path, following symlinks up to depth.
func (d *Disc) lookupFollow(path string, depth int) (*efs.Inode, error) {
	node, err := d.FS().Lookup(path)
	if err != nil {
		return nil, err
	}
	for node.IsSymlink() {
		if depth == 0 {
			return nil, fmt.Errorf("%s: too many symlinks", path)
		}
		depth--
		target, err := d.FS().Readlink(node)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(target, "/") {
			target = filepath.Join(filepath.Dir("/"+strings.Trim(path, "/")), target)
		}
		path = target
		if node, err = d.FS().Lookup(path); err != nil {
			return nil, err
		}
	}
	return node, nil
}

func sortedKeys[M ~map[string]V, V any](m M) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// efsFile adapts an EFS inode to the File interface.
type efsFile struct {
	fs   *efs.FS
	node *efs.Inode
}

func (f *efsFile) ReadAt(p []byte, off int64) (int, error) { return f.fs.ReadAt(f.node, p, off) }
func (f *efsFile) Size() int64                             { return int64(f.node.Size) }
