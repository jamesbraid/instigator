package nfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func fragment(last bool, p []byte) []byte {
	h := uint32(len(p))
	if last {
		h |= 0x80000000
	}
	return append(binary.BigEndian.AppendUint32(nil, h), p...)
}

// A record may arrive in several fragments; only the last carries the
// top bit (RFC 5531 section 11).
func TestReadRecordJoinsFragments(t *testing.T) {
	var in bytes.Buffer
	in.Write(fragment(false, []byte("abcd")))
	in.Write(fragment(false, nil))
	in.Write(fragment(true, []byte("ef")))
	in.Write(fragment(true, []byte("next")))
	rec, err := readRecord(&in)
	if err != nil || string(rec) != "abcdef" {
		t.Fatalf("first record = %q, %v", rec, err)
	}
	rec, err = readRecord(&in)
	if err != nil || string(rec) != "next" {
		t.Fatalf("second record = %q, %v", rec, err)
	}
	if _, err := readRecord(&in); err != io.EOF {
		t.Fatalf("at end: %v, want io.EOF", err)
	}
}

func TestReadRecordErrors(t *testing.T) {
	big := binary.BigEndian.AppendUint32(nil, 0x80000000|(maxRecord+1))
	if _, err := readRecord(bytes.NewReader(big)); !errors.Is(err, errRecordTooLarge) {
		t.Fatalf("oversized record: %v", err)
	}
	cut := fragment(true, []byte("abcd"))[:6]
	if _, err := readRecord(bytes.NewReader(cut)); err != io.ErrUnexpectedEOF {
		t.Fatalf("truncated fragment: %v", err)
	}
	open := fragment(false, []byte("abcd"))
	if _, err := readRecord(bytes.NewReader(open)); err != io.ErrUnexpectedEOF {
		t.Fatalf("missing last fragment: %v", err)
	}
}
