package source

import (
	"bytes"
	"testing"

	"github.com/jamesbraid/instigator/efs"
	"github.com/jamesbraid/instigator/efs/efstest"
)

func BenchmarkCachedEFSReadAt(b *testing.B) {
	img := efstest.New()
	ino := img.AddFile(0o444, bytes.Repeat([]byte("x"), 64*1024))
	backing := newCountingReaderAt(img.Bytes())
	cache := newCachingReaderAt(backing, chunkCacheChunk, chunkCacheBudget)
	fs, err := efs.Open(cache, 0)
	if err != nil {
		b.Fatal(err)
	}
	node, err := fs.Inode(ino)
	if err != nil {
		b.Fatal(err)
	}
	p := make([]byte, 32*1024)
	if _, err := fs.ReadAt(node, p, 0); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(p)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := fs.ReadAt(node, p, 0); err != nil {
			b.Fatal(err)
		}
	}
}
