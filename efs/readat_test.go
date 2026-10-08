package efs_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/jamesbraid/instigator/efs"
	"github.com/jamesbraid/instigator/efs/efstest"
)

type observedReaderAt struct {
	r     io.ReaderAt
	calls int
}

func (r *observedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.calls++
	return r.r.ReadAt(p, off)
}

func TestReadAtContiguousExtent(t *testing.T) {
	img := efstest.New()
	data := bytes.Repeat([]byte("extent data"), 4096)
	ino := img.AddFile(0o444, data)
	backing := &observedReaderAt{r: bytes.NewReader(img.Bytes())}
	fs, err := efs.Open(backing, 0)
	if err != nil {
		t.Fatal(err)
	}
	node, err := fs.Inode(ino)
	if err != nil {
		t.Fatal(err)
	}
	for _, off := range []int64{0, 13} {
		backing.calls = 0
		p := make([]byte, 32*1024)
		n, err := fs.ReadAt(node, p, off)
		if err != nil || n != len(p) || !bytes.Equal(p, data[off:off+int64(len(p))]) {
			t.Fatalf("ReadAt(%d) = %d, %v, data match %v", off, n, err, bytes.Equal(p, data[off:off+int64(len(p))]))
		}
		if backing.calls != 1 {
			t.Errorf("ReadAt(%d) used %d backing reads, want one contiguous read", off, backing.calls)
		}
	}
	if n, err := fs.ReadAt(node, nil, -1); n != 0 || err == nil {
		t.Fatalf("negative offset read = %d, %v, want an error", n, err)
	}
}

func TestReadAtPartitionOffset(t *testing.T) {
	img := efstest.New()
	data := bytes.Repeat([]byte("partition"), 1024)
	ino := img.AddFile(0o444, data)
	const base = 7
	image := append(make([]byte, base*512), img.Bytes()...)
	fs, err := efs.Open(bytes.NewReader(image), base)
	if err != nil {
		t.Fatal(err)
	}
	node, err := fs.Inode(ino)
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, len(data)+3)
	n, err := fs.ReadAt(node, p, 1)
	if n != len(data)-1 || err != io.EOF || !bytes.Equal(p[:n], data[1:]) {
		t.Fatalf("partition read = %d, %v, want %d, EOF and matching data", n, err, len(data)-1)
	}
}

func TestReadAtExtentsAndHoles(t *testing.T) {
	img := efstest.New()
	a := bytes.Repeat([]byte("a"), 3*512)
	b := bytes.Repeat([]byte("b"), 2*512)
	first, _ := img.AddData(a)
	img.AddData(bytes.Repeat([]byte("unrelated"), 512))
	second, _ := img.AddData(b)
	ino := img.AddFileExtents(8*512+71, [][3]uint32{
		{uint32(first), 3, 1}, {0xffffff, 0, 5}, {uint32(second), 2, 6},
	})
	want := make([]byte, 8*512+71)
	copy(want[512:], a)
	copy(want[6*512:], b)
	backing := &observedReaderAt{r: bytes.NewReader(img.Bytes())}
	fs, err := efs.Open(backing, 0)
	if err != nil {
		t.Fatal(err)
	}
	node, err := fs.Inode(ino)
	if err != nil {
		t.Fatal(err)
	}
	var offsets []int
	for off := 0; off < len(want); off += 137 {
		offsets = append(offsets, off)
	}
	offsets = append(offsets, len(want), len(want)+1)
	for _, off := range offsets {
		for _, size := range []int{0, 1, 511, 512, 777, len(want) + 13} {
			p := bytes.Repeat([]byte{0xff}, size)
			n, err := fs.ReadAt(node, p, int64(off))
			end := min(off+size, len(want))
			if off >= len(want) {
				if n != 0 || err != io.EOF {
					t.Fatalf("ReadAt(%d, %d) = %d, %v", off, size, n, err)
				}
				continue
			}
			wantErr := error(nil)
			if end-off < size {
				wantErr = io.EOF
			}
			if n != end-off || err != wantErr || !bytes.Equal(p[:n], want[off:end]) {
				t.Fatalf("ReadAt(%d, %d) = %d, %v, want %d, %v and matching bytes", off, size, n, err, end-off, wantErr)
			}
		}
	}
	backing.calls = 0
	p := make([]byte, len(want))
	if _, err := fs.ReadAt(node, p, 0); err != nil {
		t.Fatal(err)
	}
	if backing.calls != 2 {
		t.Errorf("read across two extents and holes used %d backing reads, want 2", backing.calls)
	}
}

type failingReaderAt struct {
	r       io.ReaderAt
	stop    int64
	failure error
}

func (r failingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.stop {
		return 0, r.failure
	}
	if int64(len(p)) > r.stop-off {
		n, _ := r.r.ReadAt(p[:r.stop-off], off)
		return n, r.failure
	}
	return r.r.ReadAt(p, off)
}

func TestReadAtBackingFailure(t *testing.T) {
	img := efstest.New()
	data := bytes.Repeat([]byte("x"), 4096)
	block, _ := img.AddData(data)
	ino := img.AddFileExtents(int32(len(data)), [][3]uint32{{uint32(block), 8, 0}})
	backing := bytes.NewReader(img.Bytes())
	fs, err := efs.Open(backing, 0)
	if err != nil {
		t.Fatal(err)
	}
	node, err := fs.Inode(ino)
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("backing read failed")
	const readable = 1024 + 17
	faulty, err := efs.Open(failingReaderAt{backing, block*512 + readable, failure}, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, len(data))
	n, err := faulty.ReadAt(node, p, 0)
	if n != readable || !errors.Is(err, failure) || !bytes.Equal(p[:n], data[:n]) {
		t.Fatalf("ReadAt = %d, %v, want %d and backing failure", n, err, readable)
	}
}

func BenchmarkReadAtExtent(b *testing.B) {
	for _, size := range []int{512, 32 * 1024} {
		b.Run(map[int]string{512: "512B", 32 * 1024: "32KiB"}[size], func(b *testing.B) {
			img := efstest.New()
			ino := img.AddFile(0o444, bytes.Repeat([]byte("x"), 64*1024))
			fs, err := efs.Open(bytes.NewReader(img.Bytes()), 0)
			if err != nil {
				b.Fatal(err)
			}
			node, err := fs.Inode(ino)
			if err != nil {
				b.Fatal(err)
			}
			p := make([]byte, size)
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := fs.ReadAt(node, p, 0); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
