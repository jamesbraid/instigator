package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	bufra "github.com/avvmoto/buf-readerat"
	"github.com/cavaliergopher/grab/v3"
	extract "github.com/hashicorp/go-extract"
	"github.com/snabb/httpreaderat"

	"github.com/jamesbraid/instigator/internal/vfs"
)

// maxExtractBytes bounds both the archive we feed the extractor and the total
// it writes out, so a hostile or corrupt source cannot exhaust the disk via a
// compression bomb or an oversized input.
const maxExtractBytes = 8 << 30

// Options configures a Resolver: where fetched archives are cached, the
// credentials offered to https hosts, the HTTP client to fetch with
// (http.DefaultClient when nil), and whether to force a whole-file download
// even for a range-capable server.
type Options struct {
	CacheDir    string
	Credentials Credentials
	Client      *http.Client
	ForceWhole  bool
}

// Resolver turns a source reference - a local path or an http(s) URL - into a
// vfs.Resolved: the read-only filesystem it serves, whether that is an EFS
// image or an extracted directory, and the closer that frees it. It satisfies
// vfs.Resolver.
type Resolver struct {
	cacheDir   string
	creds      Credentials
	client     *http.Client
	grab       *grab.Client
	forceWhole bool
}

var _ vfs.Resolver = (*Resolver)(nil)

// New returns a Resolver configured by opts. A nil opts.Client defaults to
// http.DefaultClient.
func New(opts Options) *Resolver {
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	return &Resolver{
		cacheDir:   opts.CacheDir,
		creds:      opts.Credentials,
		client:     client,
		grab:       &grab.Client{HTTPClient: client, UserAgent: "instigator"},
		forceWhole: opts.ForceWhole,
	}
}

// Resolve turns ref into a read-only filesystem. sha256hex, when non-empty,
// is the layer's expected digest; it is verified against a whole-file fetch
// (an archive or a ForceWhole image) and ignored for a local source or a
// range-served image, where a partial read cannot be hashed.
func (r *Resolver) Resolve(ref string, sha256hex string) (vfs.Resolved, error) {
	if !isURL(ref) {
		return r.resolveLocal(ref)
	}
	u, err := url.Parse(ref)
	if err != nil {
		return vfs.Resolved{}, fmt.Errorf("source: parse %s: %w", SafeURL(ref), unwrapURLErr(err))
	}
	ctx := context.Background()
	if isArchivePath(u.Path) {
		return r.resolveArchive(ctx, ref, u, sha256hex)
	}
	return r.resolveRaw(ctx, ref, u, sha256hex)
}

// isURL reports whether ref carries an http(s) scheme; anything else is a
// local filesystem path.
func isURL(ref string) bool {
	return strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://")
}

// isTreePath reports whether p's lowercased extension marks a tar archive
// that extracts to a directory tree.
func isTreePath(p string) bool {
	lp := strings.ToLower(p)
	return strings.HasSuffix(lp, ".tar.gz") ||
		strings.HasSuffix(lp, ".tgz") ||
		strings.HasSuffix(lp, ".tar")
}

// isArchivePath reports whether p is any archive this resolver unpacks: a
// tar tree, or a bare .gz wrapping a single image.
func isArchivePath(p string) bool {
	return isTreePath(p) || strings.HasSuffix(strings.ToLower(p), ".gz")
}

// resolveLocal opens a local path: a directory as an os.Root read-only view,
// a file as an EFS image. sha256 is not applied to a local source.
func (r *Resolver) resolveLocal(ref string) (vfs.Resolved, error) {
	info, err := os.Stat(ref)
	if err != nil {
		return vfs.Resolved{}, fmt.Errorf("source: stat %s: %w", ref, err)
	}
	if info.IsDir() {
		root, err := os.OpenRoot(ref)
		if err != nil {
			return vfs.Resolved{}, fmt.Errorf("source: open %s: %w", ref, err)
		}
		return vfs.Resolved{FS: root.FS(), Kind: vfs.OriginDirectory, Closer: root}, nil
	}
	disc, err := vfs.OpenImage(ref)
	if err != nil {
		return vfs.Resolved{}, fmt.Errorf("source: %w", err)
	}
	return vfs.Resolved{FS: disc.FSys(), Kind: vfs.OriginImage, Closer: disc}, nil
}

// resolveArchive fetches an archive into the cache (skipping the download when
// a complete copy is already there, verifying sha256 when given) and extracts
// it fresh into a temporary directory that is discarded when the result is
// closed. A tar archive yields a directory; a bare .gz yields the single
// image it wraps.
func (r *Resolver) resolveArchive(ctx context.Context, ref string, u *url.URL, sha256hex string) (vfs.Resolved, error) {
	base := path.Base(u.Path)
	archive := filepath.Join(r.cacheDir, base)
	if err := r.download(ctx, ref, sha256hex, archive); err != nil {
		return vfs.Resolved{}, err
	}

	if isTreePath(u.Path) {
		tmp, err := r.extract(ctx, archive, "")
		if err != nil {
			return vfs.Resolved{}, err
		}
		root, err := os.OpenRoot(tmp)
		if err != nil {
			os.RemoveAll(tmp)
			return vfs.Resolved{}, fmt.Errorf("source: open %s: %w", tmp, err)
		}
		return vfs.Resolved{FS: root.FS(), Kind: vfs.OriginDirectory, Closer: cleanup(root, tmp)}, nil
	}

	// A bare .gz decompresses to a single image, named after base with the
	// .gz suffix stripped. base reached this branch only because its lowercased
	// suffix is ".gz", so the final three bytes are that suffix whatever their
	// case, and can be trimmed by byte count.
	name := base[:len(base)-len(".gz")]
	tmp, err := r.extract(ctx, archive, name)
	if err != nil {
		return vfs.Resolved{}, err
	}
	disc, err := vfs.OpenImage(filepath.Join(tmp, name))
	if err != nil {
		os.RemoveAll(tmp)
		return vfs.Resolved{}, fmt.Errorf("source: %w", err)
	}
	return vfs.Resolved{FS: disc.FSys(), Kind: vfs.OriginImage, Closer: cleanup(disc, tmp)}, nil
}

// extract unpacks archive into a fresh temporary directory beneath the cache
// and returns that directory. When outName is empty the archive is a tar tree
// unpacked in place; otherwise it is a single-file .gz decompressed to
// outName within the directory (never untarred, so a .image.gz stays one
// image). The caller opens the result and removes the directory when done.
func (r *Resolver) extract(ctx context.Context, archive, outName string) (string, error) {
	tmp, err := os.MkdirTemp(r.cacheDir, "extract-*")
	if err != nil {
		return "", fmt.Errorf("source: create extract dir: %w", err)
	}
	f, err := os.Open(archive)
	if err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("source: open archive %s: %w", archive, err)
	}
	defer f.Close()

	dst := tmp
	opts := []extract.ConfigOption{
		extract.WithCreateDestination(true),
		extract.WithMaxInputSize(maxExtractBytes),
		extract.WithMaxExtractionSize(maxExtractBytes),
		// Contained symlinks in a dist tree must materialize; an escaping or
		// absolute target is still refused by go-extract's default security
		// check (and again, at serve time, by the os.Root the tree is read
		// through). WithInsecureTraverseSymlinks is deliberately never set.
		extract.WithDenySymlinkExtraction(false),
	}
	if outName != "" {
		dst = filepath.Join(tmp, outName)
		opts = append(opts, extract.WithNoUntarAfterDecompression(true))
	}
	if err := extract.Unpack(ctx, dst, f, extract.NewConfig(opts...)); err != nil {
		os.RemoveAll(tmp)
		return "", fmt.Errorf("source: extract %s: %w", filepath.Base(archive), err)
	}
	return tmp, nil
}

// resolveRaw opens a URL with no archive extension as a single EFS image.
// Without ForceWhole the image is read lazily by byte-range - nothing hitting
// disk on a range-capable server, and buffered whole into a temporary file
// only if the server ignores ranges. With ForceWhole it is fetched whole into
// the cache (verifying sha256) and opened from there.
func (r *Resolver) resolveRaw(ctx context.Context, ref string, u *url.URL, sha256hex string) (vfs.Resolved, error) {
	if r.forceWhole {
		name := path.Base(u.Path)
		if name == "." || name == "/" || name == "" {
			name = "image"
		}
		dst := filepath.Join(r.cacheDir, name)
		if err := r.download(ctx, ref, sha256hex, dst); err != nil {
			return vfs.Resolved{}, err
		}
		disc, err := vfs.OpenImage(dst)
		if err != nil {
			return vfs.Resolved{}, fmt.Errorf("source: %w", err)
		}
		return vfs.Resolved{FS: disc.FSys(), Kind: vfs.OriginImage, Closer: disc}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref, nil)
	if err != nil {
		return vfs.Resolved{}, fmt.Errorf("source: fetch %s: build request: %w", SafeURL(ref), unwrapURLErr(err))
	}
	r.creds.apply(req)

	// StoreFile spills the fallback whole download to a temporary file, so a
	// large non-range image cannot exhaust memory. On the range path the store
	// stays empty and its Close is a no-op; either way Close is the closer that
	// frees the reader.
	store := httpreaderat.NewStoreFile()
	hra, err := httpreaderat.New(r.client, req, store)
	if err != nil {
		store.Close()
		return vfs.Resolved{}, fmt.Errorf("source: fetch %s: %w", SafeURL(ref), unwrapURLErr(err))
	}
	// Coalesce efs's small scattered reads into 1 MiB range requests, then
	// serialize access: buf-readerat keeps one shared buffer and offset with no
	// locking, but this reader backs a single Resolved.FS that is read
	// concurrently at serve time - TFTP, NFS and rsh each read it from their own
	// goroutines. Without the guard two far-apart ReadAts race on that buffer
	// and can hand the OS installer the wrong bytes.
	buf := bufra.NewBufReaderAt(hra, 1<<20)
	disc, err := vfs.OpenImageReader(&syncReaderAt{r: buf}, store, SafeURL(ref))
	if err != nil {
		store.Close()
		return vfs.Resolved{}, fmt.Errorf("source: %w", err)
	}
	return vfs.Resolved{FS: disc.FSys(), Kind: vfs.OriginImage, Closer: disc}, nil
}

// download fetches ref into dst via grab: a complete dst already on disk is
// reused rather than re-fetched (grab's skip/resume is the fetch cache), and
// sha256hex, when given, is validated against the downloaded bytes with the
// file deleted on mismatch. Credentials are attached to the request out of
// band; the URL is never echoed into an error with its userinfo intact.
func (r *Resolver) download(ctx context.Context, ref, sha256hex, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("source: create cache dir: %w", err)
	}
	req, err := grab.NewRequest(dst, ref)
	if err != nil {
		return fmt.Errorf("source: fetch %s: build request: %w", SafeURL(ref), unwrapURLErr(err))
	}
	req = req.WithContext(ctx)
	r.creds.apply(req.HTTPRequest)
	if sha256hex != "" {
		sum, err := hex.DecodeString(sha256hex)
		if err != nil {
			return fmt.Errorf("source: fetch %s: invalid sha256 %q: %w", SafeURL(ref), sha256hex, err)
		}
		req.SetChecksum(sha256.New(), sum, true)
	}
	if err := r.grab.Do(req).Err(); err != nil {
		return fmt.Errorf("source: fetch %s: %w", SafeURL(ref), unwrapURLErr(err))
	}
	return nil
}

// cleanup returns a closer that closes c and then removes tmp, so a fresh
// extraction is discarded when the resolved filesystem is closed - there is
// no extracted-tree cache, only the fetched archive persists.
func cleanup(c io.Closer, tmp string) io.Closer {
	return closerFunc(func() error {
		err := c.Close()
		if rmErr := os.RemoveAll(tmp); rmErr != nil && err == nil {
			err = rmErr
		}
		return err
	})
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// syncReaderAt serializes ReadAt on an io.ReaderAt whose own ReadAt is not safe
// for concurrent use. It guards the buffering range reader (buf-readerat),
// which mutates one shared buffer, offset and error without locking, so the
// concurrent reads a served image FS takes never corrupt each other.
// httpreaderat's own ReadAt is already concurrency-safe; only the buffering
// layer above it needs this lock.
type syncReaderAt struct {
	mu sync.Mutex
	r  io.ReaderAt
}

func (s *syncReaderAt) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r.ReadAt(p, off)
}
