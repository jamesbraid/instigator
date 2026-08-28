package source

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/jamesbraid/instigator/internal/vfs"
)

// Options configures a Resolver: where fetched and extracted content is
// cached, the credentials offered to https hosts, the HTTP client to fetch
// with (http.DefaultClient when nil), and whether to force a whole-file
// download even for a range-capable server.
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
	cache      *Cache
	creds      Credentials
	client     *http.Client
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
		cache:      NewCache(opts.CacheDir),
		creds:      opts.Credentials,
		client:     client,
		forceWhole: opts.ForceWhole,
	}
}

// Resolve turns ref into a read-only filesystem. sha256, when non-empty, is
// the layer's expected digest; it is verified against a whole-file fetch and
// ignored for a local source or a range-served image (a partial read cannot
// be hashed).
func (r *Resolver) Resolve(ref string, sha256 string) (vfs.Resolved, error) {
	if !isURL(ref) {
		return r.resolveLocal(ref)
	}
	u, err := url.Parse(ref)
	if err != nil {
		return vfs.Resolved{}, fmt.Errorf("source: parse %s: %w", SafeURL(ref), unwrapURLErr(err))
	}
	if isArchivePath(u.Path) {
		return r.resolveArchive(ref, u, sha256)
	}
	return r.resolveRaw(ref, u, sha256)
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

// resolveArchive fetches an archive whole (verifying sha256), extracts it
// into the cache once, and reuses that extraction thereafter. A tar archive
// yields a directory; a bare .gz yields the single image it wraps.
func (r *Resolver) resolveArchive(ref string, u *url.URL, sha256 string) (vfs.Resolved, error) {
	ctx := context.Background()

	// Probe for the metadata that keys the cache; a range-incapable server
	// (ranges false) is fine here, since the archive is fetched whole.
	size, _, etag, lastModified, err := probeRange(ctx, r.client, ref, r.creds)
	if err != nil {
		return vfs.Resolved{}, err
	}
	key := r.cache.Key(ref, etag, lastModified, size)
	base := path.Base(u.Path)

	dst, err := r.cache.Install(key, "extract", func(dst string) error {
		return r.fetchExtract(ctx, ref, sha256, base, dst)
	})
	if err != nil {
		return vfs.Resolved{}, err
	}

	if isTreePath(u.Path) {
		root, err := os.OpenRoot(dst)
		if err != nil {
			return vfs.Resolved{}, fmt.Errorf("source: open %s: %w", dst, err)
		}
		return vfs.Resolved{FS: root.FS(), Kind: vfs.OriginDirectory, Closer: root}, nil
	}

	// A bare .gz extracts to a single file inside the cache dir, named after
	// base with the .gz suffix stripped - the same name Extract chose. base
	// reached this branch only because its lowercased suffix is ".gz", so the
	// final three bytes are that suffix whatever their case.
	file := filepath.Join(dst, base[:len(base)-len(".gz")])
	disc, err := vfs.OpenImage(file)
	if err != nil {
		return vfs.Resolved{}, fmt.Errorf("source: %w", err)
	}
	return vfs.Resolved{FS: disc.FSys(), Kind: vfs.OriginImage, Closer: disc}, nil
}

// fetchExtract downloads ref to a temporary archive (verifying sha256), then
// extracts it into dst, the not-yet-existing cache path Install renames into
// place. The temp archive lives beside dst so it shares the cache filesystem
// and is discarded once extracted.
func (r *Resolver) fetchExtract(ctx context.Context, ref, sha256, base, dst string) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), "archive-*")
	if err != nil {
		return fmt.Errorf("source: create temp archive: %w", err)
	}
	archive := tmp.Name()
	tmp.Close()
	defer os.Remove(archive)

	if err := fetchWhole(ctx, r.client, ref, r.creds, sha256, archive); err != nil {
		return err
	}

	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("source: reopen archive %s: %w", archive, err)
	}
	defer f.Close()

	if _, _, err := Extract(f, base, dst); err != nil {
		return err
	}
	return nil
}

// resolveRaw opens a URL with no archive extension as a single EFS image. A
// range-capable server (and ForceWhole unset) is read lazily by byte-range,
// nothing hitting disk; otherwise the image is fetched whole into the cache
// (verifying sha256) and opened from there.
func (r *Resolver) resolveRaw(ref string, u *url.URL, sha256 string) (vfs.Resolved, error) {
	ctx := context.Background()

	size, ranges, etag, lastModified, err := probeRange(ctx, r.client, ref, r.creds)
	if err != nil {
		return vfs.Resolved{}, err
	}

	if ranges && !r.forceWhole {
		rr := newRangeReaderAt(ctx, r.client, ref, r.creds, size, etag, lastModified)
		// The Disc owns rr as both its reader and its closer, so closing the
		// Disc closes the range reader. SafeURL keeps any userinfo or query
		// out of the name that appears in the Disc's errors.
		disc, err := vfs.OpenImageReader(rr, rr, SafeURL(ref))
		if err != nil {
			rr.Close()
			return vfs.Resolved{}, fmt.Errorf("source: %w", err)
		}
		return vfs.Resolved{FS: disc.FSys(), Kind: vfs.OriginImage, Closer: disc}, nil
	}

	key := r.cache.Key(ref, etag, lastModified, size)
	name := path.Base(u.Path)
	if name == "." || name == "/" || name == "" {
		name = "image"
	}
	dst, err := r.cache.Install(key, name, func(dst string) error {
		return fetchWhole(ctx, r.client, ref, r.creds, sha256, dst)
	})
	if err != nil {
		return vfs.Resolved{}, err
	}
	disc, err := vfs.OpenImage(dst)
	if err != nil {
		return vfs.Resolved{}, fmt.Errorf("source: %w", err)
	}
	return vfs.Resolved{FS: disc.FSys(), Kind: vfs.OriginImage, Closer: disc}, nil
}
