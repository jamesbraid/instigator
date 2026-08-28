package source

import (
	"fmt"
	"io"
	"os"
)

// cacheStore is an httpreaderat.Store whose fallback temporary file lives under
// the resolver's cache directory rather than the OS temp dir. It is used only
// when a raw-image server ignores Range requests and httpreaderat must buffer
// the whole object: writing it to a file keeps a large image off the heap.
//
// httpreaderat ships StoreFile, but that calls os.CreateTemp("", ...), which
// needs an OS temp dir - and the shipped scratch container has no /tmp, so the
// fallback would fail there outright. Keying the temp file to cacheDir (which
// the config already points at real storage) makes the fallback work inside the
// container and keeps it under the operator's chosen directory. The file is
// removed on Close.
type cacheStore struct {
	dir  string
	f    *os.File
	size int64
}

func newCacheStore(dir string) *cacheStore { return &cacheStore{dir: dir} }

// ReadFrom buffers r into a fresh temporary file under dir, replacing any
// previous contents. It is not safe for concurrent use, matching the
// httpreaderat.Store contract; httpreaderat calls it once before serving reads.
func (s *cacheStore) ReadFrom(r io.Reader) (int64, error) {
	if s.f != nil {
		s.Close()
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return 0, fmt.Errorf("source: create cache dir: %w", err)
	}
	f, err := os.CreateTemp(s.dir, "fallback-*")
	if err != nil {
		return 0, fmt.Errorf("source: create fallback file: %w", err)
	}
	s.f = f
	n, err := io.Copy(f, r)
	s.size = n
	return n, err
}

// ReadAt serves bytes from the buffered file. It is safe for concurrent use.
// Before any ReadFrom (the range path never buffers) it reports an empty store.
func (s *cacheStore) ReadAt(p []byte, off int64) (int, error) {
	if s.f == nil {
		return 0, nil
	}
	return s.f.ReadAt(p, off)
}

// Size is the number of bytes buffered by the last ReadFrom.
func (s *cacheStore) Size() int64 { return s.size }

// Close closes and removes the temporary file. It is a no-op on the range
// path, where ReadFrom never ran.
func (s *cacheStore) Close() error {
	if s.f == nil {
		return nil
	}
	name := s.f.Name()
	err := s.f.Close()
	if rmErr := os.Remove(name); rmErr != nil && err == nil {
		err = rmErr
	}
	s.f = nil
	s.size = 0
	return err
}
