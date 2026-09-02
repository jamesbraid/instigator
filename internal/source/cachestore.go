package source

import (
	"fmt"
	"io"
	"os"
)

// cacheStore is an httpreaderat.Store that spills a non-range whole-object
// download to a file under cacheDir. httpreaderat's own StoreFile uses the OS
// temp dir, which the scratch container lacks; Close removes the file.
type cacheStore struct {
	dir  string
	f    *os.File
	size int64
}

func newCacheStore(dir string) *cacheStore { return &cacheStore{dir: dir} }

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
	name := s.f.Name()
	err := s.f.Close()
	if rmErr := os.Remove(name); rmErr != nil && err == nil {
		err = rmErr
	}
	s.f = nil
	s.size = 0
	return err
}
