package source

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFetchWholeVerifiesSha256(t *testing.T) {
	body := []byte("archive-bytes")
	sum := fmt.Sprintf("%x", sha256.Sum256(body))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "obj")
	if err := fetchWhole(context.Background(), srv.Client(), srv.URL, nil, sum, dst); err != nil {
		t.Fatalf("good checksum: %v", err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "archive-bytes" {
		t.Errorf("fetched %q", b)
	}
	if err := fetchWhole(context.Background(), srv.Client(), srv.URL, nil, "00"+sum[2:], filepath.Join(t.TempDir(), "obj2")); err == nil {
		t.Fatal("bad checksum accepted")
	}
}
