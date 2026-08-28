package source

import (
	"bytes"
	"io"
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
	c := newCachingReaderAt(newCountingReaderAt(src), 8)

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
	c := newCachingReaderAt(newCountingReaderAt(src), 8)

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
	c := newCachingReaderAt(under, 8)

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
