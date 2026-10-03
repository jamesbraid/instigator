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
		bytes.Repeat([]byte{0xcd}, maxFrame),
		[]byte("short final frame"),
	}
	var buf bytes.Buffer
	w := frameWriter{w: &buf}
	for _, f := range frames {
		if err := w.write(f); err != nil {
			t.Fatalf("writeFrame: %v", err)
		}
	}
	r := frameReader{r: bytes.NewReader(buf.Bytes())}
	for i, want := range frames {
		got, err := r.read(65535)
		if err != nil {
			t.Fatalf("readFrame %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d = %x, want %x", i, got, want)
		}
	}
	if _, err := r.read(65535); err != io.EOF {
		t.Fatalf("after last frame: err = %v, want EOF", err)
	}
}

type frameCountingWriter struct {
	bytes.Buffer
	calls int
}

func (w *frameCountingWriter) Write(p []byte) (int, error) {
	w.calls++
	return w.Buffer.Write(p)
}

func TestWriteFrameUsesOneWrite(t *testing.T) {
	var w frameCountingWriter
	if err := (&frameWriter{w: &w}).write([]byte("frame")); err != nil {
		t.Fatal(err)
	}
	if w.calls != 1 {
		t.Fatalf("frame required %d stream writes, want 1", w.calls)
	}
	if !bytes.Equal(w.Bytes(), []byte{0, 0, 0, 5, 'f', 'r', 'a', 'm', 'e'}) {
		t.Fatalf("framed bytes = %x", w.Bytes())
	}
}

type failingFrameWriter struct{ bytes.Buffer }

func (w *failingFrameWriter) Write(p []byte) (int, error) {
	if w.Len() >= 6 {
		return 0, io.ErrClosedPipe
	}
	return w.Buffer.Write(p[:min(len(p), 6-w.Len())])
}

func TestWriteFrameReturnsPartialFailure(t *testing.T) {
	var w failingFrameWriter
	if err := (&frameWriter{w: &w}).write([]byte("frame")); err != io.ErrClosedPipe {
		t.Fatalf("writeFrame = %v, want %v", err, io.ErrClosedPipe)
	}
	if !bytes.Equal(w.Bytes(), []byte{0, 0, 0, 5, 'f', 'r'}) {
		t.Fatalf("partial framed bytes = %x", w.Bytes())
	}
}

func TestReadFrameAllocations(t *testing.T) {
	data := []byte{0, 0, 0, 5, 'f', 'r', 'a', 'm', 'e'}
	r := bytes.NewReader(data)
	reader := frameReader{r: r}
	allocs := testing.AllocsPerRun(100, func() {
		r.Reset(data)
		got, err := reader.read(maxFrame)
		if err != nil || !bytes.Equal(got, []byte("frame")) {
			t.Fatalf("readFrame = %q, %v", got, err)
		}
	})
	if allocs != 0 {
		t.Fatalf("%.0f allocations per frame after warmup, want 0", allocs)
	}
}

func TestReadFrameTruncation(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{"boundary", nil, io.EOF},
		{"header", []byte{0, 0}, io.ErrUnexpectedEOF},
		{"missing body", []byte{0, 0, 0, 5}, io.ErrUnexpectedEOF},
		{"body", []byte{0, 0, 0, 5, 'f', 'r'}, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := frameReader{r: bytes.NewReader(tc.data)}
			if _, err := r.read(maxFrame); err != tc.want {
				t.Fatalf("read error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReadFrameRejectsOversizeLength(t *testing.T) {
	// A 4-byte length header claiming 100000 bytes must be refused rather
	// than allocated, so a hostile or corrupt peer cannot exhaust memory.
	hdr := []byte{0x00, 0x01, 0x86, 0xa0} // 100000
	if _, err := (&frameReader{r: bytes.NewReader(hdr)}).read(65535); err == nil {
		t.Fatal("readFrame accepted an oversize length; want an error")
	}
}

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(p []byte) (int, error) {
	if len(p) > 2 {
		p = p[:2]
	}
	return w.Buffer.Write(p)
}

func TestWriteFrameHandlesShortWrites(t *testing.T) {
	var w shortWriter
	if err := (&frameWriter{w: &w}).write([]byte("ethernet frame")); err != nil {
		t.Fatal(err)
	}
	got, err := (&frameReader{r: &w.Buffer}).read(65535)
	if err != nil || string(got) != "ethernet frame" {
		t.Fatalf("read frame = %q, %v", got, err)
	}
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }

func TestWriteFrameRejectsZeroProgress(t *testing.T) {
	if err := (&frameWriter{w: zeroWriter{}}).write([]byte("frame")); err != io.ErrShortWrite {
		t.Fatalf("writeFrame = %v, want io.ErrShortWrite", err)
	}
}
