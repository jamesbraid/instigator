package source

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// A partial download left in the cache by an earlier run, from a file the
// server has since replaced, must not be resumed into a spliced archive.
func TestDownloadDoesNotResumeAnEarlierPartial(t *testing.T) {
	old, cur := bytes.Repeat([]byte("O"), 1000), bytes.Repeat([]byte("N"), 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "disc.tar.gz", time.Time{}, bytes.NewReader(cur))
	}))
	defer srv.Close()
	r := New(Options{CacheDir: t.TempDir(), Client: srv.Client()})
	url := srv.URL + "/disc.tar.gz"
	dst := r.cacheDest(url, "disc.tar.gz")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, old, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.download(context.Background(), url, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, cur) {
		t.Fatalf("cached file holds %d bytes starting %q, want the server's current file", len(got), got[:1])
	}
}

// A download cut off partway leaves nothing at the cache name for a later run
// to take as complete.
func TestDownloadCutOffLeavesNoCachedFile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.Write(make([]byte, 1000))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()
	r := New(Options{CacheDir: t.TempDir(), Client: srv.Client()})
	url := srv.URL + "/disc.tar.gz"
	dst := r.cacheDest(url, "disc.tar.gz")
	if err := r.download(context.Background(), url, dst); err == nil {
		t.Fatal("download of a cut-off file succeeded")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("cache name after a cut-off download: %v, want it absent", err)
	}
}

// Two runs sharing a cache can fetch the same URL at once. Neither may write
// into the other's download.
func TestDownloadSameURLAtOnce(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789abcdef"), 1<<14)
	var arrived sync.WaitGroup
	arrived.Add(2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			arrived.Done()
			arrived.Wait()
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		for i := 0; i < len(body); i += 16 << 10 {
			w.Write(body[i : i+16<<10])
			w.(http.Flusher).Flush()
			time.Sleep(time.Millisecond)
		}
	}))
	defer srv.Close()
	cache, url := t.TempDir(), srv.URL+"/disc.tar.gz"
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			r := New(Options{CacheDir: cache, Client: srv.Client()})
			errs <- r.download(context.Background(), url, r.cacheDest(url, "disc.tar.gz"))
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	dst := New(Options{CacheDir: cache}).cacheDest(url, "disc.tar.gz")
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, body) {
		t.Fatalf("cached file holds %d bytes, want the server's %d intact", len(got), len(body))
	}
}

// A download a killed run left is swept when the next one starts, a live one
// is kept, and a finished one leaves no entry behind.
func TestDownloadSweepsAbandonedDownload(t *testing.T) {
	sweepAnyAge(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("archive"))
	}))
	defer srv.Close()
	cache := t.TempDir()
	for _, name := range []string{"download-dead", "download-live"} {
		if err := os.MkdirAll(filepath.Join(cache, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache, name, lockName), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	live := flock.New(filepath.Join(cache, "download-live", lockName))
	if err := live.Lock(); err != nil {
		t.Fatal(err)
	}
	defer live.Unlock()
	r := New(Options{CacheDir: cache, Client: srv.Client()})
	url := srv.URL + "/disc.tar.gz"
	if err := r.download(context.Background(), url, r.cacheDest(url, "disc.tar.gz")); err != nil {
		t.Fatal(err)
	}
	<-startSweep(cache)
	left, _ := filepath.Glob(filepath.Join(cache, "download-*"))
	if want := []string{filepath.Join(cache, "download-live")}; !slices.Equal(left, want) {
		t.Fatalf("download entries %v, want only %v", left, want)
	}
}

// A cached copy whose check against the server fails for another reason is
// kept, and the error returned, rather than downloaded again.
func TestDownloadCheckFailureKeepsCachedCopy(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) <= 2 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("new archive"))
	}))
	defer srv.Close()
	r := New(Options{CacheDir: t.TempDir(), Client: srv.Client()})
	url := srv.URL + "/disc.tar.gz"
	dst := r.cacheDest(url, "disc.tar.gz")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.download(context.Background(), url, dst); err == nil {
		t.Fatal("download succeeded although the server failed the check")
	}
	if got, _ := os.ReadFile(dst); string(got) != "cached" {
		t.Fatalf("cached copy = %q after a failed check, want it kept", got)
	}
}
