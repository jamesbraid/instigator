package source

import (
	"errors"
	"net/url"
)

// SafeURL strips a URL's userinfo, query, and fragment - any of which can
// carry a credential - so it is safe to log, record, or fold into an error.
// An unparseable URL returns a placeholder rather than being echoed back.
func SafeURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "<unparseable url>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// unwrapURLErr discards a *url.Error wrapper, whose Error() text embeds the
// request URL, returning the URL-free cause; a non-url.Error passes through.
func unwrapURLErr(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return uerr.Err
	}
	return err
}
