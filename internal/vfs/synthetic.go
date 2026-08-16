package vfs

import "bytes"

// synthetic files are generated in memory (not read from any disc) and
// served at a fixed top-level path - the install runbook is one. They let
// instigator hand an operator the exact procedure straight from the
// server, alongside the media.
func (t *Tree) AddSynthetic(name string, content []byte) {
	if t.synthetic == nil {
		t.synthetic = map[string][]byte{}
	}
	t.synthetic[name] = content
}

// bytesFile serves an in-memory synthetic file.
type bytesFile struct {
	r    *bytes.Reader
	size int64
}

func (f *bytesFile) ReadAt(p []byte, off int64) (int, error) { return f.r.ReadAt(p, off) }
func (f *bytesFile) Size() int64                             { return f.size }
