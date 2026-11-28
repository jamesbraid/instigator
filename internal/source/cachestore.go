package source

import (
	"fmt"
	"io"
	"os"
)

// cacheStore is the rangeReader's fallback: it spills a non-range whole-object
// download to a file under cacheDir rather than the OS temp dir, which the
// scratch container lacks. Close removes the file.
type cacheStore struct {
	dir   string
	f     *os.File
	entry *cacheEntry
	size  int64
}

func newCacheStore(dir string) *cacheStore { return &cacheStore{dir: dir} }

func (s *cacheStore) ReadFrom(r io.Reader) (int64, error) {
	if s.f != nil {
		s.Close()
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return 0, fmt.Errorf("source: create cache dir: %w", err)
	}
	e, err := newCacheEntry(s.dir, "fallback", "data")
	if err != nil {
		return 0, fmt.Errorf("source: create fallback file: %w", err)
	}
	f, err := os.OpenFile(e.name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		e.remove()
		return 0, fmt.Errorf("source: create fallback file: %w", err)
	}
	s.f, s.entry = f, e
	n, err := io.Copy(f, r)
	s.size = n
	return n, err
}

func (s *cacheStore) ReadAt(p []byte, off int64) (int, error) {
	if s.f == nil {
		return 0, nil
	}
	return s.f.ReadAt(p, off)
}

func (s *cacheStore) Size() int64 { return s.size }

func (s *cacheStore) Close() error {
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	if rmErr := s.entry.remove(); rmErr != nil && err == nil {
		err = rmErr
	}
	s.f, s.entry = nil, nil
	s.size = 0
	return err
}
