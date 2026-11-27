package inst

import (
	"fmt"
	"io"
	"strconv"
)

// ChecksumError is returned in place of io.EOF by a record's reader when its
// bytes do not match the manifest's sum(). The bytes were all delivered.
type ChecksumError struct {
	Path      string // the entry's install path
	Want, Got uint16 // the manifest's sum(), and the sum of the bytes read
}

func (e *ChecksumError) Error() string {
	return fmt.Sprintf("inst: %s: checksum mismatch: bytes sum to %d, manifest says %d", e.Path, e.Got, e.Want)
}

// sumBytes adds p to the BSD 16-bit checksum c, as sum -r computes it.
func sumBytes(c uint16, p []byte) uint16 {
	for _, b := range p {
		c = c>>1 | c<<15
		c += uint16(b)
	}
	return c
}

// Sum returns the checksum the entry's sum() states, and whether it states
// one that is a decimal number below 65536.
func (e Entry) Sum() (uint16, bool) {
	n, err := strconv.ParseUint(e.Attrs["sum"], 10, 16)
	if err != nil {
		return 0, false
	}
	return uint16(n), true
}

// sumReader passes a record's bytes through and checks them at EOF.
type sumReader struct {
	io.ReadCloser
	path string
	want uint16
	got  uint16
}

func (s *sumReader) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	s.got = sumBytes(s.got, p[:n])
	if err == io.EOF && s.got != s.want {
		err = &ChecksumError{Path: s.path, Want: s.want, Got: s.got}
	}
	return n, err
}
