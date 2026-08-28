package source

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/efs/efstest"
	"github.com/jamesbraid/instigator/internal/vfs"
)

// cacheHasFile reports whether dir holds any regular file. Only the
// cache-backed branches (whole-file fetch, archive extraction) write under a
// Resolver's CacheDir; the range branch never touches it, so a non-empty
// CacheDir proves a whole-file fetch ran.
func cacheHasFile(t *testing.T, dir string) bool {
	t.Helper()
	found := false
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cache dir: %v", err)
	}
	return found
}

// efsImage builds a minimal SGI CD image whose EFS root holds one file, the
// same shape vfs's makeImage produces, so a resolved image has a known path
// to read back.
func efsImage(t *testing.T, name, content string) []byte {
	t.Helper()
	img := efstest.New()
	ino := img.AddFile(0o644, []byte(content))
	img.SetRoot(map[string]uint32{name: ino})
	return img.CDImage(64, nil)
}

// efsImageDistSA builds a CD image whose EFS holds dist/sa = "SA", the same
// nested fixture vfs's makeImage produces, so a resolved image has a known
// sub-directory path to read back.
func efsImageDistSA(t *testing.T) []byte {
	t.Helper()
	img := efstest.New()
	sa := img.AddFile(0o644, []byte("SA"))
	dist := img.AddDir(map[string]uint32{"sa": sa})
	img.SetRoot(map[string]uint32{"dist": dist})
	return img.CDImage(64, nil)
}

// TestResolveRemoteTarGzTree: a served .tar.gz resolves to an OriginDirectory
// whose tree is readable. The URL path carries the .tar.gz suffix so
// extension dispatch fires; the handler serves the archive for any path.
func TestResolveRemoteTarGzTree(t *testing.T) {
	data := tgz(t, map[string]string{"foundations/dist/pkg": "P"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	}))
	defer srv.Close()

	r := New(Options{CacheDir: t.TempDir(), Client: srv.Client()})
	res, err := r.Resolve(srv.URL+"/foundations.tar.gz", "")
	if err != nil || res.Kind != vfs.OriginDirectory {
		t.Fatalf("kind=%v err=%v", res.Kind, err)
	}
	defer res.Closer.Close()

	b, err := fs.ReadFile(res.FS, "foundations/dist/pkg")
	if err != nil || string(b) != "P" {
		t.Fatalf("pkg=%q err=%v", b, err)
	}
}

// TestResolveRemoteRangeImage: a range-served raw EFS image resolves to an
// OriginImage read lazily by byte-range (no scheme extension is an archive),
// and a known in-image file reads back through the returned fs.FS.
func TestResolveRemoteRangeImage(t *testing.T) {
	raw := efsImage(t, "unix", "UNIX")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ServeContent honours Range on the seekable content, so probeRange
		// sees 206 and the resolver takes the range branch. Zero modtime
		// keeps Last-Modified (and thus a consistency pin) out of it.
		http.ServeContent(w, r, "tools.image", time.Time{}, bytes.NewReader(raw))
	}))
	defer srv.Close()

	r := New(Options{CacheDir: t.TempDir(), Client: srv.Client()})
	res, err := r.Resolve(srv.URL+"/tools.image", "")
	if err != nil || res.Kind != vfs.OriginImage {
		t.Fatalf("kind=%v err=%v", res.Kind, err)
	}
	defer res.Closer.Close()

	b, err := fs.ReadFile(res.FS, "unix")
	if err != nil || string(b) != "UNIX" {
		t.Fatalf("unix=%q err=%v", b, err)
	}
}

// TestResolveRawNonRangeServer: a raw image on a server that ignores Range
// (always 200, whole body) still resolves to a readable OriginImage. The
// range reader (httpreaderat) detects the missing range support from its
// probe and falls back to buffering the whole object into its Store, so the
// image opens and a known in-image file reads back. Two resolves in a row
// both succeed - the fallback is per-resolve, not a persistent cache.
func TestResolveRawNonRangeServer(t *testing.T) {
	raw := efsImageDistSA(t)

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(raw) // ignores Range: always the whole body with 200
	}))
	defer srv.Close()

	r := New(Options{CacheDir: t.TempDir(), Client: srv.Client()})
	url := srv.URL + "/tools.image"

	for _, pass := range []string{"first", "second"} {
		res, err := r.Resolve(url, "")
		if err != nil || res.Kind != vfs.OriginImage {
			t.Fatalf("%s resolve: kind=%v err=%v", pass, res.Kind, err)
		}
		if b, err := fs.ReadFile(res.FS, "dist/sa"); err != nil || string(b) != "SA" {
			t.Fatalf("%s read: dist/sa=%q err=%v", pass, b, err)
		}
		res.Closer.Close()
	}
	if hits.Load() == 0 {
		t.Fatal("server never hit; resolve did not go remote")
	}
}

// TestResolveMaliciousArchiveContained: a served .tar.gz whose entries try to
// escape the extraction directory - a "../" file and a symlink pointing out -
// must be refused, so Resolve errors and nothing lands outside. This guards
// our go-extract wiring (default security check on, symlink traversal never
// enabled), not go-extract itself. The extraction dir sits directly under
// CacheDir, so a "../ESCAPED" entry that slipped through would surface at
// CacheDir/ESCAPED.
func TestResolveMaliciousArchiveContained(t *testing.T) {
	cacheDir := t.TempDir()
	data := tarWith(t, []tar.Header{
		{Name: "../ESCAPED", Typeflag: tar.TypeReg},
		{Name: "evil-link", Typeflag: tar.TypeSymlink, Linkname: "../ESCAPED"},
	}, map[string]string{"../ESCAPED": "pwned"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	}))
	defer srv.Close()

	r := New(Options{CacheDir: cacheDir, Client: srv.Client()})
	res, err := r.Resolve(srv.URL+"/evil.tar.gz", "")
	if err == nil {
		res.Closer.Close()
		t.Fatal("Resolve accepted a traversing archive; want rejection")
	}
	if _, statErr := os.Stat(filepath.Join(cacheDir, "ESCAPED")); !os.IsNotExist(statErr) {
		t.Fatalf("archive escaped the extraction dir: CacheDir/ESCAPED exists (stat err=%v)", statErr)
	}
}

// TestResolveForceWholeOnRangeServer: with ForceWhole set, a range-capable
// server is still fetched whole and opened from the cache as an OriginImage,
// never taking the range branch. Kind and content alone cannot tell the
// branches apart (both yield an OriginImage serving the same bytes), so this
// asserts the two side effects only the whole-file branch produces: exactly
// one fetchWhole GET with no Range header, and a populated CacheDir. Either
// would fail if ForceWhole were dropped or the condition inverted.
func TestResolveForceWholeOnRangeServer(t *testing.T) {
	raw := efsImageDistSA(t)

	// ServeContent honours Range, so absent ForceWhole the resolver would read
	// this by Range-bearing chunk GETs and never issue a Range-less fetch. A
	// no-Range GET here is the whole-file fetch; the probe carries Range.
	var noRangeFetches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			noRangeFetches.Add(1)
		}
		http.ServeContent(w, r, "tools.image", time.Time{}, bytes.NewReader(raw))
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	r := New(Options{CacheDir: cacheDir, Client: srv.Client(), ForceWhole: true})
	res, err := r.Resolve(srv.URL+"/tools.image", "")
	if err != nil || res.Kind != vfs.OriginImage {
		t.Fatalf("kind=%v err=%v", res.Kind, err)
	}
	defer res.Closer.Close()

	if b, err := fs.ReadFile(res.FS, "dist/sa"); err != nil || string(b) != "SA" {
		t.Fatalf("dist/sa=%q err=%v", b, err)
	}

	if got := noRangeFetches.Load(); got != 1 {
		t.Errorf("no-Range full fetches=%d, want 1 (ForceWhole must fetch whole, not by range)", got)
	}
	if !cacheHasFile(t, cacheDir) {
		t.Errorf("CacheDir %s is empty; ForceWhole did not cache a whole-file fetch", cacheDir)
	}
}

// efsImageFarApartFiles builds a CD image holding n small files whose data
// blocks are separated by gap bytes, so reading two of them forces ReadAt
// offsets more than the range reader's 1 MiB buffer apart - the spacing that
// evicts and refills that shared buffer. Each file's content is a distinct
// repeated byte, so a torn concurrent read surfaces as wrong bytes, not merely
// a race-detector report. The gaps are laid down with AddData, which advances
// the data-block cursor without spending an inode, so the fixture stays within
// efstest's eight-inode geometry however wide the gaps are. It returns the
// image and the expected content of each file by name.
func efsImageFarApartFiles(t *testing.T, n, size, gap int) ([]byte, map[string]string) {
	t.Helper()
	b := efstest.New()
	want := make(map[string]string, n)
	root := make(map[string]uint32, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("f%d", i)
		content := string(bytes.Repeat([]byte{byte('A' + i)}, size))
		root[name] = b.AddFile(0o644, []byte(content))
		want[name] = content
		if i < n-1 {
			b.AddData(make([]byte, gap)) // pushes the next file's blocks past the buffer, no inode spent
		}
	}
	b.SetRoot(root)
	return b.CDImage(64, nil), want
}

// TestResolveRangeImageConcurrentReadsRace drives the range path the way the
// servers do: one Resolved.FS read by many goroutines at once. The image's
// files sit multiple megabytes apart, so concurrent reads keep evicting and
// refilling the range reader's shared 1 MiB buffer. Without the syncReaderAt
// guard around that buffer this races (buf-readerat mutates its buffer, offset
// and error with no locking) and can return wrong bytes; it must run clean
// under `go test -race`, with every read yielding its file's exact content.
func TestResolveRangeImageConcurrentReadsRace(t *testing.T) {
	const (
		files = 4
		size  = 4096
		gap   = 2 << 20 // 2 MiB, comfortably past the 1 MiB range-reader buffer
	)
	raw, want := efsImageFarApartFiles(t, files, size, gap)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ServeContent honours Range on seekable content, so the resolver takes
		// the range branch. Zero modtime keeps Last-Modified out of it.
		http.ServeContent(w, r, "tools.image", time.Time{}, bytes.NewReader(raw))
	}))
	defer srv.Close()

	r := New(Options{CacheDir: t.TempDir(), Client: srv.Client()})
	res, err := r.Resolve(srv.URL+"/tools.image", "")
	if err != nil || res.Kind != vfs.OriginImage {
		t.Fatalf("kind=%v err=%v", res.Kind, err)
	}
	defer res.Closer.Close()

	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}

	const (
		workers = 64
		iters   = 20
	)
	var wg sync.WaitGroup
	for g := 0; g < workers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				name := names[(g+i)%len(names)]
				b, err := fs.ReadFile(res.FS, name)
				if err != nil {
					t.Errorf("worker %d read %s: %v", g, name, err)
					return
				}
				if string(b) != want[name] {
					t.Errorf("worker %d read %s: wrong/torn bytes (got %d bytes)", g, name, len(b))
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
