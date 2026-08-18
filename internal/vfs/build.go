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
	"time"

	"github.com/jamesbraid/instigator/efs"
)

// maxSymlinks bounds symlink following inside one source, so a link loop
// on the media ends the walk instead of the process.
const maxSymlinks = 8

// compareBlock is how much of two colliding files is compared at a time.
const compareBlock = 64 << 10

// Build assembles the configured install sets into one read-only tree.
// Each set becomes a directory holding the ordered merge of its layers;
// no disc name appears anywhere in the result. Every source is opened
// once and stays open for the tree's life, so Close it when done.
//
// Build fails rather than guess: a layer whose source or SourceDir is
// missing, a file present with different bytes in two layers and no
// configured winner for that exact path, a configured winner no layer
// delivers, or a symlink that does not resolve, contained, to a regular
// file all stop the build. Content is read lazily on Open; only files two
// layers both claim are read here, to compare them.
func Build(sets []SetSpec) (*Tree, error) {
	t := &Tree{root: newDir(".", Origin{})}
	images := map[string]*Disc{}
	roots := map[string]*os.Root{}

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
			if err := t.addLayer(set, layer, images, roots); err != nil {
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

// addLayer maps one layer's SourceDir onto its TargetDir within the set.
func (t *Tree) addLayer(set SetSpec, layer LayerSpec, images map[string]*Disc, roots map[string]*os.Root) error {
	if layer.Name == "" {
		return fmt.Errorf("layer with no name")
	}
	srcDir, err := cleanDir(layer.SourceDir)
	if err != nil {
		return fmt.Errorf("source directory: %w", err)
	}
	targetDir, err := cleanDir(layer.TargetDir)
	if err != nil {
		return fmt.Errorf("target directory: %w", err)
	}
	target := path.Join(set.Name, targetDir)

	switch {
	case layer.Image != "" && layer.Dir != "":
		return fmt.Errorf("names both an image and a directory")
	case layer.Image != "":
		disc, err := t.openImage(layer.Image, images)
		if err != nil {
			return err
		}
		return t.addImageLayer(set, layer, disc.FS(), srcDir, target)
	case layer.Dir != "":
		root, err := t.openRoot(layer.Dir, roots)
		if err != nil {
			return err
		}
		return t.addDirLayer(set, layer, root, srcDir, target)
	default:
		return fmt.Errorf("names neither an image nor a directory")
	}
}

// openImage opens a disc image once and shares it across every layer that
// draws from it.
func (t *Tree) openImage(image string, images map[string]*Disc) (*Disc, error) {
	key := filepath.Clean(image)
	if d, ok := images[key]; ok {
		return d, nil
	}
	d, err := OpenImage(key)
	if err != nil {
		return nil, err
	}
	images[key] = d
	t.closers = append(t.closers, d)
	return d, nil
}

// openRoot opens a directory layer under os.OpenRoot, which is what keeps
// a symlink inside it from serving a host path.
func (t *Tree) openRoot(dir string, roots map[string]*os.Root) (*os.Root, error) {
	key := filepath.Clean(dir)
	if r, ok := roots[key]; ok {
		return r, nil
	}
	r, err := os.OpenRoot(key)
	if err != nil {
		return nil, err
	}
	roots[key] = r
	t.closers = append(t.closers, r)
	return r, nil
}

// cleanDir normalizes a configured SourceDir or TargetDir to an io/fs
// name; empty means the whole root.
func cleanDir(dir string) (string, error) {
	if dir == "" {
		return ".", nil
	}
	clean := path.Clean(dir)
	if !fs.ValidPath(clean) {
		return "", fmt.Errorf("%q is not a relative path within its source", dir)
	}
	return clean, nil
}

// addImageLayer walks an EFS image from SourceDir down.
func (t *Tree) addImageLayer(set SetSpec, layer LayerSpec, fsys *efs.FS, srcDir, target string) error {
	ino, srcPath, err := lookupFollow(fsys, srcDir, maxSymlinks)
	if err != nil {
		return err
	}
	if !ino.IsDir() {
		return fmt.Errorf("%s is not a directory", srcDir)
	}
	dir, err := t.ensureDir(target, imageOrigin(layer, srcPath))
	if err != nil {
		return err
	}
	return t.walkImage(set, layer, fsys, ino, srcPath, target, dir)
}

func (t *Tree) walkImage(set SetSpec, layer LayerSpec, fsys *efs.FS, ino *efs.Inode, srcPath, target string, dir *node) error {
	ents, err := fsys.ReadDir(ino)
	if err != nil {
		return fmt.Errorf("%s: %w", srcPath, err)
	}
	for _, e := range ents {
		if e.Name == "." || e.Name == ".." {
			continue
		}
		childSrc := path.Join(srcPath, e.Name)
		childTarget := path.Join(target, e.Name)
		child, err := fsys.Inode(e.Ino)
		if err != nil {
			return fmt.Errorf("%s: %w", childSrc, err)
		}
		if child.IsSymlink() {
			// A link resolves within its own image - Lookup never leaves
			// the image's inode graph - so one aimed outside simply finds
			// nothing. Serving the set without it would be a quiet lie
			// about what the media holds, so say which link it was.
			linkSrc := childSrc
			child, childSrc, err = lookupFollow(fsys, linkSrc, maxSymlinks)
			if err != nil {
				return fmt.Errorf("symlink %s does not resolve within the image: %w", linkSrc, err)
			}
			// A link to a directory would duplicate a subtree and could
			// cycle; the tree materializes files, not link graphs.
			if child.IsDir() {
				return fmt.Errorf("symlink %s resolves to a directory; directory-link traversal is unsupported", linkSrc)
			}
		}
		switch {
		case child.IsDir():
			sub, err := mergeChild(dir, newDir(e.Name, imageOrigin(layer, childSrc)), childTarget, nil)
			if err != nil {
				return err
			}
			if err := t.walkImage(set, layer, fsys, child, childSrc, childTarget, sub); err != nil {
				return err
			}
		case child.IsRegular():
			f := &node{
				name:   e.Name,
				origin: imageOrigin(layer, childSrc),
				perm:   fs.FileMode(child.Mode & 0o7777),
				size:   int64(child.Size),
				mtime:  time.Unix(int64(child.Mtime), 0),
				uid:    uint32(child.UID),
				gid:    uint32(child.GID),
				nlink:  int(child.Nlink),
				image:  fsys,
				inode:  child,
			}
			if _, err := mergeChild(dir, f, childTarget, set.Collisions); err != nil {
				return err
			}
		}
	}
	return nil
}

// addDirLayer walks a pre-extracted directory layer from SourceDir down,
// through the os.Root that bounds it.
func (t *Tree) addDirLayer(set SetSpec, layer LayerSpec, root *os.Root, srcDir, target string) error {
	fsys := root.FS()
	info, err := fs.Stat(fsys, srcDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", srcDir)
	}
	dir, err := t.ensureDir(target, dirOrigin(layer, srcDir))
	if err != nil {
		return err
	}
	return t.walkDir(set, layer, root, fsys, srcDir, target, dir)
}

func (t *Tree) walkDir(set SetSpec, layer LayerSpec, root *os.Root, fsys fs.FS, srcPath, target string, dir *node) error {
	ents, err := fs.ReadDir(fsys, srcPath)
	if err != nil {
		return err
	}
	for _, e := range ents {
		childSrc := path.Join(srcPath, e.Name())
		childTarget := path.Join(target, e.Name())
		info, err := e.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			// os.Root follows a link that stays inside the layer and
			// refuses one that escapes, so a failed Stat here is exactly
			// the escape case. Skipping it would serve a set missing an
			// entry the layer lists, so fail and name the link.
			info, err = fs.Stat(fsys, childSrc)
			if err != nil {
				return fmt.Errorf("symlink %s escapes the layer or does not resolve: %w", childSrc, err)
			}
			if info.IsDir() {
				return fmt.Errorf("symlink %s resolves to a directory; directory-link traversal is unsupported", childSrc)
			}
		}
		switch {
		case info.IsDir():
			sub, err := mergeChild(dir, newDir(e.Name(), dirOrigin(layer, childSrc)), childTarget, nil)
			if err != nil {
				return err
			}
			if err := t.walkDir(set, layer, root, fsys, childSrc, childTarget, sub); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			// A directory layer reports no owner: the tree is read-only
			// and the host's uid/gid say nothing about the media.
			f := &node{
				name:   e.Name(),
				origin: dirOrigin(layer, childSrc),
				perm:   info.Mode().Perm(),
				size:   info.Size(),
				mtime:  info.ModTime(),
				nlink:  1,
				root:   root,
			}
			if _, err := mergeChild(dir, f, childTarget, set.Collisions); err != nil {
				return err
			}
		}
	}
	return nil
}

func imageOrigin(layer LayerSpec, srcPath string) Origin {
	return Origin{Kind: OriginImage, Source: layer.Name, Path: srcPath}
}

func dirOrigin(layer LayerSpec, srcPath string) Origin {
	return Origin{Kind: OriginDirectory, Source: layer.Name, Path: srcPath}
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

// lookupFollow resolves an in-image path, following up to depth symlinks.
// A relative target resolves against the link's own directory and an
// absolute one against the image root, so no target can name anything
// outside the image. It returns the resolved path too, because that is
// where the bytes actually live.
func lookupFollow(fsys *efs.FS, p string, depth int) (*efs.Inode, string, error) {
	p = path.Clean("/" + p)
	for {
		ino, err := fsys.Lookup(p)
		if err != nil {
			return nil, "", err
		}
		if !ino.IsSymlink() {
			return ino, strings.TrimPrefix(p, "/"), nil
		}
		if depth == 0 {
			return nil, "", fmt.Errorf("%s: too many levels of symbolic links", p)
		}
		depth--
		target, err := fsys.Readlink(ino)
		if err != nil {
			return nil, "", err
		}
		if strings.HasPrefix(target, "/") {
			p = path.Clean(target)
		} else {
			p = path.Clean(path.Dir(p) + "/" + target)
		}
	}
}
