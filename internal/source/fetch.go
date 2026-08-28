package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// Object is the response metadata a probe captures for a remote object,
// used to key the cache and to decide whether a byte-range read is
// possible.
type Object struct {
	ETag          string
	LastModified  string
	ContentLength int64
	AcceptsRanges bool
}

// fetchWhole GETs url (credentials applied via creds.apply), streaming the
// response body to dstFile through a sha256 tee so the whole object is
// never held in memory. If sha256hex is non-empty, the streamed digest is
// compared against it (hex, case-insensitive) once the body is fully
// written; a mismatch is an error and dstFile is left for the caller to
// discard (the cache Install wrapper handles that). A non-2xx response is
// reported by status only — never by dumping the body or the request URL,
// which may carry credentials in its userinfo.
func fetchWhole(ctx context.Context, client *http.Client, rawURL string, creds Credentials, sha256hex string, dstFile string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("source: fetch %s: build request: %w", safeURL(rawURL), unwrapURLErr(err))
	}
	creds.apply(req)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("source: fetch %s: %w", safeURL(rawURL), unwrapURLErr(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("source: fetch: unexpected status %s", resp.Status)
	}

	f, err := os.OpenFile(dstFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("source: fetch: create %s: %w", dstFile, err)
	}
	defer f.Close()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hasher), resp.Body); err != nil {
		return fmt.Errorf("source: fetch: write %s: %w", dstFile, err)
	}

	if sha256hex != "" {
		got := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(got, sha256hex) {
			return fmt.Errorf("source: fetch: sha256 mismatch: got %s, want %s", got, sha256hex)
		}
	}

	return nil
}

// safeURL returns rawURL with any userinfo (username and/or password)
// stripped, safe to fold into an error message or a log line. Basic auth
// is normally attached out-of-band via creds.apply, never embedded in the
// URL itself, but this is defense in depth against a caller (or a
// redirect) that does embed one. If rawURL fails to parse, the raw string
// is not echoed back either — it could itself carry credentials — so a
// fixed placeholder is returned instead.
func safeURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<unparseable url>"
	}
	u.User = nil
	return u.String()
}

// unwrapURLErr strips the *url.Error net/http wraps request-build and
// transport errors in. That wrapper's own Error() text embeds the request
// URL verbatim — net/http redacts only the password, not the username, so
// it is not safe on its own — so the wrapper is discarded in favor of the
// safeURL rendering fetchWhole builds itself, keeping only the underlying
// cause (a parse complaint, a dial failure, and so on), which is not
// URL-shaped and safe to surface as-is. err is returned unchanged if it is
// not a *url.Error.
func unwrapURLErr(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}
