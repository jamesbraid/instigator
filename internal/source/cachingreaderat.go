package source

import (
	"fmt"
	"io"
	"sync"
)

// cachingReaderAt wraps an io.ReaderAt (an httpreaderat, which owns the range
// GETs, range detection and whole-file fallback) with an aligned in-memory
// chunk cache. ReadAt requests are rounded out to whole chunks so a scattered
// sequence of small reads - as EFS produces while serving TFTP, NFS and inst -
// collapses into a bounded number of underlying reads, and each chunk is
// fetched at most once. It is safe for concurrent use: the served image FS is
// read from many goroutines at once, so ReadAt runs concurrently, and this
// reader is thread-safe by construction rather than by an external lock.
type cachingReaderAt struct {
	r     io.ReaderAt      // underlying, thread-safe range fetch (httpreaderat)
	size  int64            // total object size (httpreaderat.Size())
	chunk int64            // aligned chunk size, e.g. 1<<20
	mu    sync.Mutex       // guards cache
	cache map[int64][]byte // chunk index -> immutable bytes, one copy per chunk
}

// newCachingReaderAt returns a cachingReaderAt over r, whose total length is
// size, caching aligned chunks of chunk bytes.
func newCachingReaderAt(r interface {
	io.ReaderAt
	Size() int64
}, chunk int64) *cachingReaderAt {
	return &cachingReaderAt{
		r:     r,
		size:  r.Size(),
		chunk: chunk,
		cache: map[int64][]byte{},
	}
}

// ReadAt implements io.ReaderAt: it fills p with bytes starting at off,
// fetching and caching whatever aligned chunks the read spans. Per the
// io.ReaderAt contract, a read starting at or past the end returns 0, io.EOF; a
// read that runs off the end returns the bytes it could satisfy plus io.EOF;
// and a read that fills p exactly returns a nil error even when it lands on the
// object's last byte - the same signalling os.File and bytes.Reader use, and
// what efs's fixed-size block reader relies on to accept the final block.
func (c *cachingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("source: cachingReaderAt: negative offset %d", off)
	}
	if off >= c.size {
		return 0, io.EOF
	}

	end := off + int64(len(p))
	if end > c.size {
		end = c.size
	}

	var n int
	for pos := off; pos < end; {
		idx := pos / c.chunk
		data, err := c.chunkAt(idx)
		if err != nil {
			return n, err
		}

		startInChunk := pos - idx*c.chunk
		want := end - pos
		if avail := int64(len(data)) - startInChunk; avail < want {
			want = avail
		}
		copy(p[n:], data[startInChunk:startInChunk+want])
		n += int(want)
		pos += want
	}

	// Signal io.EOF only on a short read - a request that ran past the end of
	// the object, so n < len(p). A read that fills p exactly returns nil even
	// when it ends on the last byte, matching os.File and bytes.Reader.
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// chunkAt returns the cached bytes for chunk index idx, fetching it through
// the underlying reader first if it is not yet cached. The fetch runs outside
// the mutex so a slow underlying read never serializes reads that hit already-
// cached chunks; the map is only touched under the lock. A chunk already
// fetched by another goroutine is reused rather than re-fetched.
func (c *cachingReaderAt) chunkAt(idx int64) ([]byte, error) {
	c.mu.Lock()
	if data, ok := c.cache[idx]; ok {
		c.mu.Unlock()
		return data, nil
	}
	c.mu.Unlock()

	data, err := c.fetch(idx)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	// First writer wins: a racing duplicate fetch is discarded, so every
	// caller observes identical bytes and the cache holds one copy per chunk.
	// A stored chunk is never mutated afterwards, so concurrent readers copy
	// out of it safely.
	if existing, ok := c.cache[idx]; ok {
		c.mu.Unlock()
		return existing, nil
	}
	c.cache[idx] = data
	c.mu.Unlock()
	return data, nil
}

// fetch reads chunk index idx from the underlying reader, clamping the last
// chunk to the object size so the request never runs past the end. The result
// is exactly the clamped chunk length; the underlying reader's contract
// returns a non-nil error whenever it hands back fewer bytes, so a short read
// is a hard error rather than a silently truncated chunk.
func (c *cachingReaderAt) fetch(idx int64) ([]byte, error) {
	lo := idx * c.chunk
	length := c.chunk
	if lo+length > c.size {
		length = c.size - lo
	}
	buf := make([]byte, length)
	got, err := c.r.ReadAt(buf, lo)
	if int64(got) != length {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("source: cachingReaderAt: short read of chunk %d: %w", idx, err)
	}
	// A read that ends exactly on the last byte may report io.EOF alongside a
	// full buffer; that is a complete chunk, not an error.
	return buf, nil
}
