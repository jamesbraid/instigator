package source

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
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

// TestRangeReaderAtChunkFetchHardErrors covers the three ways a chunk GET
// can go wrong once probeRange has already established the server is
// range-capable: the pinned object changed underneath the read (412), the
// requested range became impossible (416), and the server reverts to
// ignoring Range entirely (200) — which, if silently accepted, would cache
// bytes from the wrong absolute offset (a 200 response starts at offset 0,
// not at the chunk's lo) under this chunk's index and serve corrupted data
// with a nil error. All three must surface as a real error naming the
// object, never a corrupted or short read.
//
// The URL carries embedded userinfo so the assertions also confirm the
// error text never leaks the credential — every error in this path is
// built through safeURL, matching fetch.go's fetchWhole errors.
func TestRangeReaderAtChunkFetchHardErrors(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 2000) // 20000 bytes

	cases := []struct {
		name       string
		chunkResp  func(w http.ResponseWriter, r *http.Request)
		wantSubstr string
	}{
		{
			name: "412 precondition failed (object changed)",
			chunkResp: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusPreconditionFailed)
			},
			wantSubstr: "412",
		},
		{
			name: "416 range not satisfiable",
			chunkResp: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			},
			wantSubstr: "416",
		},
		{
			name: "200 ignores Range after probe said it was capable",
			chunkResp: func(w http.ResponseWriter, r *http.Request) {
				// A server (or something in front of it) that stops
				// honouring Range returns the WHOLE object starting at
				// offset 0. Accepting this used to get cached as if it
				// were the requested [lo,hi] span — silent corruption.
				w.WriteHeader(http.StatusOK)
				w.Write(body)
			},
			wantSubstr: "200",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reqNum int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// The first request is probeRange's own bytes=0-0
				// probe; let it succeed normally so the reader
				// believes the server is range-capable. Every request
				// after that is the "real" chunk fetch this test is
				// targeting.
				if atomic.AddInt32(&reqNum, 1) == 1 {
					w.Header().Set("Accept-Ranges", "bytes")
					http.ServeContent(w, r, "img", time.Unix(0, 0), bytes.NewReader(body))
					return
				}
				tc.chunkResp(w, r)
			}))
			defer srv.Close()

			u, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatalf("parse server URL: %v", err)
			}
			u.User = url.UserPassword("probe-user", "super-secret-password")
			target := u.String()

			size, ranges, etag, lm, err := probeRange(context.Background(), srv.Client(), target, nil)
			if err != nil || !ranges {
				t.Fatalf("probe: size=%d ranges=%v err=%v", size, ranges, err)
			}
			ra := newRangeReaderAt(context.Background(), srv.Client(), target, nil, size, etag, lm)
			defer ra.Close()

			// Offset 12003 forces a real chunk fetch (it's not the
			// bytes=0-0 the probe already satisfied).
			p := make([]byte, 5)
			n, err := ra.ReadAt(p, 12003)
			if err == nil {
				t.Fatalf("ReadAt: want error, got n=%d err=nil", n)
			}
			if n != 0 {
				t.Errorf("ReadAt: want n=0 alongside the error, got %d", n)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("ReadAt error = %q, want it to mention %q", err.Error(), tc.wantSubstr)
			}
			if strings.Contains(err.Error(), "super-secret-password") || strings.Contains(err.Error(), "probe-user") {
				t.Errorf("ReadAt error leaks the URL's credential: %q", err.Error())
			}
		})
	}
}

// TestRangeReaderAtChunkLengthMismatch asserts that a 206 response whose
// body doesn't match the requested [lo,hi] span (the server claims success
// but returns the wrong number of bytes) is a hard error rather than a
// cached, wrongly-indexed chunk.
func TestRangeReaderAtChunkLengthMismatch(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 2000) // 20000 bytes

	var reqNum int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&reqNum, 1) == 1 {
			w.Header().Set("Accept-Ranges", "bytes")
			http.ServeContent(w, r, "img", time.Unix(0, 0), bytes.NewReader(body))
			return
		}
		// Claim 206 but hand back fewer bytes than the Range asked for.
		w.Header().Set("Content-Range", "bytes 12000-16095/20000")
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body[12000:15000])
	}))
	defer srv.Close()

	size, ranges, etag, lm, err := probeRange(context.Background(), srv.Client(), srv.URL, nil)
	if err != nil || !ranges {
		t.Fatalf("probe: size=%d ranges=%v err=%v", size, ranges, err)
	}
	ra := newRangeReaderAt(context.Background(), srv.Client(), srv.URL, nil, size, etag, lm)
	defer ra.Close()

	p := make([]byte, 5)
	n, err := ra.ReadAt(p, 12003)
	if err == nil {
		t.Fatalf("ReadAt: want error, got n=%d err=nil", n)
	}
	if n != 0 {
		t.Errorf("ReadAt: want n=0 alongside the error, got %d", n)
	}
	if !strings.Contains(err.Error(), "length mismatch") {
		t.Errorf("ReadAt error = %q, want it to mention a length mismatch", err.Error())
	}
}
