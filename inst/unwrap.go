package inst

import (
	"bufio"
	"io"
	"strings"

	"github.com/jamesbraid/instigator/unixcompress"
	"github.com/jamesbraid/instigator/unixpack"
)

// Unwrap removes installed compression from a file: a name ending in ".z"
// whose data is pack data, or in ".Z" whose data is compress data, is
// decoded and loses the suffix. Otherwise, including a suffix with no
// matching data behind it, the name is kept and the reader yields exactly
// the bytes of r. A decoder error for data with a matching magic is
// returned.
func Unwrap(name string, r io.Reader) (string, io.Reader, error) {
	// The decoders buffer their input at this size, so they reuse br
	// rather than buffering it a second time.
	br := bufio.NewReaderSize(r, 64<<10)
	head, _ := br.Peek(2) // short data simply fails the magic test
	var z io.Reader
	var err error
	switch {
	case strings.HasSuffix(name, ".z") && unixpack.IsPacked(head):
		z, err = unixpack.NewReader(br)
	case strings.HasSuffix(name, ".Z") && unixcompress.IsCompressed(head):
		z, err = unixcompress.NewReader(br)
	default:
		return name, br, nil
	}
	if err != nil {
		return name, nil, err
	}
	return name[:len(name)-2], z, nil
}
