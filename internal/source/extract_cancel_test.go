package source

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A stop must interrupt one long decompression. The .gz repeats a gzip member
// of zeros: small on disk, seconds to decompress.
func TestResolveCancelsDuringOneLongGzip(t *testing.T) {
	var member bytes.Buffer
	gz, err := gzip.NewWriterLevel(&member, gzip.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gz.Write(make([]byte, 64<<20)); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "disc.image.gz")
	if err := os.WriteFile(archive, bytes.Repeat(member.Bytes(), 64), 0o644); err != nil {
		t.Fatal(err)
	}

	cache := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := New(Options{CacheDir: cache, Context: ctx}).Resolve(archive)
		done <- err
	}()
	out := filepath.Join(cache, "extract-*", "tree", "disc.image")
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if m, _ := filepath.Glob(out); len(m) > 0 {
			if fi, err := os.Stat(m[0]); err == nil && fi.Size() > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("decompression never started")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Resolve = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Resolve still decompressing 1s after the cancel")
	}
	if left, _ := os.ReadDir(cache); len(left) > 0 {
		t.Fatalf("cache holds %v after the cancel, want nothing", left)
	}
}
