// Package qemunet gives Instigator's services an unprivileged private
// Ethernet segment. It embeds a user-space IPv4 stack (gVisor netstack) and
// attaches it to a QEMU machine over QEMU's stream network backend: each
// Ethernet frame is exchanged over a byte stream (a Unix socket) framed by a
// four-byte big-endian length prefix. The same services that serve the host
// network - bootp, tftp, rsh - serve this segment through the sockets the
// Network hands them, so installation is not a different machine mode.
package qemunet

import (
	"encoding/binary"
	"fmt"
	"io"
)

// writeFrame writes one Ethernet frame using QEMU's stream framing: a
// four-byte big-endian length prefix followed by the frame bytes.
func writeFrame(w io.Writer, frame []byte) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(frame)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(frame) == 0 {
		return nil
	}
	_, err := w.Write(frame)
	return err
}

// readFrame reads one framed Ethernet frame. A length past max is refused
// rather than allocated, so a corrupt or hostile peer cannot drive an
// unbounded allocation. It returns io.EOF only on a clean boundary (before
// the length header); a truncated frame is io.ErrUnexpectedEOF.
func readFrame(r io.Reader, max int) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err // io.EOF here means a clean end of stream
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if int64(n) > int64(max) {
		return nil, fmt.Errorf("qemunet: framed length %d exceeds maximum %d", n, max)
	}
	if n == 0 {
		return []byte{}, nil
	}
	frame := make([]byte, n)
	if _, err := io.ReadFull(r, frame); err != nil {
		return nil, err
	}
	return frame, nil
}
