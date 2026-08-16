package vfs

import (
	"fmt"
	"sort"
	"strings"
)

// union merges the dist directories of several discs into one virtual
// dist. Layers are ordered low to high precedence: for a product present
// on more than one disc, the highest layer wins, which is how an IRIX
// overlay supersedes the base foundation. inst opens the union's single
// dist path once instead of every disc in turn.
type union struct {
	layers []*Disc // low -> high precedence
}

// AddUnion opens the given CD images as the layers of a named union,
// lowest precedence first. The union is served at /<name>/dist.
func (t *Tree) AddUnion(name string, imagePaths []string) error {
	if _, dup := t.medias[name]; dup {
		return fmt.Errorf("union %q clashes with a media set", name)
	}
	if _, dup := t.unions[name]; dup {
		return fmt.Errorf("union %q defined twice", name)
	}
	u := &union{}
	for _, p := range imagePaths {
		d, err := OpenImage(p)
		if err != nil {
			return fmt.Errorf("union %q: %s: %w", name, p, err)
		}
		u.layers = append(u.layers, d)
	}
	if len(u.layers) == 0 {
		return fmt.Errorf("union %q: no images", name)
	}
	if t.unions == nil {
		t.unions = map[string]*union{}
	}
	t.unions[name] = u
	return nil
}

// unionOpen resolves a regular file within a union, searching layers from
// highest precedence down so an overlay's copy wins.
func (u *union) open(rest string) (File, error) {
	if !strings.HasPrefix(strings.Trim(rest, "/")+"/", "dist/") {
		return nil, ErrNotFound
	}
	for i := len(u.layers) - 1; i >= 0; i-- {
		d := u.layers[i]
		node, err := d.lookupFollow(rest, 8)
		if err != nil || !node.IsRegular() {
			continue
		}
		return &efsFile{fs: d.FS(), node: node}, nil
	}
	return nil, ErrNotFound
}

// readdir lists a directory within a union, merging every layer that has
// it and reporting each name once. The union root holds only "dist".
func (u *union) readdir(rest string) ([]string, error) {
	rest = strings.Trim(rest, "/")
	if rest == "" {
		return []string{"dist"}, nil
	}
	if !strings.HasPrefix(rest+"/", "dist/") {
		return nil, ErrNotFound
	}
	seen := map[string]bool{}
	var names []string
	found := false
	for i := len(u.layers) - 1; i >= 0; i-- {
		node, err := u.layers[i].lookupFollow(rest, 8)
		if err != nil || !node.IsDir() {
			continue
		}
		found = true
		ents, err := u.layers[i].FS().ReadDir(node)
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			if e.Name == "." || e.Name == ".." || seen[e.Name] {
				continue
			}
			seen[e.Name] = true
			names = append(names, e.Name)
		}
	}
	if !found {
		return nil, fmt.Errorf("%s: %w", rest, ErrNotFound)
	}
	sort.Strings(names)
	return names, nil
}

func (u *union) close() {
	for _, d := range u.layers {
		d.Close()
	}
}
