package source

import (
	"fmt"
	"os"
	"testing"
)

func TestCacheKeyPrecedence(t *testing.T) {
	c := NewCache(t.TempDir())
	byEtag := c.Key("u", "etag-1", "Mon", 10)
	byLM := c.Key("u", "", "Mon", 10)
	byURL := c.Key("u", "", "", 0)
	if byEtag == byLM || byLM == byURL || byEtag == byURL {
		t.Fatalf("keys not distinct: %s %s %s", byEtag, byLM, byURL)
	}
	if c.Key("u", "etag-1", "x", 9) != byEtag {
		t.Error("etag key should ignore last-modified/len")
	}
}

func TestCacheInstallAtomicAndIdempotent(t *testing.T) {
	c := NewCache(t.TempDir())
	calls := 0
	write := func(dst string) error { calls++; return os.WriteFile(dst, []byte("data"), 0o644) }
	p1, err := c.Install("k", "obj", write)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := c.Install("k", "obj", write)
	if err != nil {
		t.Fatal(err)
	}
	if p1 != p2 || calls != 1 {
		t.Fatalf("expected one write and a stable path; calls=%d p1=%s p2=%s", calls, p1, p2)
	}
	if b, _ := os.ReadFile(p1); string(b) != "data" {
		t.Errorf("content = %q, want data", b)
	}

	c2 := NewCache(t.TempDir())
	_, err = c2.Install("k", "obj", func(dst string) error { return fmt.Errorf("boom") })
	if err == nil || c2.Has("k", "obj") {
		t.Error("failed write must install nothing")
	}
}
