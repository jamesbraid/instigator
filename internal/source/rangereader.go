package source

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// rangeChunkSize is the aligned unit a rangeReaderAt fetches and caches.
// ReadAt requests are rounded out to whole chunks so a scattered sequence
// of small reads (as EFS produces while serving TFTP/inst) still collapses
// into a bounded number of range requests against the remote object.
const rangeChunkSize = 4 << 20 // 4 MiB

// probeRange issues a GET Range: bytes=0-0 against url to determine
// whether the server honours byte ranges and, either way, the object's
// total size — without downloading the object. A 206 response means
// ranges are supported; its Content-Range header (bytes 0-0/<total>)
// carries the total. A 200 response means the server ignored the Range
// request and returned the whole object; ranges is false and the total
// comes from Content-Length instead. Either way the body is closed
// without being read. etag and lastModified are captured from the
// response so the caller can pin later range requests to this exact
// object version.
func probeRange(ctx context.Context, client *http.Client, rawURL string, creds Credentials) (size int64, ranges bool, etag, lastModified string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, false, "", "", fmt.Errorf("source: probe %s: build request: %w", safeURL(rawURL), unwrapURLErr(err))
	}
	creds.apply(req)
	req.Header.Set("Range", "bytes=0-0")

	resp, err := client.Do(req)
	if err != nil {
		return 0, false, "", "", fmt.Errorf("source: probe %s: %w", safeURL(rawURL), unwrapURLErr(err))
	}
	defer resp.Body.Close()

	etag = resp.Header.Get("ETag")
	lastModified = resp.Header.Get("Last-Modified")

	switch resp.StatusCode {
	case http.StatusPartialContent:
		total, ok := parseContentRangeTotal(resp.Header.Get("Content-Range"))
		if !ok {
			return 0, false, "", "", fmt.Errorf("source: probe %s: malformed Content-Range %q", safeURL(rawURL), resp.Header.Get("Content-Range"))
		}
		return total, true, etag, lastModified, nil
	case http.StatusOK:
		return resp.ContentLength, false, etag, lastModified, nil
	default:
		return 0, false, "", "", fmt.Errorf("source: probe %s: unexpected status %s", safeURL(rawURL), resp.Status)
	}
}

// parseContentRangeTotal extracts the total from a Content-Range header of
// the form "bytes 0-0/<total>". It returns ok=false if the header is not
// in that shape (a "*" total, for instance, leaves the size unknown).
func parseContentRangeTotal(cr string) (total int64, ok bool) {
	i := strings.LastIndexByte(cr, '/')
	if i < 0 || i+1 >= len(cr) {
		return 0, false
	}
	n, err := strconv.ParseInt(cr[i+1:], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// rangeReaderAt is an io.ReaderAt over an http(s) object, satisfying each
// read with one or more ranged GETs, aligned to rangeChunkSize and cached
// in memory so a chunk is fetched at most once. It is safe for concurrent
// use: EFS serves the tree (TFTP and inst) to more than one client at a
// time, so ReadAt may be called from multiple goroutines simultaneously.
type rangeReaderAt struct {
	ctx    context.Context
	client *http.Client
	url    string
	creds  Credentials

	size         int64
	etag         string
	lastModified string

	mu     sync.Mutex
	chunks map[int64][]byte // keyed by chunk index (offset / rangeChunkSize)
}

// newRangeReaderAt returns a rangeReaderAt for url, which must support
// byte-range requests (see probeRange). size is the object's total length;
// etag and lastModified pin every subsequent range request to this exact
// object version, so the read stays consistent even if the object is
// later overwritten.
func newRangeReaderAt(ctx context.Context, client *http.Client, url string, creds Credentials, size int64, etag, lastModified string) *rangeReaderAt {
	return &rangeReaderAt{
		ctx:          ctx,
		client:       client,
		url:          url,
		creds:        creds,
		size:         size,
		etag:         etag,
		lastModified: lastModified,
		chunks:       make(map[int64][]byte),
	}
}

// Close is a no-op; rangeReaderAt holds no resources beyond its in-memory
// chunk cache, which the garbage collector reclaims. It exists so
// rangeReaderAt satisfies io.Closer, letting callers defer Close
// uniformly across source types.
func (r *rangeReaderAt) Close() error {
	return nil
}

// ReadAt implements io.ReaderAt: it fills p with bytes starting at off,
// fetching and caching whatever aligned chunks the read spans. Per the
// io.ReaderAt contract, a read that runs off the end of the object returns
// the bytes it could satisfy along with io.EOF; a read starting at or past
// the end returns 0, io.EOF. A read that fills p exactly returns a nil
// error even when it lands on the object's last byte - the same signalling
// os.File and bytes.Reader use, and what a consumer that treats any non-nil
// error as fatal (efs's fixed-size block reader does) relies on to accept
// the final block.
func (r *rangeReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("source: rangeReaderAt: negative offset %d", off)
	}
	if off >= r.size {
		return 0, io.EOF
	}

	end := off + int64(len(p))
	if end > r.size {
		end = r.size
	}

	var n int
	for pos := off; pos < end; {
		chunkIdx := pos / rangeChunkSize
		chunk, err := r.chunk(chunkIdx)
		if err != nil {
			return n, err
		}

		chunkStart := chunkIdx * rangeChunkSize
		startInChunk := pos - chunkStart
		avail := int64(len(chunk)) - startInChunk
		want := end - pos
		if avail < want {
			want = avail
		}
		if want <= 0 {
			// fetchRange asserts every cached chunk is exactly
			// hi-lo+1 bytes, so a cached chunk should always cover
			// through wherever it was fetched to; this means the
			// chunk on record is shorter than its own index implies.
			// The io.ReaderAt contract forbids returning n < len(p)
			// with a nil error, so this is a hard error rather than
			// a silent short read.
			return n, fmt.Errorf("source: rangeReaderAt: chunk %d shorter than expected (have %d bytes, need offset %d)", chunkIdx, len(chunk), startInChunk)
		}

		copy(p[n:], chunk[startInChunk:startInChunk+want])
		n += int(want)
		pos += want
	}

	// Signal io.EOF only on a short read - a request that ran past the end
	// of the object, so n < len(p). A read that fills p exactly returns nil
	// even when it ends on the last byte, matching os.File and bytes.Reader.
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// chunk returns the cached bytes for chunk index idx, fetching it over the
// network first if it is not yet cached. The cache is guarded by r.mu so
// concurrent ReadAt calls never race on the map, and a chunk already
// fetched by one caller is reused by another rather than re-fetched.
func (r *rangeReaderAt) chunk(idx int64) ([]byte, error) {
	r.mu.Lock()
	if c, ok := r.chunks[idx]; ok {
		r.mu.Unlock()
		return c, nil
	}
	r.mu.Unlock()

	lo := idx * rangeChunkSize
	hi := lo + rangeChunkSize - 1
	if hi > r.size-1 {
		hi = r.size - 1
	}

	data, err := r.fetchRange(lo, hi)
	if err != nil {
		return nil, err
	}

	r.mu.Lock()
	// Another goroutine may have fetched the same chunk concurrently;
	// keep whichever landed first so both callers observe identical
	// bytes and the cache holds one copy per chunk.
	if c, ok := r.chunks[idx]; ok {
		r.mu.Unlock()
		return c, nil
	}
	r.chunks[idx] = data
	r.mu.Unlock()
	return data, nil
}

// fetchRange issues a single ranged GET for bytes [lo, hi] (inclusive),
// pinned to the object version captured at probe time so a mid-install
// change to the object is caught rather than silently mixed into the
// read.
func (r *rangeReaderAt) fetchRange(lo, hi int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, fmt.Errorf("source: range %s: build request: %w", safeURL(r.url), unwrapURLErr(err))
	}
	r.creds.apply(req)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", lo, hi))
	r.applyConsistencyPin(req)

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("source: range %s: %w", safeURL(r.url), unwrapURLErr(err))
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		// expected: the server honoured the Range request.
	case http.StatusPreconditionFailed:
		return nil, fmt.Errorf("source: range %s: object changed during read (412 precondition failed)", safeURL(r.url))
	case http.StatusRequestedRangeNotSatisfiable:
		return nil, fmt.Errorf("source: range %s: range not satisfiable (416)", safeURL(r.url))
	default:
		// This also catches a bare 200: probeRange already established
		// that this server honours ranges, so a 200 here means it (or
		// whatever's in front of it) started ignoring Range mid-read. A
		// 200 to a ranged GET returns the WHOLE object starting at
		// offset 0, not the [lo,hi] span this chunk's cache slot
		// assumes — silently accepting it would cache wrong-offset
		// bytes under this chunk's index and serve them as if they came
		// from lo. Hard-fail instead of risking that.
		return nil, fmt.Errorf("source: range %s: unexpected status %s", safeURL(r.url), resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("source: range %s: read body: %w", safeURL(r.url), err)
	}

	// Defense in depth against a 206 that claims success but returns the
	// wrong span: a short or overlong body would otherwise be cached and
	// silently misindexed by ReadAt, the same corruption a bare 200
	// causes.
	if want := hi - lo + 1; int64(len(data)) != want {
		return nil, fmt.Errorf("source: range %s: chunk length mismatch: got %d bytes, want %d (range bytes=%d-%d)", safeURL(r.url), len(data), want, lo, hi)
	}

	return data, nil
}

// applyConsistencyPin attaches the precondition that keeps every range
// request scoped to the exact object version probeRange observed: If-Match
// on the etag when one was captured, else If-Unmodified-Since on the
// last-modified timestamp when that was captured instead, else no pin at
// all (the server offered neither).
func (r *rangeReaderAt) applyConsistencyPin(req *http.Request) {
	switch {
	case r.etag != "":
		req.Header.Set("If-Match", r.etag)
	case r.lastModified != "":
		req.Header.Set("If-Unmodified-Since", r.lastModified)
	}
}
