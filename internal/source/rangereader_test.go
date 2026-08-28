package source

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func rangeServer(t *testing.T, body []byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		http.ServeContent(w, r, "img", time.Unix(0, 0), bytes.NewReader(body)) // honours Range, sets 206/Content-Range
	}))
}

func TestRangeReaderAtReadsMidFile(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 2000) // 20000 bytes
	srv := rangeServer(t, body)
	defer srv.Close()

	size, ranges, etag, lm, err := probeRange(context.Background(), srv.Client(), srv.URL, nil)
	if err != nil || !ranges || size != int64(len(body)) {
		t.Fatalf("probe: size=%d ranges=%v err=%v", size, ranges, err)
	}
	ra := newRangeReaderAt(context.Background(), srv.Client(), srv.URL, nil, size, etag, lm)
	defer ra.Close()

	p := make([]byte, 5)
	if _, err := ra.ReadAt(p, 12003); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if want := body[12003:12008]; !bytes.Equal(p, want) {
		t.Errorf("ReadAt(12003) = %q, want %q", p, want)
	}
}

// TestRangeReaderAtCachesChunk asserts that a second ReadAt landing in an
// already-fetched chunk does not issue a second HTTP request.
func TestRangeReaderAtCachesChunk(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 2000) // 20000 bytes, well under one 4MiB chunk
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Accept-Ranges", "bytes")
		http.ServeContent(w, r, "img", time.Unix(0, 0), bytes.NewReader(body))
	}))
	defer srv.Close()

	size, ranges, etag, lm, err := probeRange(context.Background(), srv.Client(), srv.URL, nil)
	if err != nil || !ranges {
		t.Fatalf("probe: size=%d ranges=%v err=%v", size, ranges, err)
	}
	hits = 0 // reset after probe's own request

	ra := newRangeReaderAt(context.Background(), srv.Client(), srv.URL, nil, size, etag, lm)
	defer ra.Close()

	p := make([]byte, 5)
	if _, err := ra.ReadAt(p, 100); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if _, err := ra.ReadAt(p, 200); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Errorf("hits = %d, want 1 (second read should hit the chunk cache)", hits)
	}
}

// TestRangeReaderAtEOF asserts a read starting at or past size returns
// io.EOF, and a read spanning the end returns the partial bytes plus
// io.EOF, per the io.ReaderAt contract.
func TestRangeReaderAtEOF(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 2000) // 20000 bytes
	srv := rangeServer(t, body)
	defer srv.Close()

	size, _, etag, lm, err := probeRange(context.Background(), srv.Client(), srv.URL, nil)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	ra := newRangeReaderAt(context.Background(), srv.Client(), srv.URL, nil, size, etag, lm)
	defer ra.Close()

	// Read entirely past the end.
	p := make([]byte, 5)
	n, err := ra.ReadAt(p, size)
	if err != io.EOF {
		t.Errorf("ReadAt(size) err = %v, want io.EOF", err)
	}
	if n != 0 {
		t.Errorf("ReadAt(size) n = %d, want 0", n)
	}

	// Read spanning the end: partial bytes plus io.EOF.
	p2 := make([]byte, 10)
	n, err = ra.ReadAt(p2, size-5)
	if err != io.EOF {
		t.Errorf("ReadAt(size-5) err = %v, want io.EOF", err)
	}
	if n != 5 {
		t.Errorf("ReadAt(size-5) n = %d, want 5", n)
	}
	if want := body[size-5:]; !bytes.Equal(p2[:n], want) {
		t.Errorf("ReadAt(size-5) = %q, want %q", p2[:n], want)
	}
}

// TestRangeReaderAtConcurrentReads hammers a single rangeReaderAt from many
// goroutines at scattered offsets, as EFS does when serving TFTP and inst
// concurrently. Run with -race: the point is to catch a data race on the
// chunk cache, not just to check the returned bytes.
func TestRangeReaderAtConcurrentReads(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 2000) // 20000 bytes
	srv := rangeServer(t, body)
	defer srv.Close()

	size, _, etag, lm, err := probeRange(context.Background(), srv.Client(), srv.URL, nil)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	ra := newRangeReaderAt(context.Background(), srv.Client(), srv.URL, nil, size, etag, lm)
	defer ra.Close()

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		off := int64(i * 250 % int(size-8))
		wg.Add(1)
		go func(off int64) {
			defer wg.Done()
			p := make([]byte, 8)
			if _, err := ra.ReadAt(p, off); err != nil && err != io.EOF {
				errs <- err
				return
			}
			if want := body[off : off+8]; !bytes.Equal(p, want) {
				errs <- fmt.Errorf("ReadAt(%d) = %q, want %q", off, p, want)
			}
		}(off)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
