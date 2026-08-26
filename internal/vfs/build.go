package vfs

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jamesbraid/instigator/efs"
)

// maxSymlinks bounds symlink following inside one source, so a link loop
// on the media ends the walk instead of the process.
const maxSymlinks = 8

// compareBlock is how much of two colliding files is compared at a time.
const compareBlock = 64 << 10

// distDir is the one directory every layer contributes to and inst reads
// from, whatever the source media call theirs. standDir is the boot
// layer's, served so the PROM can fetch fx.64 before anything else exists.
const (
	distDir  = "dist"
	standDir = "stand"
)

// Build assembles the configured install sets into one read-only tree.
// Each set becomes a directory holding the ordered merge of its layers'
// distribution directories at <set>/dist, plus <set>/stand from the set's
// boot layer if it has one; no disc name appears anywhere in the result.
// Every source is opened once and stays open for the tree's life, so
// Close it when done.
//
// Build fails rather than guess: a layer whose source or distribution
// directory is missing, a boot layer whose source has no stand directory,
// a file present with different bytes in two layers and no
// configured winner for that exact path, a configured winner no layer
// delivers, or a symlink that does not resolve, contained, to a regular
// file all stop the build. Content is read lazily on Open; only files two
// layers both claim are read here, to compare them.
func Build(sets []SetSpec) (*Tree, error) {
	t := &Tree{root: newDir(".", Origin{})}
	sources := map[string]source{}

	for _, set := range sets {
		if set.Name == "" {
			t.Close()
			return nil, fmt.Errorf("install set with no name")
		}
		if _, dup := t.root.children[set.Name]; dup {
			t.Close()
			return nil, fmt.Errorf("install set %q defined twice", set.Name)
		}
		t.root.children[set.Name] = newDir(set.Name, Origin{})
		for _, layer := range set.Layers {
			if err := t.addLayer(set, layer, sources); err != nil {
				t.Close()
				return nil, fmt.Errorf("install set %q: layer %q: %w", set.Name, layer.Name, err)
			}
		}
		if err := t.checkCollisions(set); err != nil {
			t.Close()
			return nil, fmt.Errorf("install set %q: %w", set.Name, err)
		}
	}
	t.number()
	return t, nil
}

// addLayer merges one layer's distribution directory into the set's dist,
// and for the set's boot layer serves its stand directory alongside.
func (t *Tree) addLayer(set SetSpec, layer LayerSpec, sources map[string]source) error {
	if layer.Name == "" {
		return fmt.Errorf("layer with no name")
	}
	dist, err := cleanDir(layer.Dist)
	if err != nil {
		return fmt.Errorf("distribution directory: %w", err)
	}

	switch {
	case layer.Image != "" && layer.Dir != "":
		return fmt.Errorf("names both an image and a directory")
	case layer.Image != "":
		src, err := t.openImageSource(layer.Image, sources)
		if err != nil {
			return err
		}
		return t.addSourceLayer(set, layer, src, dist)
	case layer.Dir != "":
		src, err := t.openDirSource(layer.Dir, sources)
		if err != nil {
			return err
		}
		return t.addSourceLayer(set, layer, src, dist)
	default:
		return fmt.Errorf("names neither an image nor a directory")
	}
}

// source is one opened layer filesystem: the fs.FS the walker reads and
// the closer that releases it when the tree closes.
type source struct {
	fsys   fs.FS
	closer io.Closer
}

// openImageSource opens an EFS disc image once and shares it. The Disc
// stays the closer; its fs view is what the walker reads.
func (t *Tree) openImageSource(image string, cache map[string]source) (source, error) {
	key := filepath.Clean(image)
	if s, ok := cache[key]; ok {
		return s, nil
	}
	d, err := OpenImage(key)
	if err != nil {
		return source{}, err
	}
	s := source{fsys: d.FS().FSys(), closer: d}
	cache[key] = s
	t.closers = append(t.closers, d)
	return s, nil
}

// openDirSource opens a directory layer under os.OpenRoot, whose fs view
// keeps a symlink inside it from serving a host path.
func (t *Tree) openDirSource(dir string, cache map[string]source) (source, error) {
	key := filepath.Clean(dir)
	if s, ok := cache[key]; ok {
		return s, nil
	}
	r, err := os.OpenRoot(key)
	if err != nil {
		return source{}, err
	}
	s := source{fsys: r.FS(), closer: r}
	cache[key] = s
	t.closers = append(t.closers, r)
	return s, nil
}

// addSourceLayer merges a layer's distribution directory into the set's
// dist, and for the set's boot layer serves its stand directory too. The
// origin kind records which sort of source it was, for provenance.
func (t *Tree) addSourceLayer(set SetSpec, layer LayerSpec, src source, dist string) error {
	kind := OriginImage
	if layer.Dir != "" {
		kind = OriginDirectory
	}
	if err := t.mergeTree(set, layer, src, kind, dist, path.Join(set.Name, distDir)); err != nil {
		return err
	}
	if !layer.Boot {
		return nil
	}
	if _, err := fs.Stat(src.fsys, standDir); err != nil {
		return fmt.Errorf("boot layer has no %s directory: %w", standDir, err)
	}
	return t.mergeTree(set, layer, src, kind, standDir, path.Join(set.Name, standDir))
}

// cleanDir normalizes a configured Dist to an io/fs name; empty means the
// ordinary "dist", which is what all but the version-stub media use.
func cleanDir(dir string) (string, error) {
	if dir == "" {
		return distDir, nil
	}
	clean := path.Clean(dir)
	if !fs.ValidPath(clean) {
		return "", fmt.Errorf("%q is not a relative path within its source", dir)
	}
	return clean, nil
}

// mergeTree walks one subtree of a source onto a tree path. srcDir is the
// directory within the source (dist or stand); target is where it lands.
func (t *Tree) mergeTree(set SetSpec, layer LayerSpec, src source, kind OriginKind, srcDir, target string) error {
	info, srcPath, err := statFollow(src.fsys, srcDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", srcDir)
	}
	dir, err := t.ensureDir(target, originOf(kind, layer, srcPath))
	if err != nil {
		return err
	}
	return t.walkFS(set, layer, src, kind, srcPath, target, dir)
}

func (t *Tree) walkFS(set SetSpec, layer LayerSpec, src source, kind OriginKind, srcPath, target string, dir *node) error {
	ents, err := fs.ReadDir(src.fsys, srcPath)
	if err != nil {
		return fmt.Errorf("%s: %w", srcPath, err)
	}
	for _, e := range ents {
		childSrc := path.Join(srcPath, e.Name())
		childTarget := path.Join(target, e.Name())
		info, err := e.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			// A link resolves within its own source - an EFS Lookup never
			// leaves the image's inode graph, and an os.Root refuses a link
			// that escapes the layer - so one aimed outside simply finds
			// nothing. Serving the set without it would be a quiet lie
			// about what the media holds, so resolveLink names the link.
			info, childSrc, err = resolveLink(src.fsys, childSrc)
			if err != nil {
				return err
			}
			// A link to a directory would duplicate a subtree and could
			// cycle; the tree materializes files, not link graphs.
			if info.IsDir() {
				return fmt.Errorf("symlink %s resolves to a directory; directory-link traversal is unsupported", path.Join(srcPath, e.Name()))
			}
			// Nothing else is materializable. A device node or a pipe
			// reached directly is skipped below, but a link is followed
			// only to a regular file, so one aimed at a special file is
			// as much an incomplete set as an unresolvable one.
			if !info.Mode().IsRegular() {
				return fmt.Errorf("symlink %s resolves to a special file (%s); only a link to a regular file is served", path.Join(srcPath, e.Name()), info.Mode())
			}
		}
		switch {
		case info.IsDir():
			sub, err := mergeChild(dir, newDir(e.Name(), originOf(kind, layer, childSrc)), childTarget, nil)
			if err != nil {
				return err
			}
			if err := t.walkFS(set, layer, src, kind, childSrc, childTarget, sub); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			uid, gid, nlink := ownerOf(info)
			f := &node{
				name:   e.Name(),
				origin: originOf(kind, layer, childSrc),
				perm:   info.Mode().Perm(),
				size:   info.Size(),
				mtime:  info.ModTime(),
				uid:    uid,
				gid:    gid,
				nlink:  nlink,
				fsys:   src.fsys,
			}
			if _, err := mergeChild(dir, f, childTarget, set.Collisions); err != nil {
				return err
			}
		}
	}
	return nil
}

// resolveLink follows a symlink within its own source, up to maxSymlinks
// hops, and returns the resolved file's info and path. A relative target
// resolves against the link's directory and an absolute one against the
// source root, so no target can name anything outside the source. A target
// that does not resolve is an error, because serving the set without it
// would quietly misreport what the media holds. Errors name the link the
// walk started from, not the intermediate path that following mutated it to.
func resolveLink(fsys fs.FS, name string) (fs.FileInfo, string, error) {
	orig := name
	for hops := 0; ; hops++ {
		if hops > maxSymlinks {
			return nil, "", fmt.Errorf("symlink %s: too many levels of symbolic links", orig)
		}
		info, err := fs.Stat(fsys, name)
		if err != nil {
			return nil, "", fmt.Errorf("symlink %s does not resolve within the source: %w", orig, err)
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			return info, name, nil
		}
		target, err := fs.ReadLink(fsys, name)
		if err != nil {
			return nil, "", fmt.Errorf("symlink %s: %w", orig, err)
		}
		if path.IsAbs(target) {
			name = path.Clean(strings.TrimPrefix(target, "/"))
			if name == "" || name == "." {
				name = "."
			}
		} else {
			name = path.Join(path.Dir(name), target)
		}
	}
}

// statFollow stats a path, following it if it is itself a symlink, so a
// dist or stand directory reached through a link is walked as its target.
func statFollow(fsys fs.FS, name string) (fs.FileInfo, string, error) {
	info, err := fs.Stat(fsys, name)
	if err != nil {
		return nil, "", err
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		return info, name, nil
	}
	return resolveLink(fsys, name)
}

// ownerOf reads the owner an EFS source reports through Sys(); a directory
// layer reports none, so its files stay unowned with a single link.
func ownerOf(info fs.FileInfo) (uid, gid uint32, nlink int) {
	if st, ok := info.Sys().(*efs.Stat); ok {
		return uint32(st.UID), uint32(st.GID), int(st.Nlink)
	}
	return 0, 0, 1
}

// originOf builds the provenance for a walked path: the kind of source,
// the layer name, and the source-relative path the bytes live at.
func originOf(kind OriginKind, layer LayerSpec, srcPath string) Origin {
	return Origin{Kind: kind, Source: layer.Name, Path: srcPath}
}

// ensureDir returns the directory node at a logical path, creating it and
// any missing parent with the given origin.
func (t *Tree) ensureDir(logical string, origin Origin) (*node, error) {
	n := t.root
	if logical == "." {
		return n, nil
	}
	seen := ""
	for _, part := range strings.Split(logical, "/") {
		seen = path.Join(seen, part)
		child, err := mergeChild(n, newDir(part, origin), seen, nil)
		if err != nil {
			return nil, err
		}
		n = child
	}
	return n, nil
}

// mergeChild inserts one candidate under parent and returns the node that
// won. Directories union and keep the earliest layer's origin. Regular
// files must agree byte for byte - the earliest layer then owning the
// origin - unless collisions names the layer whose copy wins at this exact
// logical path. A file landing where a directory is, or the reverse, is
// structural and no winner can settle it.
func mergeChild(parent, cand *node, logical string, collisions map[string]string) (*node, error) {
	old, ok := parent.children[cand.name]
	if !ok {
		parent.children[cand.name] = cand
		return cand, nil
	}
	if old.dir != cand.dir {
		return nil, fmt.Errorf("%s: %q has a %s where %q has a %s",
			logical, old.origin.Source, kindOf(old), cand.origin.Source, kindOf(cand))
	}
	if old.dir {
		return old, nil
	}
	if winner := collisions[logical]; winner != "" {
		if cand.origin.Source == winner {
			parent.children[cand.name] = cand
			return cand, nil
		}
		return old, nil
	}
	same, err := sameContent(old, cand)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", logical, err)
	}
	if !same {
		return nil, fmt.Errorf("%s: layers %q and %q disagree; name the winning layer in the set's collisions",
			logical, old.origin.Source, cand.origin.Source)
	}
	return old, nil
}

func kindOf(n *node) string {
	if n.dir {
		return "directory"
	}
	return "file"
}

// sameContent compares two colliding files, short-circuiting on size and
// otherwise reading a block at a time. This is the only content read the
// build ever does.
func sameContent(a, b *node) (bool, error) {
	if a.size != b.size {
		return false, nil
	}
	af, err := a.openRegular()
	if err != nil {
		return false, err
	}
	defer af.Close()
	bf, err := b.openRegular()
	if err != nil {
		return false, err
	}
	defer bf.Close()

	ap := make([]byte, compareBlock)
	bp := make([]byte, compareBlock)
	for off := int64(0); off < a.size; {
		n := int64(compareBlock)
		if rest := a.size - off; n > rest {
			n = rest
		}
		if err := readFullAt(af, ap[:n], off); err != nil {
			return false, err
		}
		if err := readFullAt(bf, bp[:n], off); err != nil {
			return false, err
		}
		if !bytes.Equal(ap[:n], bp[:n]) {
			return false, nil
		}
		off += n
	}
	return true, nil
}

// readFullAt fills p from off, treating the EOF a source reports on its
// last full block as success.
func readFullAt(f File, p []byte, off int64) error {
	for len(p) > 0 {
		n, err := f.ReadAt(p, off)
		p = p[n:]
		off += int64(n)
		if err != nil {
			if err == io.EOF && len(p) == 0 {
				return nil
			}
			return err
		}
	}
	return nil
}

// checkCollisions verifies every configured winner actually won. A winner
// suppresses the byte comparison for its path, so one that names a layer
// contributing nothing there would leave un-compared bytes serving under
// an override that did nothing.
func (t *Tree) checkCollisions(set SetSpec) error {
	paths := make([]string, 0, len(set.Collisions))
	for p := range set.Collisions {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		winner := set.Collisions[p]
		n, ok := t.lookup(p)
		if !ok || n.dir {
			return fmt.Errorf("collision winner %q: no file at %s", winner, p)
		}
		if n.origin.Source != winner {
			return fmt.Errorf("collision winner %q: %s comes from layer %q instead", winner, p, n.origin.Source)
		}
	}
	return nil
}
