package source

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestCacheStoreRoundTrip: the store buffers into a file under the cache dir
// (not the OS temp dir), reads it back, and removes it on Close.
func TestCacheStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := newCacheStore(dir)
	data := []byte("the quick brown fox")

	n, err := s.ReadFrom(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadFrom: %v", err)
	}
	if n != int64(len(data)) || s.Size() != int64(len(data)) {
		t.Fatalf("size = %d / Size()=%d, want %d", n, s.Size(), len(data))
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "fallback-*")); len(files) != 1 {
		t.Fatalf("fallback files under cache dir = %v, want exactly 1", files)
	}

	got := make([]byte, 5)
	if _, err := s.ReadAt(got, 4); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(got) != "quick" {
		t.Errorf("ReadAt = %q, want %q", got, "quick")
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if files, _ := filepath.Glob(filepath.Join(dir, "fallback-*")); len(files) != 0 {
		t.Errorf("fallback file survived Close: %v", files)
	}
}

// TestCacheStoreCreatesMissingDir mirrors the scratch container, which has no
// /tmp: the store must create its cache directory rather than depend on one
// already existing, which is why it cannot use httpreaderat's default
// os.TempDir-backed StoreFile.
func TestCacheStoreCreatesMissingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist", "yet")
	s := newCacheStore(dir)
	if _, err := s.ReadFrom(bytes.NewReader([]byte("x"))); err != nil {
		t.Fatalf("ReadFrom into missing dir: %v", err)
	}
	defer s.Close()
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("cache dir not created: %v", err)
	}
}
