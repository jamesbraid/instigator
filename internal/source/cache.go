package source

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Cache is a directory keyed by content identity, so a fetch or extraction
// of the same content is installed once and reused thereafter.
type Cache struct {
	dir string
}

// NewCache returns a Cache rooted at dir. dir need not exist yet; Install
// creates the subdirectories it needs.
func NewCache(dir string) *Cache {
	return &Cache{dir: dir}
}

// Key returns a stable, filesystem-safe identifier for an object at url.
// It prefers etag, then falls back to lastModified+contentLength, then to
// url alone, so two fetches of unchanged content land on the same key even
// when the server offers only a subset of that metadata.
func (c *Cache) Key(url string, etag, lastModified string, contentLength int64) string {
	switch {
	case etag != "":
		return hashHex(etag)
	case lastModified != "":
		return hashHex(fmt.Sprintf("%s\n%s\n%d", url, lastModified, contentLength))
	default:
		return hashHex(url)
	}
}

// hashHex returns the hex-encoded sha256 of s.
func hashHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Path returns the cache location for name under key, whether or not it
// has been installed yet.
func (c *Cache) Path(key, name string) string {
	return filepath.Join(c.dir, key, name)
}

// Has reports whether name is already installed under key.
func (c *Cache) Has(key, name string) bool {
	_, err := os.Lstat(c.Path(key, name))
	return err == nil
}

// Install returns the cache path for name under key, populating it first
// if necessary. If already present, it returns that path without calling
// write. Otherwise it creates the key directory, calls write with a
// non-existent sibling path for it to create — as a file or, via
// os.MkdirAll, as a directory tree — and on success renames that path into
// place atomically (a same-directory rename) and returns it. If write
// fails, the temp path is removed and nothing is installed.
func (c *Cache) Install(key, name string, write func(dst string) error) (string, error) {
	final := c.Path(key, name)
	if c.Has(key, name) {
		return final, nil
	}

	keyDir := filepath.Dir(final)
	if err := os.MkdirAll(keyDir, 0o755); err != nil {
		return "", fmt.Errorf("source: create cache dir %s: %w", keyDir, err)
	}

	// Mint a unique temp path in the key dir so concurrent Install calls
	// for the same key never collide. write needs a path that does not
	// yet exist (it may create a file or a directory there), so the temp
	// file CreateTemp opens is removed immediately rather than reused.
	tmpFile, err := os.CreateTemp(keyDir, name+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("source: create temp in %s: %w", keyDir, err)
	}
	tmp := tmpFile.Name()
	tmpFile.Close()
	if err := os.Remove(tmp); err != nil {
		return "", fmt.Errorf("source: clear temp placeholder %s: %w", tmp, err)
	}

	if err := write(tmp); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}

	if err := os.Rename(tmp, final); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("source: install %s: %w", final, err)
	}
	return final, nil
}
