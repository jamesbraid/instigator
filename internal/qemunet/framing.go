// Package qemunet gives Instigator's services an unprivileged private
// Ethernet segment. It embeds a user-space IPv4 stack (gVisor netstack) and
// attaches it to a QEMU machine over QEMU's stream network backend: each
// Ethernet frame is exchanged over a byte stream framed by a
// four-byte big-endian length prefix. The same services that serve the host
// network - bootp, tftp, rsh - serve this segment through the sockets the
// Network hands them, so installation is not a different machine mode.
package qemunet

import (
	"encoding/binary"
	"fmt"
	"io"
)

// frameWriter owns its scratch space for the lifetime of one stream pump.
// Combining the prefix and payload avoids a socket write for every prefix.
type frameWriter struct {
	w   io.Writer
	buf []byte
}

func (w *frameWriter) write(frame []byte) error {
	w.append(frame)
	return w.flush()
}

func (w *frameWriter) append(frame []byte) {
	start := len(w.buf)
	w.buf = append(w.buf, 0, 0, 0, 0)
	binary.BigEndian.PutUint32(w.buf[start:], uint32(len(frame)))
	w.buf = append(w.buf, frame...)
}

func (w *frameWriter) flush() error {
	err := writeFull(w.w, w.buf)
	w.buf = w.buf[:0]
	return err
}

func writeFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

type frameReader struct {
	r   io.Reader
	hdr [4]byte
	buf []byte
}

// read reads one framed Ethernet frame, valid until the next call. A length
// past max is refused rather than allocated, so a corrupt peer cannot drive an
// unbounded allocation. It returns io.EOF only on a clean boundary (before
// the length header); a truncated frame is io.ErrUnexpectedEOF.
func (r *frameReader) read(max int) ([]byte, error) {
	if _, err := io.ReadFull(r.r, r.hdr[:]); err != nil {
		return nil, err // io.EOF here means a clean end of stream
	}
	n := binary.BigEndian.Uint32(r.hdr[:])
	if int64(n) > int64(max) {
		return nil, fmt.Errorf("qemunet: framed length %d exceeds maximum %d", n, max)
	}
	if cap(r.buf) < int(n) {
		r.buf = make([]byte, n)
	}
	r.buf = r.buf[:n]
	if _, err := io.ReadFull(r.r, r.buf); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return r.buf, nil
}
