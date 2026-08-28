package source

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
)

// countingReaderAt wraps a *bytes.Reader and tallies every ReadAt call, so a
// test can assert how many times cachingReaderAt reached the underlying reader
// - one call per chunk miss, none on a hit.
type countingReaderAt struct {
	r     *bytes.Reader
	calls atomic.Int64
}

func newCountingReaderAt(b []byte) *countingReaderAt {
	return &countingReaderAt{r: bytes.NewReader(b)}
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.calls.Add(1)
	return c.r.ReadAt(p, off)
}

func (c *countingReaderAt) Size() int64 { return c.r.Size() }

// TestCachingReaderAtMultiChunk reads spans that cross several 8-byte chunk
// boundaries at assorted offsets and lengths, checking the bytes match the
// source exactly - the chunk stitching must reassemble the object faithfully
// whether a read starts mid-chunk, ends mid-chunk, or covers whole chunks.
func TestCachingReaderAtMultiChunk(t *testing.T) {
	const size = 100
	src := make([]byte, size)
	for i := range src {
		src[i] = byte(i)
	}
	c := newCachingReaderAt(newCountingReaderAt(src), 8, 0)

	cases := []struct{ off, n int }{
		{0, 8},   // exactly one chunk
		{0, 20},  // spans three chunks
		{3, 10},  // starts mid-chunk, ends mid-chunk
		{7, 2},   // straddles a single boundary
		{0, 100}, // the whole object
		{95, 5},  // the last five bytes
	}
	for _, tc := range cases {
		p := make([]byte, tc.n)
		got, err := c.ReadAt(p, int64(tc.off))
		if err != nil {
			t.Fatalf("ReadAt(off=%d,n=%d): unexpected err %v", tc.off, tc.n, err)
		}
		if got != tc.n {
			t.Fatalf("ReadAt(off=%d,n=%d): got %d bytes, want %d", tc.off, tc.n, got, tc.n)
		}
		if !bytes.Equal(p, src[tc.off:tc.off+tc.n]) {
			t.Fatalf("ReadAt(off=%d,n=%d): wrong bytes\n got %v\nwant %v", tc.off, tc.n, p, src[tc.off:tc.off+tc.n])
		}
	}
}

// TestCachingReaderAtEOF pins the io.ReaderAt contract cachingReaderAt owes its
// callers: a read starting at or past the end returns 0, io.EOF; a read running
// off the end returns the partial bytes plus io.EOF; and a read that fills p
// exactly returns a nil error even when it lands on the last byte - what efs's
// block reader relies on to accept the final block.
func TestCachingReaderAtEOF(t *testing.T) {
	const size = 20
	src := make([]byte, size)
	for i := range src {
		src[i] = byte('a' + i)
	}
	c := newCachingReaderAt(newCountingReaderAt(src), 8, 0)

	// At the end: no bytes, io.EOF.
	if n, err := c.ReadAt(make([]byte, 4), size); n != 0 || err != io.EOF {
		t.Fatalf("ReadAt at size: got (%d,%v), want (0, EOF)", n, err)
	}
	// Past the end: same.
	if n, err := c.ReadAt(make([]byte, 4), size+5); n != 0 || err != io.EOF {
		t.Fatalf("ReadAt past size: got (%d,%v), want (0, EOF)", n, err)
	}
	// Spanning the end: partial bytes plus io.EOF.
	p := make([]byte, 10)
	n, err := c.ReadAt(p, 15)
	if n != 5 || err != io.EOF {
		t.Fatalf("ReadAt spanning end: got (%d,%v), want (5, EOF)", n, err)
	}
	if !bytes.Equal(p[:n], src[15:]) {
		t.Fatalf("ReadAt spanning end: got %v, want %v", p[:n], src[15:])
	}
	// Ending exactly on the last byte, filling p: nil error, not EOF.
	p = make([]byte, 5)
	if n, err := c.ReadAt(p, 15); n != 5 || err != nil {
		t.Fatalf("ReadAt ending on last byte: got (%d,%v), want (5, nil)", n, err)
	}
}

// TestCachingReaderAtCacheHit proves a chunk is fetched at most once: a second
// read landing in the same chunk is served from the cache without touching the
// underlying reader again, and a read spanning three chunks fetches exactly
// three, then zero on a repeat.
func TestCachingReaderAtCacheHit(t *testing.T) {
	const size = 64
	src := make([]byte, size)
	for i := range src {
		src[i] = byte(i)
	}
	under := newCountingReaderAt(src)
	c := newCachingReaderAt(under, 8, 0)

	// Two reads inside chunk 0: one underlying fetch total.
	if _, err := c.ReadAt(make([]byte, 4), 0); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if _, err := c.ReadAt(make([]byte, 4), 2); err != nil {
		t.Fatalf("second read (same chunk): %v", err)
	}
	if got := under.calls.Load(); got != 1 {
		t.Fatalf("two reads in one chunk fetched %d times, want 1", got)
	}

	// A read spanning chunks 2,3,4 fetches three new chunks.
	if _, err := c.ReadAt(make([]byte, 20), 16); err != nil {
		t.Fatalf("span read: %v", err)
	}
	if got := under.calls.Load(); got != 4 {
		t.Fatalf("after spanning three fresh chunks, total fetches %d, want 4", got)
	}

	// Repeating that read hits the cache: no further fetches.
	if _, err := c.ReadAt(make([]byte, 20), 16); err != nil {
		t.Fatalf("repeat span read: %v", err)
	}
	if got := under.calls.Load(); got != 4 {
		t.Fatalf("repeat of cached span fetched again: total %d, want 4", got)
	}
}

// TestCachingReaderAtEviction proves the cache stays within its byte budget and
// that an evicted chunk is transparently re-fetched. With 8-byte chunks and a
// 16-byte budget only two chunks stay resident: touching a third drops the
// least-recently-used one, and reading it again re-fetches it and returns the
// correct bytes. Throughout, resident bytes never exceed the budget.
func TestCachingReaderAtEviction(t *testing.T) {
	const (
		size   = 64
		chunk  = 8
		budget = 16 // two chunks
	)
	src := make([]byte, size)
	for i := range src {
		src[i] = byte(i)
	}
	under := newCountingReaderAt(src)
	c := newCachingReaderAt(under, chunk, budget)

	// readChunk reads chunk idx whole and checks its bytes against the source.
	readChunk := func(idx int64) {
		t.Helper()
		off := idx * chunk
		p := make([]byte, chunk)
		if _, err := c.ReadAt(p, off); err != nil {
			t.Fatalf("read chunk %d: %v", idx, err)
		}
		if !bytes.Equal(p, src[off:off+chunk]) {
			t.Fatalf("chunk %d: wrong bytes\n got %v\nwant %v", idx, p, src[off:off+chunk])
		}
		c.mu.Lock()
		resident := c.resident
		c.mu.Unlock()
		if resident > budget {
			t.Fatalf("after chunk %d: resident %d exceeds budget %d", idx, resident, budget)
		}
	}
	fetches := func() int64 { return under.calls.Load() }

	readChunk(0)
	readChunk(1)
	if got := fetches(); got != 2 {
		t.Fatalf("after reading chunks 0,1: fetches %d, want 2", got)
	}

	// Re-reading chunk 0 is a hit and makes chunk 1 the least-recently-used.
	readChunk(0)
	if got := fetches(); got != 2 {
		t.Fatalf("re-reading resident chunk 0 fetched again: %d, want 2", got)
	}

	// Chunk 2 pushes resident over budget, evicting the LRU chunk 1.
	readChunk(2)
	if got := fetches(); got != 3 {
		t.Fatalf("reading chunk 2: fetches %d, want 3", got)
	}

	// Chunk 1 was evicted, so touching it again re-fetches - the observable
	// proof that eviction happened - and still returns the right bytes. This
	// insert in turn evicts chunk 0, now the least-recently-used, leaving
	// chunks 1 and 2 resident.
	readChunk(1)
	if got := fetches(); got != 4 {
		t.Fatalf("evicted chunk 1 not re-fetched: fetches %d, want 4", got)
	}

	// Chunk 2 was more recently used than the just-evicted chunk 0, so it
	// survived and is still a hit.
	readChunk(2)
	if got := fetches(); got != 4 {
		t.Fatalf("resident chunk 2 re-fetched: %d, want 4", got)
	}
}

// TestCachingReaderAtConcurrentEviction hammers the reader from many goroutines
// with a budget far smaller than the object, so eviction runs constantly while
// other goroutines are mid-copy out of the very chunks being evicted. It is the
// empirical backstop for the safety argument the single-threaded eviction test
// only reasons about: a chunk removed from the cache stays valid for a reader
// that already holds its slice (the backing array outlives the map entry), and
// stored chunks are never mutated. Run under -race, a torn read surfaces as
// wrong bytes and any unsynchronised access as a race report.
func TestCachingReaderAtConcurrentEviction(t *testing.T) {
	const (
		chunk   = 1024
		nchunks = 256
		size    = chunk * nchunks
		budget  = 8 * chunk // only 8 of 256 chunks resident: relentless churn
	)
	src := make([]byte, size)
	for i := range src {
		src[i] = byte(i)
	}
	under := newCountingReaderAt(src)
	c := newCachingReaderAt(under, chunk, budget)

	const (
		goroutines = 16
		reads      = 400
	)
	var wg sync.WaitGroup
	errc := make(chan error, goroutines)
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			// A deterministic but scattered walk, distinct per goroutine, so
			// reads land across many chunks and keep the 8-chunk cache evicting
			// while peers hold references to chunks in flight.
			off := int64((g * 9973) % size)
			for i := 0; i < reads; i++ {
				n := int64(1 + (i*7+g)%(3*chunk)) // spans up to three chunks
				if off+n > size {
					n = size - off
				}
				p := make([]byte, n)
				got, err := c.ReadAt(p, off)
				if int64(got) != n || err != nil {
					errc <- fmt.Errorf("g%d read off=%d n=%d: got %d err %v", g, off, n, got, err)
					return
				}
				if !bytes.Equal(p, src[off:off+n]) {
					errc <- fmt.Errorf("g%d torn read at off=%d n=%d", g, off, n)
					return
				}
				off = (off + int64(1+(i*131+g*17)%size)) % size
			}
		}(g)
	}
	wg.Wait()
	close(errc)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}

	// Guard that the run actually exercised eviction: with only 8 of 256 chunks
	// resident under a scattered load, re-fetches must push total fetches well
	// past the chunk count. If this ever fails, the cache stopped evicting and
	// the concurrency above proved nothing.
	if got := under.calls.Load(); got <= nchunks {
		t.Fatalf("fetches=%d did not exceed chunk count %d; eviction was not exercised", got, nchunks)
	}
}
