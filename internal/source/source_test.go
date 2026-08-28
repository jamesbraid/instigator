package source

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/efs/efstest"
	"github.com/jamesbraid/instigator/internal/vfs"
)

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

// TestResolveRawWholeFileFallback: a raw image on a server that ignores Range
// (always 200, whole body) resolves to an OriginImage fetched whole into the
// cache and opened from there - a distinct closer from the range branch (a
// Disc over a cache file, not the range reader). A second Resolve of the same
// URL is served from the cache, issuing no second full download.
func TestResolveRawWholeFileFallback(t *testing.T) {
	raw := efsImageDistSA(t)

	// The probe (probeRange) sends "Range: bytes=0-0"; the whole-file fetch
	// (fetchWhole) sends none. This server ignores Range and always returns
	// the full body with 200, so probeRange sees ranges=false. Counting by
	// the presence of the Range header separates the two.
	var probes, fetches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			probes.Add(1)
		} else {
			fetches.Add(1)
		}
		w.Write(raw)
	}))
	defer srv.Close()

	r := New(Options{CacheDir: t.TempDir(), Client: srv.Client()})
	url := srv.URL + "/tools.image"

	res, err := r.Resolve(url, "")
	if err != nil || res.Kind != vfs.OriginImage {
		t.Fatalf("first resolve: kind=%v err=%v", res.Kind, err)
	}
	if b, err := fs.ReadFile(res.FS, "dist/sa"); err != nil || string(b) != "SA" {
		t.Fatalf("first read: dist/sa=%q err=%v", b, err)
	}
	res.Closer.Close()
	if got := fetches.Load(); got != 1 {
		t.Fatalf("first resolve: full-body fetches=%d, want 1", got)
	}

	// Second resolve of the same URL: the cache serves the image, so no new
	// full-body fetch is issued (the probe still runs to key the cache).
	res2, err := r.Resolve(url, "")
	if err != nil || res2.Kind != vfs.OriginImage {
		t.Fatalf("second resolve: kind=%v err=%v", res2.Kind, err)
	}
	if b, err := fs.ReadFile(res2.FS, "dist/sa"); err != nil || string(b) != "SA" {
		t.Fatalf("second read: dist/sa=%q err=%v", b, err)
	}
	res2.Closer.Close()
	if got := fetches.Load(); got != 1 {
		t.Fatalf("second resolve re-downloaded: full-body fetches=%d, want 1 (cache reuse)", got)
	}
}

// TestResolveForceWholeOnRangeServer: with ForceWhole set, a range-capable
// server is still fetched whole and opened from the cache as an OriginImage,
// never taking the range branch.
func TestResolveForceWholeOnRangeServer(t *testing.T) {
	raw := efsImageDistSA(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "tools.image", time.Time{}, bytes.NewReader(raw))
	}))
	defer srv.Close()

	r := New(Options{CacheDir: t.TempDir(), Client: srv.Client(), ForceWhole: true})
	res, err := r.Resolve(srv.URL+"/tools.image", "")
	if err != nil || res.Kind != vfs.OriginImage {
		t.Fatalf("kind=%v err=%v", res.Kind, err)
	}
	defer res.Closer.Close()

	b, err := fs.ReadFile(res.FS, "dist/sa")
	if err != nil || string(b) != "SA" {
		t.Fatalf("dist/sa=%q err=%v", b, err)
	}
}
