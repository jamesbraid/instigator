package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/internal/logging"
	"github.com/jamesbraid/instigator/internal/vfs"
)

// fastRetry keeps the backoff and stall deadline small enough for tests while
// leaving the shape of the real policy.
var fastRetry = retryPolicy{
	attempts: 4,
	base:     time.Millisecond,
	max:      4 * time.Millisecond,
	budget:   10 * time.Second,
	idle:     150 * time.Millisecond,
}

// flakyServer serves data with Range support, and lets a test break the
// requests that are not the one-byte probe. inject returns true when it has
// answered (or abandoned) the request itself.
type flakyServer struct {
	*httptest.Server
	data    []byte
	etag    atomic.Value // string
	reqs    atomic.Int64
	reads   atomic.Int64 // requests other than the probe
	inject  func(read int64, w http.ResponseWriter, r *http.Request) bool
	content atomic.Value // []byte, swapped by changed-object tests
}

func newFlakyServer(t *testing.T) *flakyServer {
	t.Helper()
	data := make([]byte, 8192)
	for i := range data {
		data[i] = byte(i*7 + i>>8)
	}
	f := &flakyServer{data: data}
	f.etag.Store(`"v1"`)
	f.content.Store(data)
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.reqs.Add(1)
		if r.Header.Get("Range") != "bytes=0-0" && f.inject != nil {
			if f.inject(f.reads.Add(1), w, r) {
				return
			}
		}
		w.Header().Set("ETag", f.etag.Load().(string))
		http.ServeContent(w, r, "x", time.Time{}, bytes.NewReader(f.content.Load().([]byte)))
	}))
	t.Cleanup(f.Close)
	return f
}

// failFirst runs bad on the first n non-probe requests and lets the rest through.
func (f *flakyServer) failFirst(n int64, bad func(w http.ResponseWriter, r *http.Request)) {
	f.inject = func(read int64, w http.ResponseWriter, r *http.Request) bool {
		if read > n {
			return false
		}
		bad(w, r)
		return true
	}
}

func (f *flakyServer) reader(t *testing.T, logOut io.Writer) (*rangeReader, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.URL+"/img", nil)
	if err != nil {
		t.Fatal(err)
	}
	store := newCacheStore(t.TempDir())
	t.Cleanup(func() { store.Close() })
	return newRangeReader(context.Background(), f.Client(), req, store, fastRetry, logging.New(logOut, logging.LevelInfo))
}

func stall(w http.ResponseWriter, r *http.Request) {
	select {
	case <-r.Context().Done():
	case <-time.After(10 * time.Second):
	}
}

func reset(w http.ResponseWriter, r *http.Request) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	conn.(*net.TCPConn).SetLinger(0) // close sends RST, not FIN
	conn.Close()
}

func status(code int, hdr ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i+1 < len(hdr); i += 2 {
			w.Header().Set(hdr[i], hdr[i+1])
		}
		w.WriteHeader(code)
	}
}

// wholeBody is a server that stopped honouring Range: 200 and the whole file.
func (f *flakyServer) wholeBody(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("ETag", f.etag.Load().(string))
	w.Write(f.data)
}

// shortBody promises the range and delivers half of it.
func (f *flakyServer) shortBody(w http.ResponseWriter, r *http.Request) {
	spec := strings.TrimPrefix(r.Header.Get("Range"), "bytes=")
	lo, hi, _ := strings.Cut(spec, "-")
	first, _ := strconv.ParseInt(lo, 10, 64)
	last, _ := strconv.ParseInt(hi, 10, 64)
	w.Header().Set("ETag", f.etag.Load().(string))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(f.data)))
	w.Header().Set("Content-Length", strconv.FormatInt(last-first+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	w.Write(f.data[first : first+(last-first+1)/2])
}

func countLines(s, sub string) int { return strings.Count(s, sub) }

// TestRangeReaderRecoversFromTransientFailures: every kind of transient
// failure on the first two reads is retried, and the third try returns the
// right bytes.
func TestRangeReaderRecoversFromTransientFailures(t *testing.T) {
	var f *flakyServer
	cases := []struct {
		name string
		bad  func(w http.ResponseWriter, r *http.Request)
	}{
		{"stalled past the idle deadline", stall},
		{"connection reset", reset},
		{"503", status(http.StatusServiceUnavailable)},
		{"500", status(http.StatusInternalServerError)},
		{"502", status(http.StatusBadGateway)},
		{"504", status(http.StatusGatewayTimeout)},
		{"429", status(http.StatusTooManyRequests, "Retry-After", "0")},
		{"408", status(http.StatusRequestTimeout)},
		{"200 instead of 206", func(w http.ResponseWriter, r *http.Request) { f.wholeBody(w, r) }},
		{"short body", func(w http.ResponseWriter, r *http.Request) { f.shortBody(w, r) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f = newFlakyServer(t)
			f.failFirst(2, c.bad)
			var log bytes.Buffer
			rr, err := f.reader(t, &log)
			if err != nil {
				t.Fatal(err)
			}
			got := make([]byte, 1000)
			start := time.Now()
			n, err := rr.ReadAt(got, 100)
			if err != nil || n != len(got) || !bytes.Equal(got, f.data[100:1100]) {
				t.Fatalf("ReadAt = %d, %v; bytes equal %v", n, err, bytes.Equal(got, f.data[100:1100]))
			}
			// net/http itself replays an idempotent request once when a
			// reused connection resets before any response, so a reset
			// costs us one logged retry, not two.
			wantRetries := 2
			if c.name == "connection reset" {
				wantRetries = 1
			}
			if reads := f.reads.Load(); reads != 3 {
				t.Errorf("read requests = %d, want 3 (two failures, one success)", reads)
			}
			if lines := countLines(log.String(), "retrying in"); lines != wantRetries {
				t.Errorf("retry log lines = %d, want %d:\n%s", lines, wantRetries, &log)
			}
			if c.name == "stalled past the idle deadline" && time.Since(start) < 2*fastRetry.idle {
				t.Errorf("returned in %v, before two idle deadlines passed", time.Since(start))
			}
		})
	}
}

// TestRangeReaderGivesUp: a server that never recovers fails the read after
// exactly the policy's attempts, and says so.
func TestRangeReaderGivesUp(t *testing.T) {
	f := newFlakyServer(t)
	f.failFirst(1<<30, status(http.StatusServiceUnavailable))
	rr, err := f.reader(t, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rr.ReadAt(make([]byte, 100), 0)
	if err == nil {
		t.Fatal("ReadAt succeeded against a server that always fails")
	}
	if !strings.Contains(err.Error(), "giving up") || !strings.Contains(err.Error(), "503") {
		t.Errorf("error = %v, want give-up naming the 503", err)
	}
	var te *transientError
	if errors.As(err, &te) {
		t.Errorf("transientError leaked to the caller: %v", err)
	}
	if reads := f.reads.Load(); reads != int64(fastRetry.attempts) {
		t.Errorf("read requests = %d, want %d", reads, fastRetry.attempts)
	}
}

// TestRangeReaderGivesUpOnStall: a server that never answers costs the policy's
// attempts times the idle deadline, not forever.
func TestRangeReaderGivesUpOnStall(t *testing.T) {
	f := newFlakyServer(t)
	f.failFirst(1<<30, stall)
	rr, err := f.reader(t, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = rr.ReadAt(make([]byte, 100), 0)
	if err == nil || !strings.Contains(err.Error(), "no data for") {
		t.Fatalf("error = %v, want a no-data timeout", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("took %v", took)
	}
	if reads := f.reads.Load(); reads != int64(fastRetry.attempts) {
		t.Errorf("read requests = %d, want %d", reads, fastRetry.attempts)
	}
}

// TestRangeReaderDoesNotRetryClientErrors: a 4xx other than 408 and 429 is the
// server's final answer.
func TestRangeReaderDoesNotRetryClientErrors(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 410, 416} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			f := newFlakyServer(t)
			f.failFirst(1<<30, status(code))
			var log bytes.Buffer
			rr, err := f.reader(t, &log)
			if err != nil {
				t.Fatal(err)
			}
			_, err = rr.ReadAt(make([]byte, 100), 0)
			if err == nil || !strings.Contains(err.Error(), strconv.Itoa(code)) {
				t.Fatalf("error = %v, want the %d", err, code)
			}
			if reads := f.reads.Load(); reads != 1 {
				t.Errorf("read requests = %d, want 1", reads)
			}
			if log.Len() != 0 {
				t.Errorf("logged a retry: %s", &log)
			}
		})
	}
}

// TestRangeReaderChangedObjectFailsLoudly: a different ETag or size mid-read
// is ErrChanged at once, never retried.
func TestRangeReaderChangedObjectFailsLoudly(t *testing.T) {
	cases := []struct {
		name   string
		change func(f *flakyServer)
	}{
		{"etag", func(f *flakyServer) { f.etag.Store(`"v2"`) }},
		{"size", func(f *flakyServer) { f.content.Store(append(bytes.Clone(f.data), 1, 2, 3)) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFlakyServer(t)
			var log bytes.Buffer
			rr, err := f.reader(t, &log)
			if err != nil {
				t.Fatal(err)
			}
			c.change(f)
			_, err = rr.ReadAt(make([]byte, 100), 0)
			if !errors.Is(err, ErrChanged) {
				t.Fatalf("error = %v, want ErrChanged", err)
			}
			if f.reqs.Load() != 2 {
				t.Errorf("requests = %d, want 2 (probe, one read)", f.reqs.Load())
			}
			if log.Len() != 0 {
				t.Errorf("logged a retry: %s", &log)
			}
		})
	}
}

// TestRangeReaderLogsRetriesWithoutSecrets: each retry is one info line with
// the safe URL, the range, the attempt and the error.
func TestRangeReaderLogsRetriesWithoutSecrets(t *testing.T) {
	f := newFlakyServer(t)
	f.failFirst(1, status(http.StatusServiceUnavailable))
	u, err := url.Parse(f.URL + "/irix/img")
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("user", "hunter2")
	u.RawQuery = "token=sekrit"
	req, _ := http.NewRequest(http.MethodGet, u.String(), nil)
	var log bytes.Buffer
	rr, err := newRangeReader(context.Background(), f.Client(), req, nil, fastRetry, logging.New(&log, logging.LevelInfo))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rr.ReadAt(make([]byte, 100), 200); err != nil {
		t.Fatal(err)
	}
	out := log.String()
	for _, want := range []string{" INFO ", f.URL + "/irix/img", "bytes=200-299", "attempt 1 of 4", "503"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"hunter2", "sekrit"} {
		if strings.Contains(out, secret) {
			t.Errorf("log leaks %q:\n%s", secret, out)
		}
	}
}

// TestRangeReaderRetriesTheProbe: the first request, which decides range
// support and the object's identity, is retried like any other.
func TestRangeReaderRetriesTheProbe(t *testing.T) {
	var n atomic.Int64
	data := bytes.Repeat([]byte("0123456789"), 100)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		http.ServeContent(w, r, "x", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	rr, err := newRangeReader(context.Background(), srv.Client(), req, nil, fastRetry, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Size() != int64(len(data)) || n.Load() != 2 {
		t.Errorf("size=%d requests=%d", rr.Size(), n.Load())
	}
}

// TestRangeReaderRetriesTruncatedWholeFileFallback: a server that ignores
// ranges and cuts its first full-file response short is retried, and the
// buffered copy is complete.
func TestRangeReaderRetriesTruncatedWholeFileFallback(t *testing.T) {
	data := bytes.Repeat([]byte("abcdefghij"), 500)
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if n.Add(1) == 1 {
			w.Write(data[:len(data)/2])
			return
		}
		w.Write(data)
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	store := newCacheStore(t.TempDir())
	defer store.Close()
	rr, err := newRangeReader(context.Background(), srv.Client(), req, store, fastRetry, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Size() != int64(len(data)) {
		t.Fatalf("size = %d, want %d", rr.Size(), len(data))
	}
	got := make([]byte, 50)
	if _, err := rr.ReadAt(got, 4950); err != nil || !bytes.Equal(got, data[4950:5000]) {
		t.Fatalf("ReadAt = %v, %q", err, got)
	}
}

// TestRangeReaderNoRangeWithoutStore: with nowhere to buffer, a 200 to the
// probe is ErrNoRange and is not retried.
func TestRangeReaderNoRangeWithoutStore(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Write([]byte("whole"))
	}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	_, err := newRangeReader(context.Background(), srv.Client(), req, nil, fastRetry, nil)
	if !errors.Is(err, ErrNoRange) || n.Load() != 1 {
		t.Fatalf("err = %v after %d requests, want ErrNoRange after 1", err, n.Load())
	}
}

// TestRangeReaderEOF: reads at and past the end follow the io.ReaderAt
// contract without asking the server for bytes it does not have.
func TestRangeReaderEOF(t *testing.T) {
	f := newFlakyServer(t)
	rr, err := f.reader(t, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(len(f.data))
	buf := make([]byte, 100)
	n, err := rr.ReadAt(buf, size-40)
	if n != 40 || err != io.EOF || !bytes.Equal(buf[:40], f.data[size-40:]) {
		t.Errorf("straddling read = %d, %v", n, err)
	}
	if n, err := rr.ReadAt(buf, size); n != 0 || err != io.EOF {
		t.Errorf("read at end = %d, %v", n, err)
	}
	if n, err := rr.ReadAt(buf[:40], size-40); n != 40 || err != nil {
		t.Errorf("read ending on last byte = %d, %v", n, err)
	}
}

// TestRangeReaderConcurrentRetries: many readers hitting transient failures at
// once all recover (run under -race).
func TestRangeReaderConcurrentRetries(t *testing.T) {
	f := newFlakyServer(t)
	f.inject = func(read int64, w http.ResponseWriter, r *http.Request) bool {
		if read%3 == 0 {
			status(http.StatusServiceUnavailable)(w, r)
			return true
		}
		return false
	}
	rr, err := f.reader(t, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			off := int64(i * 500)
			got := make([]byte, 400)
			if _, err := rr.ReadAt(got, off); err != nil || !bytes.Equal(got, f.data[off:off+400]) {
				t.Errorf("reader %d: %v", i, err)
			}
		}()
	}
	wg.Wait()
}

// TestRangeReaderStopsWhenCallerCancels: the caller's own cancellation is not
// a transient failure.
func TestRangeReaderStopsWhenCallerCancels(t *testing.T) {
	f := newFlakyServer(t)
	f.failFirst(1<<30, stall)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequest(http.MethodGet, f.URL+"/img", nil)
	rr, err := newRangeReader(ctx, f.Client(), req, nil, fastRetry, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := rr.ReadAt(make([]byte, 10), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if reads := f.reads.Load(); reads != 1 {
		t.Errorf("read requests = %d, want 1", reads)
	}
}

// TestResolveRawRetriesFlakyServer: end to end, an EFS image served by a server
// that fails its first two read requests still resolves and reads back, and
// the retries reach the Resolver's logger.
func TestResolveRawRetriesFlakyServer(t *testing.T) {
	raw := efsImage(t, "unix", "UNIX")
	f := newFlakyServer(t)
	f.content.Store(raw)
	f.failFirst(2, status(http.StatusBadGateway))

	var log bytes.Buffer
	r := New(Options{CacheDir: t.TempDir(), Client: f.Client(), Logger: logging.New(&log, logging.LevelInfo)})
	r.retry = fastRetry
	res, err := r.Resolve(f.URL + "/tools.image")
	if err != nil || res.Kind != vfs.OriginImage {
		t.Fatalf("kind=%v err=%v", res.Kind, err)
	}
	defer res.Closer.Close()
	if b, err := fs.ReadFile(res.FS, "unix"); err != nil || string(b) != "UNIX" {
		t.Fatalf("unix=%q err=%v", b, err)
	}
	if countLines(log.String(), "retrying in") != 2 {
		t.Errorf("log:\n%s", &log)
	}
}
