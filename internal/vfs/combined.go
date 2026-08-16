package vfs

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// combined presents a full IRIX distribution set the way inst's overlay
// resolver needs it: each disc served WHOLE at /<name>/<slug>/, never
// flattened into one dist. Flattening is fatal twice over. It merges the
// version-conditional .redirect stub a devfoundation/nfs disc carries into
// the top dist/, and inst - which checks .redirect first when it opens a
// distribution - follows that redirect, fails, and reopens forever. And a
// highest-file-wins merge lets an overlay's product files bury the base
// versions, which inst must keep as a separate open distribution to
// resolve the overlay's prerequisite edges against.
//
// The primary disc (the Installation-Tools/Overlays1 disc: it carries the
// miniroot and the stock .related_dists marker) gets a SYNTHESIZED
// .related_dists naming every other disc by relative path, so a single
// `from <server>:/<name>/<primary>/dist` makes inst auto-open the whole
// set. A .redirect disc is named PAST its stub, at the resolved dist6.5
// subdir; instigator only ever targets 6.5.x, so that branch is always
// the right one.
type combined struct {
	discs   map[string]*Disc // slug -> disc, each mounted whole
	order   []string         // slugs in config order
	primary string           // slug of the disc bearing .related_dists
	related []byte           // synthesized .related_dists for the primary
}

// relatedDistsName is the file inst reads to learn about sibling
// distributions; combined shadows the primary disc's copy with a
// generated path list in place of its stock "CD" marker.
const relatedDistsName = ".related_dists"

// AddCombined opens the given CD images as a combined distribution set
// served under /<name>/. Each disc keeps its own dist; the primary disc's
// .related_dists is synthesized to chain the rest. discNames overrides a
// disc's serve slug per image filename, like a media set's, keeping the
// paths short and readable; a filename with no override falls back to the
// slugged name. It returns the serve path of the primary's dist - the
// single path an operator points inst at.
func (t *Tree) AddCombined(name string, imagePaths []string, discNames map[string]string) (string, error) {
	if _, dup := t.medias[name]; dup {
		return "", fmt.Errorf("combined %q clashes with a media set", name)
	}
	if _, dup := t.combined[name]; dup {
		return "", fmt.Errorf("combined %q defined twice", name)
	}
	c := &combined{discs: map[string]*Disc{}}
	type entry struct{ slug, distPath string }
	var entries []entry
	for _, p := range imagePaths {
		d, err := OpenImage(p)
		if err != nil {
			c.close()
			return "", fmt.Errorf("combined %q: %s: %w", name, p, err)
		}
		s := discNames[filepath.Base(p)]
		if s == "" {
			s = slug(filepath.Base(p))
		}
		if _, dup := c.discs[s]; dup {
			d.Close()
			c.close()
			return "", fmt.Errorf("combined %q: two images slug to %q", name, s)
		}
		c.discs[s] = d
		c.order = append(c.order, s)

		dp, err := distPathFor(d)
		if err != nil {
			c.close()
			return "", fmt.Errorf("combined %q: %s: %w", name, s, err)
		}
		entries = append(entries, entry{s, dp})

		if hasPath(d, "dist/"+relatedDistsName) {
			if c.primary != "" {
				c.close()
				return "", fmt.Errorf("combined %q: two primary discs (%q and %q) carry dist/%s",
					name, c.primary, s, relatedDistsName)
			}
			c.primary = s
		}
	}
	if len(c.order) == 0 {
		return "", fmt.Errorf("combined %q: no images", name)
	}
	if c.primary == "" {
		c.close()
		return "", fmt.Errorf("combined %q: no primary disc (none carries dist/%s)", name, relatedDistsName)
	}

	// The synthesized .related_dists names every OTHER disc by a path
	// relative to the primary disc's own directory - the base inst
	// resolves a .related_dists entry against - matching the field recipe
	// (../<disc>/dist, redirect discs ../<disc>/dist/dist6.5).
	var b strings.Builder
	for _, e := range entries {
		if e.slug == c.primary {
			continue
		}
		fmt.Fprintf(&b, "../%s/%s\n", e.slug, e.distPath)
	}
	c.related = []byte(b.String())

	if t.combined == nil {
		t.combined = map[string]*combined{}
	}
	t.combined[name] = c
	return "/" + name + "/" + c.primary + "/dist", nil
}

// hasPath reports whether a disc resolves the given EFS path.
func hasPath(d *Disc, p string) bool {
	_, err := d.lookupFollow(p, 8)
	return err == nil
}

// distPathFor returns the catalog dist within a disc: "dist" for an
// ordinary disc, "dist/dist6.5" for a version-stub disc whose dist/ holds
// a .redirect. A stub missing dist6.5 is an error rather than a silent
// fall back to the stub, which would loop inst.
func distPathFor(d *Disc) (string, error) {
	if !hasPath(d, "dist/.redirect") {
		return "dist", nil
	}
	if !hasPath(d, "dist/dist6.5") {
		return "", fmt.Errorf("dist/.redirect present but dist/dist6.5 missing")
	}
	return "dist/dist6.5", nil
}

// split separates a combined-relative path into "<slug>" and the path
// within that disc; a bare slug yields the disc root "/".
func (c *combined) split(rest string) (slug, sub string, ok bool) {
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return "", "", false
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i], rest[i+1:], true
	}
	return rest, "/", true
}

// isRelated reports whether sub within slug is the primary's synthesized
// .related_dists, the one path whose content combined generates.
func (c *combined) isRelated(slug, sub string) bool {
	return slug == c.primary && strings.Trim(sub, "/") == "dist/"+relatedDistsName
}

func (c *combined) open(rest string) (File, error) {
	slug, sub, ok := c.split(rest)
	if !ok {
		return nil, ErrNotFound
	}
	if c.isRelated(slug, sub) {
		return &bytesFile{r: bytes.NewReader(c.related), size: int64(len(c.related))}, nil
	}
	d := c.discs[slug]
	if d == nil {
		return nil, ErrNotFound
	}
	node, err := d.lookupFollow(sub, 8)
	if err != nil || !node.IsRegular() {
		return nil, ErrNotFound
	}
	return &efsFile{fs: d.FS(), node: node}, nil
}

func (c *combined) readdir(rest string) ([]string, error) {
	rest = strings.Trim(rest, "/")
	if rest == "" {
		out := append([]string(nil), c.order...)
		sort.Strings(out)
		return out, nil
	}
	slug, sub, _ := c.split(rest)
	d := c.discs[slug]
	if d == nil {
		return nil, fmt.Errorf("%s: %w", slug, ErrNotFound)
	}
	node, err := d.lookupFollow(sub, 8)
	if err != nil || !node.IsDir() {
		return nil, fmt.Errorf("%s: %w", rest, ErrNotFound)
	}
	ents, err := d.FS().ReadDir(node)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.Name == "." || e.Name == ".." {
			continue
		}
		names = append(names, e.Name)
	}
	sort.Strings(names)
	return names, nil
}

func (c *combined) stat(rest string) (FileInfo, error) {
	trimmed := strings.Trim(rest, "/")
	if trimmed == "" {
		return syntheticDir(), nil
	}
	slug, sub, _ := c.split(trimmed)
	d := c.discs[slug]
	if d == nil {
		return FileInfo{}, fmt.Errorf("%s: %w", slug, ErrNotFound)
	}
	if c.isRelated(slug, sub) {
		return FileInfo{Ino: syntheticDirIno + 1, IsDir: false, Perm: 0o444, Nlink: 1, Size: int64(len(c.related))}, nil
	}
	node, err := d.lookupFollow(sub, 8)
	if err != nil {
		return FileInfo{}, fmt.Errorf("%s: %w", trimmed, ErrNotFound)
	}
	return inodeInfo(node), nil
}

func (c *combined) close() {
	for _, d := range c.discs {
		d.Close()
	}
}
