package nfs

import "encoding/binary"

// xdrReader decodes XDR-encoded values (RFC 4506) from a request body.
// A short or malformed body sets err, which the dispatcher turns into
// GARBAGE_ARGS; the values read after that are zero.
type xdrReader struct {
	b   []byte
	off int
	err bool
}

func (r *xdrReader) uint32() uint32 {
	if r.off+4 > len(r.b) {
		r.err = true
		return 0
	}
	v := binary.BigEndian.Uint32(r.b[r.off:])
	r.off += 4
	return v
}

func (r *xdrReader) uint64() uint64 {
	hi := r.uint32()
	return uint64(hi)<<32 | uint64(r.uint32())
}

func (r *xdrReader) bool() bool { return r.uint32() != 0 }

// bytes reads a fixed-length opaque of exactly n bytes (no length prefix,
// no padding), used for the 32-byte NFSv2 filehandle.
func (r *xdrReader) bytes(n int) []byte {
	if r.off+n > len(r.b) {
		r.err = true
		return nil
	}
	v := r.b[r.off : r.off+n]
	r.off += n
	return v
}

// opaque reads a length-prefixed, 4-byte-padded opaque of at most max
// bytes.
func (r *xdrReader) opaque(max int) []byte {
	n := r.uint32()
	if r.err || n > uint32(max) || r.off+int(n) > len(r.b) {
		r.err = true
		return nil
	}
	v := r.b[r.off : r.off+int(n)]
	r.off += (int(n) + 3) &^ 3
	if r.off > len(r.b) {
		r.err = true
	}
	return v
}

// str reads a length-prefixed, 4-byte-padded string.
func (r *xdrReader) str() string {
	return string(r.opaque(len(r.b)))
}

// xdrWriter encodes XDR-encoded values into a reply body.
type xdrWriter struct{ b []byte }

func (w *xdrWriter) uint32(v uint32) { w.b = binary.BigEndian.AppendUint32(w.b, v) }

func (w *xdrWriter) uint64(v uint64) { w.b = binary.BigEndian.AppendUint64(w.b, v) }

func (w *xdrWriter) bool(v bool) {
	if v {
		w.uint32(1)
	} else {
		w.uint32(0)
	}
}

// fixed appends raw bytes with no length prefix or padding.
func (w *xdrWriter) fixed(p []byte) { w.b = append(w.b, p...) }

// opaque appends a length-prefixed, 4-byte-padded byte slice.
func (w *xdrWriter) opaque(p []byte) {
	w.uint32(uint32(len(p)))
	w.b = append(w.b, p...)
	for len(w.b)%4 != 0 {
		w.b = append(w.b, 0)
	}
}
