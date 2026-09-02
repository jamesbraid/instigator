package source

import (
	"container/list"
	"fmt"
	"io"
	"sync"
)

// cachingReaderAt wraps a range-fetching io.ReaderAt (httpreaderat) with an
// in-memory chunk cache: scattered reads coalesce into aligned chunks, each
// fetched at most once and evicted least-recently-used past the byte budget
// (<= 0 disables eviction). Safe for concurrent use - the served FS is read
// from many goroutines at once.
type cachingReaderAt struct {
	r      io.ReaderAt // underlying, thread-safe range fetch (httpreaderat)
	size   int64       // total object size (httpreaderat.Size())
	chunk  int64       // aligned chunk size, e.g. 1<<20
	budget int64       // max resident chunk bytes; <= 0 caches without eviction

	mu       sync.Mutex
	cache    map[int64]*list.Element // chunk index -> element holding a *chunkEntry
	lru      *list.List              // front = most recently used, back = eviction target
	resident int64                   // sum of len(entry.data) held in the cache
}

// chunkEntry is one cached chunk: its index and its immutable bytes. It is the
// Value of a list.Element in the LRU list; the map indexes those elements by
// chunk index so a hit is O(1) and can promote the element to the front.
type chunkEntry struct {
	idx  int64
	data []byte
}

// newCachingReaderAt returns a cachingReaderAt over r, whose total length is
// size, caching aligned chunks of chunk bytes and holding at most budget bytes
// of them resident. A budget of zero or less disables eviction and caches
// every touched chunk. A positive budget below one chunk still keeps the chunk
// currently being served, so it degrades to caching a single chunk rather than
// failing.
func newCachingReaderAt(r interface {
	io.ReaderAt
	Size() int64
}, chunk, budget int64) *cachingReaderAt {
	return &cachingReaderAt{
		r:      r,
		size:   r.Size(),
		chunk:  chunk,
		budget: budget,
		cache:  map[int64]*list.Element{},
		lru:    list.New(),
	}
}

// ReadAt fills p from off, fetching the aligned chunks the read spans. It
// follows the io.ReaderAt EOF contract exactly - a full read ending on the last
// byte returns nil, not io.EOF - which efs's fixed-size block reader depends on.
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

	// EOF only on a short read (n < len(p)); a full read returns nil even on the
	// last byte, matching os.File and bytes.Reader.
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// chunkAt returns chunk idx, fetching a miss outside the lock so a slow fetch
// never blocks cache hits. A duplicate racing fetch is discarded; every access
// promotes the chunk to most-recently-used.
func (c *cachingReaderAt) chunkAt(idx int64) ([]byte, error) {
	c.mu.Lock()
	if el, ok := c.cache[idx]; ok {
		c.lru.MoveToFront(el)
		data := el.Value.(*chunkEntry).data
		c.mu.Unlock()
		return data, nil
	}
	c.mu.Unlock()

	data, err := c.fetch(idx)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// First writer wins: a racing duplicate is discarded. Stored chunks are
	// immutable, so a concurrent reader - even one whose chunk is evicted
	// mid-copy - safely holds its own reference to the backing array.
	if el, ok := c.cache[idx]; ok {
		c.lru.MoveToFront(el)
		return el.Value.(*chunkEntry).data, nil
	}
	el := c.lru.PushFront(&chunkEntry{idx: idx, data: data})
	c.cache[idx] = el
	c.resident += int64(len(data))
	c.evictLocked()
	return data, nil
}

// evictLocked drops least-recently-used chunks from the back of the list until
// the resident bytes fit the budget, always keeping at least the chunk just
// inserted at the front. It is a no-op when the budget is unset. Callers hold
// c.mu.
func (c *cachingReaderAt) evictLocked() {
	if c.budget <= 0 {
		return
	}
	for c.resident > c.budget && c.lru.Len() > 1 {
		back := c.lru.Back()
		e := back.Value.(*chunkEntry)
		c.lru.Remove(back)
		delete(c.cache, e.idx)
		c.resident -= int64(len(e.data))
	}
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
