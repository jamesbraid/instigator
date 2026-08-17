package vfs

import (
	"fmt"
	"io/fs"
	"path"
	"time"
)

// AddGenerated inserts a file instigator synthesizes in memory at the
// given io/fs path, creating any missing parent directories, and records
// generator as its origin. Generated files then read, list, stat and
// resolve exactly like backed ones - there is no side map for a caller to
// miss.
//
// An existing regular file at path is shadowed: its bytes and origin are
// replaced. That is how the generated .related_dists menu aid takes over
// the stock copy a primary layer's media ships. An existing directory is
// an error, because burying a directory of served media under one file is
// never what a generator means.
func (t *Tree) AddGenerated(name, generator string, content []byte) error {
	if !fs.ValidPath(name) || name == "." {
		return &fs.PathError{Op: "addgenerated", Path: name, Err: fs.ErrInvalid}
	}
	origin := Origin{Kind: OriginGenerated, Source: generator}
	parent, err := t.ensureDir(path.Dir(name), origin)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	base := path.Base(name)
	n, ok := parent.children[base]
	if !ok {
		n = &node{name: base}
		parent.children[base] = n
	} else if n.dir {
		return fmt.Errorf("%s: a served directory is already there", name)
	}
	n.origin = origin
	n.data = content
	n.size = int64(len(content))
	n.perm = 0o444
	n.mtime = time.Time{}
	n.nlink = 1
	n.uid, n.gid = 0, 0
	n.image, n.inode, n.root = nil, nil, nil
	t.number()
	return nil
}
