package vfs

import (
	"io"
	"io/fs"
)

// Resolved is a layer's source, opened: the read-only filesystem it serves,
// the kind of payload it is, and the closer that frees it.
type Resolved struct {
	FS     fs.FS
	Kind   OriginKind
	Closer io.Closer
}

// Resolver turns a layer's source reference - a local path or a URL - into
// a read-only filesystem, the kind of payload it is, and the closer that
// frees it.
type Resolver interface {
	Resolve(ref string) (Resolved, error)
}
