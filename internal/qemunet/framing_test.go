package qemunet

import (
	"bytes"
	"io"
	"testing"
)

func TestFramingRoundTrip(t *testing.T) {
	frames := [][]byte{
		[]byte("first frame"),
		{}, // an empty frame is a valid length-0 record
		bytes.Repeat([]byte{0xab}, 1514),
	}
	var buf bytes.Buffer
	for _, f := range frames {
		if err := writeFrame(&buf, f); err != nil {
			t.Fatalf("writeFrame: %v", err)
		}
	}
	r := bytes.NewReader(buf.Bytes())
	for i, want := range frames {
		got, err := readFrame(r, 65535)
		if err != nil {
			t.Fatalf("readFrame %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d = %x, want %x", i, got, want)
		}
	}
	if _, err := readFrame(r, 65535); err != io.EOF {
		t.Fatalf("after last frame: err = %v, want EOF", err)
	}
}

func TestReadFrameRejectsOversizeLength(t *testing.T) {
	// A 4-byte length header claiming 100000 bytes must be refused rather
	// than allocated, so a hostile or corrupt peer cannot exhaust memory.
	hdr := []byte{0x00, 0x01, 0x86, 0xa0} // 100000
	if _, err := readFrame(bytes.NewReader(hdr), 65535); err == nil {
		t.Fatal("readFrame accepted an oversize length; want an error")
	}
}
