package source

import (
	"errors"
	"net/url"
)

// SafeURL returns rawURL with any userinfo (username and/or password) and
// any query or fragment stripped, safe to fold into an error message, a log
// line, or a recorded manifest. Basic auth is normally attached out-of-band
// via creds.apply, never embedded in the URL itself, but this is defense in
// depth against a caller (or a redirect) that does embed one; the query is
// stripped too since a bearer token or signed-URL secret travels there just
// as often as in userinfo. If rawURL fails to parse, the raw string is not
// echoed back either — it could itself carry credentials — so a fixed
// placeholder is returned instead.
func SafeURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<unparseable url>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// unwrapURLErr strips the *url.Error that net/http (and libraries built on
// it, such as grab and httpreaderat) wrap transport failures in. That
// wrapper's Error() text embeds the request URL verbatim — net/http redacts
// only the password, not the username — so it is discarded in favor of the
// SafeURL rendering the caller supplies, keeping only the underlying cause
// (a dial failure, a TLS error, and so on), which is not URL-shaped and safe
// to surface as-is. err is returned unchanged if it wraps no *url.Error.
func unwrapURLErr(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}
