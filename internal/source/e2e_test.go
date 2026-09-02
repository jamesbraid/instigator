package source

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/efs/efstest"
	"github.com/jamesbraid/instigator/internal/vfs"
)

// readTree returns a tree path's full contents, read through fs.File - the
// same shape internal/vfs's own readTree test helper uses, reimplemented
// here since that one lives in a different package.
func readTree(t *testing.T, tree *vfs.Tree, name string) string {
	t.Helper()
	f, err := tree.Open(name)
	if err != nil {
		t.Fatalf("Open(%q): %v", name, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %q: %v", name, err)
	}
	return string(b)
}

// TestEndToEndRemoteSet proves the whole path from config to served bytes: a
// remote .tar.gz is fetched over http, extracted, and merged by vfs.Build
// into an install set whose dist a client can read - and whose origin still
// names the layer that provided it.
func TestEndToEndRemoteSet(t *testing.T) {
	data := tgz(t, map[string]string{"foundations/dist/inst.README": "hello"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	}))
	defer srv.Close()

	r := New(Options{CacheDir: t.TempDir(), Client: srv.Client()})
	tree, err := vfs.Build([]vfs.SetSpec{{
		Name: "6.5.30",
		Layers: []vfs.LayerSpec{
			{Name: "foundations", Source: srv.URL + "/foundations.tar.gz", Base: "foundations"},
		},
	}}, r)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer tree.Close()

	if got := readTree(t, tree, "6.5.30/dist/inst.README"); got != "hello" {
		t.Errorf("6.5.30/dist/inst.README = %q, want hello", got)
	}

	origin, err := tree.Resolve("6.5.30/dist/inst.README")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if origin.Source != "foundations" {
		t.Errorf("origin.Source = %q, want foundations", origin.Source)
	}
}

// countingResponseWriter tallies every byte written to the response body, so
// a test can measure how much of a served object actually crossed the wire.
type countingResponseWriter struct {
	http.ResponseWriter
	n *int64
}

func (c *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	atomic.AddInt64(c.n, int64(n))
	return n, err
}

// efsImagePaddedDistSA builds the efsImageDistSA fixture (dist/sa = "SA") plus
// a large unreferenced trailing file. No directory names the padding, so it
// sits in the image's data blocks without being on the path Build walks to
// reach dist/sa - letting a test prove the tail is never fetched.
func efsImagePaddedDistSA(t *testing.T, padBytes int) []byte {
	t.Helper()
	img := efstest.New()
	sa := img.AddFile(0o644, []byte("SA"))
	dist := img.AddDir(map[string]uint32{"sa": sa})
	img.SetRoot(map[string]uint32{"dist": dist})
	img.AddFile(0o644, make([]byte, padBytes))
	return img.CDImage(64, nil)
}

// TestEndToEndRangeImageTransfersOnlyTouchedBytes proves the range path is lazy
// end to end: a raw EFS image served from a range-capable httptest server,
// built and read through Build's Tree, transfers strictly fewer bytes than the
// image holds - the unreferenced multi-megabyte tail is never fetched. Counting
// bytes written by the server, not just requests, is what confirms it.
func TestEndToEndRangeImageTransfersOnlyTouchedBytes(t *testing.T) {
	const padBytes = 16 << 20 // 16 MiB: several chunks past dist/sa and its metadata
	raw := efsImagePaddedDistSA(t, padBytes)

	var transferred int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &countingResponseWriter{ResponseWriter: w, n: &transferred}
		// ServeContent honours Range on the seekable content, so probeRange
		// sees 206 and the resolver takes the range branch. Zero modtime
		// keeps Last-Modified (and thus a consistency pin) out of it.
		http.ServeContent(cw, r, "tools.image", time.Time{}, bytes.NewReader(raw))
	}))
	defer srv.Close()

	r := New(Options{CacheDir: t.TempDir(), Client: srv.Client()})
	tree, err := vfs.Build([]vfs.SetSpec{{
		Name: "tools",
		Layers: []vfs.LayerSpec{
			{Name: "tools", Source: srv.URL + "/tools.image"},
		},
	}}, r)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer tree.Close()

	if got := readTree(t, tree, "tools/dist/sa"); got != "SA" {
		t.Errorf("tools/dist/sa = %q, want SA", got)
	}

	got := atomic.LoadInt64(&transferred)
	if got >= int64(len(raw)) {
		t.Errorf("transferred %d bytes serving one file, want less than the full image (%d bytes); range reads should not fetch the untouched tail", got, len(raw))
	} else {
		t.Logf("transferred %d of %d image bytes (%.1f%%) to read one file by range", got, len(raw), 100*float64(got)/float64(len(raw)))
	}
}
