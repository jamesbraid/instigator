package instcmd

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/jamesbraid/instigator/internal/logging"
)

type writeCounter struct {
	calls int
	bytes int
}

func (w *writeCounter) Write(p []byte) (int, error) {
	w.calls++
	w.bytes += len(p)
	return len(p), nil
}

func BenchmarkShellDD(b *testing.B) {
	payload := make([]byte, 1024*1024)
	fsys := &fakeFS{files: map[string][]byte{"dist/data": payload}}
	logger := logging.New(io.Discard, logging.LevelInfo)
	for _, block := range []int{512, 4096, 32768, 65536} {
		b.Run(fmt.Sprint(block), func(b *testing.B) {
			script := fmt.Sprintf("dd if=/dist/data bs=%d\n", block)
			var out writeCounter
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				out = writeCounter{}
				if err := RunShell(fsys, strings.NewReader(script), &out, io.Discard, logger, nil); err != nil {
					b.Fatal(err)
				}
				if out.bytes != len(payload) {
					b.Fatalf("copied %d bytes, want %d", out.bytes, len(payload))
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(out.calls), "stdout-writes/MiB")
		})
	}
}
