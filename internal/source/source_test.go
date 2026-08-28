package source

import (
	"bytes"
	"io/fs"
	"net/http"
	"net/http/httptest"
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
