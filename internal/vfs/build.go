package vfs

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"path"
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
// The resolver r opens each layer's source - local path or remote URL - and
// every unique source is opened once and stays open for the tree's life, so
// Close it when done.
//
// A layer's Dist and Stand are joined under its Base, so a source whose tree
// sits below a subdirectory is read as if that subdirectory were the root.
//
// Build fails rather than guess: a layer whose source or distribution
// directory is missing, a boot layer whose source has no stand directory,
// a file present with different bytes in two layers and no
// configured winner for that exact path, a configured winner no layer
// delivers, or a symlink that does not resolve, contained, to a regular
// file all stop the build. Content is read lazily on Open; only files two
// layers both claim are read here, to compare them.
func Build(sets []SetSpec, r Resolver) (*Tree, error) {
	digests, err := sourceDigests(sets)
	if err != nil {
		return nil, err
	}
	t := &Tree{root: newDir(".", Origin{})}
	resolved := map[string]Resolved{}

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
			if err := t.addLayer(set, layer, r, resolved, digests); err != nil {
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

// sourceDigests collects the expected sha256 of each layer source across every
// set. One source may be named by several layers; if more than one pins a
// non-empty digest they must agree. It returns source -> agreed digest (absent
// when no layer pins one) and fails on a conflict, so that resolving a shared
// source once - with this map's digest rather than whichever layer happens to
// resolve it first - can never skip a digest a later layer asked for. Layers
// are named, not their source URLs, to keep a possibly credential-bearing URL
// out of the error, as the rest of this package does.
func sourceDigests(sets []SetSpec) (map[string]string, error) {
	type pin struct{ digest, layer string }
	pins := map[string]pin{}
	for _, set := range sets {
		for _, layer := range set.Layers {
			if layer.Source == "" || layer.Sha256 == "" {
				continue
			}
			if prev, ok := pins[layer.Source]; ok && prev.digest != layer.Sha256 {
				return nil, fmt.Errorf("layers %q and %q pin the same source to conflicting sha256 digests", prev.layer, layer.Name)
			}
			pins[layer.Source] = pin{digest: layer.Sha256, layer: layer.Name}
		}
	}
	digests := make(map[string]string, len(pins))
	for src, p := range pins {
		digests[src] = p.digest
	}
	return digests, nil
}

// addLayer merges one layer's distribution directory into the set's dist,
// and for the set's boot layer serves its stand directory alongside. The
// resolver opens the layer's source, sharing an already-opened one when a
// later layer names the same reference, so each unique source is opened once
// and closed once with the tree. A shared source is resolved with the digest
// from digests, not this layer's own, so the sharing can never let a
// digest-bearing layer be satisfied by an earlier digestless resolution.
func (t *Tree) addLayer(set SetSpec, layer LayerSpec, r Resolver, resolved map[string]Resolved, digests map[string]string) error {
	if layer.Name == "" {
		return fmt.Errorf("layer with no name")
	}
	if layer.Source == "" {
		return fmt.Errorf("names no source")
	}
	res, ok := resolved[layer.Source]
	if !ok {
		var err error
		res, err = r.Resolve(layer.Source, digests[layer.Source])
		if err != nil {
			return err
		}
		resolved[layer.Source] = res
		if res.Closer != nil {
			t.closers = append(t.closers, res.Closer)
		}
	}

	dist, err := cleanDir(layer.Dist)
	if err != nil {
		return fmt.Errorf("distribution directory: %w", err)
	}
	distPath, err := underBase(layer.Base, dist)
	if err != nil {
		return fmt.Errorf("distribution directory: %w", err)
	}
	if err := t.mergeTree(set, layer, res.FS, res.Kind, distPath, path.Join(set.Name, distDir)); err != nil {
		return err
	}
	if !layer.Boot {
		return nil
	}
	stand, err := standOr(layer.Stand)
	if err != nil {
		return fmt.Errorf("stand directory: %w", err)
	}
	standPath, err := underBase(layer.Base, stand)
	if err != nil {
		return fmt.Errorf("stand directory: %w", err)
	}
	if _, err := fs.Stat(res.FS, standPath); err != nil {
		return fmt.Errorf("boot layer has no %s directory: %w", standDir, err)
	}
	return t.mergeTree(set, layer, res.FS, res.Kind, standPath, path.Join(set.Name, standDir))
}

// underBase joins a layer's distribution or stand directory under its base
// and confirms the result stays a relative path within the source. An empty
// base leaves dir where it is.
func underBase(base, dir string) (string, error) {
	joined := dir
	if base != "" {
		joined = path.Join(base, dir)
	}
	if !fs.ValidPath(joined) {
		return "", fmt.Errorf("%q is not a relative path within its source", joined)
	}
	return joined, nil
}

// cleanDir normalizes a configured Dist to an io/fs name; empty means the
// ordinary "dist", which is what all but the version-stub media use.
func cleanDir(dir string) (string, error) {
	return cleanRel(dir, distDir)
}

// standOr normalizes a configured Stand to an io/fs name; empty means the
// ordinary "stand", the boot directory every bootable medium ships.
func standOr(dir string) (string, error) {
	return cleanRel(dir, standDir)
}

// cleanRel normalizes a configured relative directory to an io/fs name,
// defaulting an empty one to def and refusing anything that would climb out
// of its source.
func cleanRel(dir, def string) (string, error) {
	if dir == "" {
		return def, nil
	}
	clean := path.Clean(dir)
	if !fs.ValidPath(clean) {
		return "", fmt.Errorf("%q is not a relative path within its source", dir)
	}
	return clean, nil
}

// mergeTree walks one subtree of a source onto a tree path. srcDir is the
// directory within the source (dist or stand); target is where it lands.
func (t *Tree) mergeTree(set SetSpec, layer LayerSpec, fsys fs.FS, kind OriginKind, srcDir, target string) error {
	info, srcPath, err := statFollow(fsys, srcDir)
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
	return t.walkFS(set, layer, fsys, kind, srcPath, target, dir)
}

func (t *Tree) walkFS(set SetSpec, layer LayerSpec, fsys fs.FS, kind OriginKind, srcPath, target string, dir *node) error {
	ents, err := fs.ReadDir(fsys, srcPath)
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
			info, childSrc, err = resolveLink(fsys, childSrc)
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
			if err := t.walkFS(set, layer, fsys, kind, childSrc, childTarget, sub); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			uid, gid, nlink, perm := ownerOf(info)
			f := &node{
				name:   e.Name(),
				origin: originOf(kind, layer, childSrc),
				perm:   perm,
				size:   info.Size(),
				mtime:  info.ModTime(),
				uid:    uid,
				gid:    gid,
				nlink:  nlink,
				fsys:   fsys,
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
//
// A relative target that climbs strictly above the source root and back
// down onto a real file (dist/link -> ../../foo, with an in-image /foo) is
// one such error here: path.Join leaves a leading ".." that fs.ValidPath
// refuses, so the next fs.Stat fails before it ever reaches the file. The
// old EFS walker resolved names in an absolute image namespace, where
// path.Clean simply clamps a "/.." at the root, and would have served it.
// That divergence is deliberate, not a regression: refusing the escape
// instead of silently clamping it matches this package's fail-rather-
// than-guess stance, and no real IRIX install media links above its own
// dist, so it is unobservable in practice. A directory layer never sees
// this either way - os.Root already refuses any target that would leave
// the layer, before resolveLink runs.
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

// ownerOf reads the metadata an EFS source reports through Sys(): the
// owner, link count, and full permission bits. The mode is taken from
// Sys() rather than info.Mode() because io/fs.FileInfo.Mode() masks the
// setuid, setgid, and sticky bits an EFS regular file may carry, and the
// tree serves those bits as the media holds them. A directory layer
// reports no Sys() owner, so its files stay unowned with a single link,
// but its mode can carry the same three bits - os reports them as the
// symbolic ModeSetuid, ModeSetgid, and ModeSticky, which Perm() drops -
// so they are mapped back onto the numeric form the tree serves.
func ownerOf(info fs.FileInfo) (uid, gid uint32, nlink int, perm fs.FileMode) {
	if st, ok := info.Sys().(*efs.Stat); ok {
		return uint32(st.UID), uint32(st.GID), int(st.Nlink), fs.FileMode(st.Mode & 0o7777)
	}
	mode := info.Mode()
	perm = mode.Perm()
	if mode&fs.ModeSetuid != 0 {
		perm |= 0o4000
	}
	if mode&fs.ModeSetgid != 0 {
		perm |= 0o2000
	}
	if mode&fs.ModeSticky != 0 {
		perm |= 0o1000
	}
	return 0, 0, 1, perm
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
