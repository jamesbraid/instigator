package source

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// IsRemote reports whether ref names an http(s) URL rather than a local path.
func IsRemote(ref string) bool { return isURL(ref) }

// Probe confirms a remote source answers without reading its body. It sends
// a HEAD and, when the server refuses HEAD, a one-byte ranged GET whose body
// is closed unread. A 200 or 206 passes unless the reported size is zero.
// It returns the size the server reported, or -1 when it gave none. Errors
// name the URL with its secrets removed, as Resolve's do.
func (r *Resolver) Probe(ctx context.Context, ref string) (int64, error) {
	resp, err := r.probeRequest(ctx, http.MethodHead, ref)
	if err != nil {
		return -1, fmt.Errorf("source: probe %s: %w", SafeURL(ref), unwrapURLErr(err))
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Some object stores and CDNs reject HEAD outright; a ranged GET
		// asks the same question for at most one byte.
		resp, err = r.probeRequest(ctx, http.MethodGet, ref)
		if err != nil {
			return -1, fmt.Errorf("source: probe %s: %w", SafeURL(ref), unwrapURLErr(err))
		}
		resp.Body.Close()
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return -1, fmt.Errorf("source: probe %s: %s", SafeURL(ref), resp.Status)
	}
	size := responseSize(resp)
	if size == 0 {
		return 0, fmt.Errorf("source: probe %s: server reports an empty file", SafeURL(ref))
	}
	return size, nil
}

func (r *Resolver) probeRequest(ctx context.Context, method, ref string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, ref, nil)
	if err != nil {
		return nil, err
	}
	if method == http.MethodGet {
		req.Header.Set("Range", "bytes=0-0")
	}
	r.creds.apply(req)
	return r.client.Do(req)
}

// responseSize is the whole file's size: the total in a 206's Content-Range,
// otherwise Content-Length, or -1 when neither says.
func responseSize(resp *http.Response) int64 {
	if resp.StatusCode == http.StatusPartialContent {
		cr := resp.Header.Get("Content-Range")
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
				return n
			}
		}
		return -1
	}
	return resp.ContentLength
}
