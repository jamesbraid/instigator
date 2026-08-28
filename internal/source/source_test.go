package source

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// TestResolveRawImageWithChecksumFetchesWhole: a raw image given an expected
// sha256 is fetched whole and opened from the cache as an OriginImage, never
// taking the range branch - verifying a digest means reading every byte, so
// there is nothing to gain by ranging. The server honours Range, so absent the
// digest the resolver would read it by Range-bearing chunk GETs and never issue
// a Range-less fetch. Kind and content alone cannot tell the branches apart
// (both yield an OriginImage serving the same bytes), so this asserts the two
// side effects only the whole-file branch produces: exactly one GET with no
// Range header, and a populated CacheDir.
func TestResolveRawImageWithChecksumFetchesWhole(t *testing.T) {
	raw := efsImageDistSA(t)
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])

	var noRangeFetches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			noRangeFetches.Add(1)
		}
		http.ServeContent(w, r, "tools.image", time.Time{}, bytes.NewReader(raw))
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	r := New(Options{CacheDir: cacheDir, Client: srv.Client()})
	res, err := r.Resolve(srv.URL+"/tools.image", digest)
	if err != nil || res.Kind != vfs.OriginImage {
		t.Fatalf("kind=%v err=%v", res.Kind, err)
	}
	defer res.Closer.Close()

	if b, err := fs.ReadFile(res.FS, "dist/sa"); err != nil || string(b) != "SA" {
		t.Fatalf("dist/sa=%q err=%v", b, err)
	}

	if got := noRangeFetches.Load(); got != 1 {
		t.Errorf("no-Range full fetches=%d, want 1 (a digest must fetch whole, not by range)", got)
	}
	if !cacheHasFile(t, cacheDir) {
		t.Errorf("CacheDir %s is empty; the digested fetch did not cache a whole-file download", cacheDir)
	}
}

// TestResolveRawImageChecksumMismatch: a raw image whose bytes do not match the
// given sha256 is rejected - Resolve errors and nothing is served - proving the
// digest is actually verified, not merely a signal to fetch whole.
func TestResolveRawImageChecksumMismatch(t *testing.T) {
	raw := efsImageDistSA(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "tools.image", time.Time{}, bytes.NewReader(raw))
	}))
	defer srv.Close()

	wrong := hex.EncodeToString(bytes.Repeat([]byte{0xab}, sha256.Size))
	r := New(Options{CacheDir: t.TempDir(), Client: srv.Client()})
	res, err := r.Resolve(srv.URL+"/tools.image", wrong)
	if err == nil {
		res.Closer.Close()
		t.Fatal("Resolve accepted an image whose sha256 did not match; want rejection")
	}
}

// efsImageFarApartFiles builds a CD image holding n small files whose data
// blocks are separated by gap bytes, so reading two of them forces ReadAt
// offsets that land in different 1 MiB chunks of the caching reader - the
// spacing that would trip an unlocked shared-buffer reader. Each file's
// content is a distinct repeated byte, so a torn concurrent read surfaces as
// wrong bytes, not merely a race-detector report. The gaps are laid down with
// AddData, which advances
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
// files sit multiple megabytes apart, so concurrent reads keep touching
// different 1 MiB chunks of the caching reader. A reader that shared one
// mutable buffer with no locking would race here (mutating its buffer, offset
// and error) and could return wrong bytes; cachingReaderAt is thread-safe by
// construction, so this must run clean under `go test -race`, with every read
// yielding its file's exact content.
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

// TestFetchCacheKeyedOnFullURL proves the fetch cache keys on the whole URL,
// not the basename: two archive URLs that differ only in their version
// directory - .../6.5.30/disc1.tar.gz and .../6.5.22/disc1.tar.gz - must land
// in distinct cache files and each serve its own bytes. Keyed on the basename
// alone they would collide and grab's skip-if-complete would serve the first
// version's bytes for the second.
func TestFetchCacheKeyedOnFullURL(t *testing.T) {
	a := tgz(t, map[string]string{"dist/version": "6.5.30"})
	b := tgz(t, map[string]string{"dist/version": "6.5.22"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "6.5.30"):
			w.Write(a)
		case strings.Contains(r.URL.Path, "6.5.22"):
			w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	r := New(Options{CacheDir: t.TempDir(), Client: srv.Client()})
	url30 := srv.URL + "/6.5.30/disc1.tar.gz"
	url22 := srv.URL + "/6.5.22/disc1.tar.gz"

	if d30, d22 := r.cacheDest(url30, "disc1.tar.gz"), r.cacheDest(url22, "disc1.tar.gz"); d30 == d22 {
		t.Fatalf("cacheDest collides for distinct URLs sharing a basename: both %s", d30)
	}

	res30, err := r.Resolve(url30, "")
	if err != nil {
		t.Fatalf("resolve 6.5.30: %v", err)
	}
	defer res30.Closer.Close()
	res22, err := r.Resolve(url22, "")
	if err != nil {
		t.Fatalf("resolve 6.5.22: %v", err)
	}
	defer res22.Closer.Close()

	if got, _ := fs.ReadFile(res30.FS, "dist/version"); string(got) != "6.5.30" {
		t.Errorf("6.5.30/disc1.tar.gz served %q, want 6.5.30", got)
	}
	if got, _ := fs.ReadFile(res22.FS, "dist/version"); string(got) != "6.5.22" {
		t.Errorf("6.5.22/disc1.tar.gz served %q, want 6.5.22 (basename cache collision?)", got)
	}
}

// TestClientStripsAuthOnCrossHostRedirect: a source that 302s to a different
// host:port must not carry the Authorization header along. net/http compares
// only hostnames, so it would forward the header across a port change on the
// same host; the resolver's CheckRedirect strips it. The two httptest servers
// share the 127.0.0.1 hostname and differ only in port, exactly the case
// net/http would not catch on its own.
func TestClientStripsAuthOnCrossHostRedirect(t *testing.T) {
	gotAuth := make(chan string, 1)
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth <- r.Header.Get("Authorization")
		w.Write([]byte("ok"))
	}))
	defer dest.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL+"/next", http.StatusFound)
	}))
	defer src.Close()

	r := New(Options{CacheDir: t.TempDir()})
	req, _ := http.NewRequest(http.MethodGet, src.URL+"/start", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	select {
	case got := <-gotAuth:
		if got != "" {
			t.Errorf("cross-host redirect leaked Authorization: %q", got)
		}
	default:
		t.Fatal("redirect target was never reached")
	}
}

// TestClientKeepsAuthOnSameHostRedirect: a redirect that stays on the same
// host:port must keep the Authorization header, so a source that internally
// redirects to another path of itself still authenticates. This is the guard
// against a too-broad strip that would break legitimate same-origin redirects.
func TestClientKeepsAuthOnSameHostRedirect(t *testing.T) {
	finalAuth := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		finalAuth <- r.Header.Get("Authorization")
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	r := New(Options{CacheDir: t.TempDir()})
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/start", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	select {
	case got := <-finalAuth:
		if got != "Bearer sekret" {
			t.Errorf("same-host redirect dropped Authorization: got %q, want %q", got, "Bearer sekret")
		}
	default:
		t.Fatal("same-host redirect never reached /final")
	}
}

// TestStripAuthOnCrossHostRedirect exercises the redirect credential policy
// directly: Authorization survives only a same-host redirect that does not
// downgrade https to http. A downgrade would put Basic credentials on the wire
// in cleartext; a host or port change would hand them to a different server.
func TestStripAuthOnCrossHostRedirect(t *testing.T) {
	cases := []struct {
		name     string
		from, to string
		wantKeep bool
	}{
		{"same host https", "https://media.example/a", "https://media.example/b", true},
		{"same host http", "http://media.example/a", "http://media.example/b", true},
		{"downgrade to http", "https://media.example/a", "http://media.example/b", false},
		{"cross host", "https://media.example/a", "https://other.example/b", false},
		{"port change", "https://media.example/a", "https://media.example:8443/b", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig, err := http.NewRequest(http.MethodGet, tc.from, nil)
			if err != nil {
				t.Fatalf("orig request: %v", err)
			}
			next, err := http.NewRequest(http.MethodGet, tc.to, nil)
			if err != nil {
				t.Fatalf("next request: %v", err)
			}
			next.Header.Set("Authorization", "Basic c2VrcmV0")
			if err := stripAuthOnCrossHostRedirect(next, []*http.Request{orig}); err != nil {
				t.Fatalf("stripAuthOnCrossHostRedirect: %v", err)
			}
			switch got := next.Header.Get("Authorization"); {
			case tc.wantKeep && got == "":
				t.Errorf("Authorization was stripped, want kept")
			case !tc.wantKeep && got != "":
				t.Errorf("Authorization survived as %q, want stripped", got)
			}
		})
	}
}
