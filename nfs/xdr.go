package nfs

import "encoding/binary"

// xdrReader decodes XDR-encoded values from a request body.
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

// str reads a length-prefixed, 4-byte-padded string.
func (r *xdrReader) str() string {
	n := int(r.uint32())
	if n < 0 || r.off+n > len(r.b) {
		r.err = true
		return ""
	}
	s := string(r.b[r.off : r.off+n])
	r.off += (n + 3) &^ 3
	return s
}

// xdrWriter encodes XDR-encoded values into a reply body.
type xdrWriter struct{ b []byte }

func (w *xdrWriter) uint32(v uint32) {
	var t [4]byte
	binary.BigEndian.PutUint32(t[:], v)
	w.b = append(w.b, t[:]...)
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
