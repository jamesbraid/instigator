package source

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/snabb/httpreaderat/pkg/contentrange"

	"github.com/jamesbraid/instigator/internal/logging"
)

// ErrChanged is returned when the remote object's size, ETag or Last-Modified
// differs from what the first response reported. It is never retried: serving
// an install from a file that changed mid-read would corrupt it silently.
var ErrChanged = errors.New("remote file changed while reading")

// ErrNoRange is returned when the server answers a range request with the
// whole file and there is nowhere to buffer it.
var ErrNoRange = errors.New("server does not support range requests")

var errTooManyRedirects = errors.New("stopped after 10 redirects")

// retryPolicy bounds how hard a range read fights a flaky server. Transient
// failures (see isTransient) are retried with exponential backoff and jitter;
// anything else fails at once.
type retryPolicy struct {
	// attempts is the total tries per read, the first included.
	attempts int
	// base and max bound the backoff: base, 2*base, ... capped at max.
	base, max time.Duration
	// budget caps one read's total time, waits included.
	budget time.Duration
	// idle is how long one request may go without receiving a byte, headers
	// included, before it is abandoned and retried.
	idle time.Duration
}

var defaultRetryPolicy = retryPolicy{
	attempts: 5,
	base:     500 * time.Millisecond,
	max:      8 * time.Second,
	budget:   5 * time.Minute,
	idle:     60 * time.Second,
}

// transientError marks a failure worth retrying. retryAfter, when positive, is
// the server's own request for how long to wait.
type transientError struct {
	err        error
	retryAfter time.Duration
}

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

func transient(err error) error { return &transientError{err: err} }

// statusError is an HTTP status that is not a success.
type statusError struct {
	status string
}

func (e *statusError) Error() string { return "http request error: " + e.status }

// rangeReader is an io.ReaderAt over an http(s) URL, fetched by byte range. A
// read that fails transiently is retried; a request that stalls is cut off. It
// replaces httpreaderat's reader, which fails a read on the first hiccup and
// leaves the caller (inst, in an install) to retry by hand. It is safe for
// concurrent use.
type rangeReader struct {
	ctx    context.Context
	client *http.Client
	req    *http.Request // prototype; cloned per request
	store  *cacheStore   // whole-file fallback for a server that ignores ranges
	pol    retryPolicy
	logger *logging.Logger
	url    string // SafeURL of req, for logs

	// Set once by newRangeReader, then read-only.
	size     int64
	etag     string
	modified string
	useStore bool
}

// newRangeReader probes the URL with a one-byte range request, retrying like
// any read. A server that ignores ranges is buffered whole into store.
func newRangeReader(ctx context.Context, client *http.Client, req *http.Request, store *cacheStore, pol retryPolicy, logger *logging.Logger) (*rangeReader, error) {
	rr := &rangeReader{
		ctx:    ctx,
		client: client,
		req:    req,
		store:  store,
		pol:    pol,
		logger: logger,
		url:    SafeURL(req.URL.String()),
	}
	_, err := rr.retry("bytes=0-0", func(ctx context.Context) (int, error) {
		return rr.get(ctx, make([]byte, 1), 0, true)
	})
	if err != nil {
		return nil, err
	}
	return rr, nil
}

// Size is the object's length.
func (rr *rangeReader) Size() int64 { return rr.size }

// ReadAt implements io.ReaderAt: always a non-nil error when n < len(p), io.EOF
// at the end of the object.
func (rr *rangeReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if rr.useStore {
		return rr.store.ReadAt(p, off)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= rr.size {
		return 0, io.EOF
	}
	var tail error
	if off+int64(len(p)) > rr.size {
		// Some servers answer a range past the end with 416; ask only for
		// what exists.
		p = p[:rr.size-off]
		tail = io.EOF
	}
	spec := fmt.Sprintf("bytes=%d-%d", off, off+int64(len(p))-1)
	n, err := rr.retry(spec, func(ctx context.Context) (int, error) {
		return rr.get(ctx, p, off, false)
	})
	if err != nil {
		return n, err
	}
	return n, tail
}

// retry runs op until it succeeds, fails permanently, or the policy runs out.
// spec names the range in log lines.
func (rr *rangeReader) retry(spec string, op func(context.Context) (int, error)) (int, error) {
	ctx, cancel := context.WithTimeout(rr.ctx, rr.pol.budget)
	defer cancel()
	for attempt := 1; ; attempt++ {
		n, err := op(ctx)
		if err == nil {
			return n, nil
		}
		var te *transientError
		if !errors.As(err, &te) {
			return n, err
		}
		if attempt >= rr.pol.attempts || ctx.Err() != nil {
			return n, fmt.Errorf("giving up on %s after %d attempts: %w", spec, attempt, te.err)
		}
		delay := rr.backoff(attempt, te.retryAfter)
		rr.logger.Infof("source: %s %s: attempt %d of %d failed, retrying in %s: %v",
			rr.url, spec, attempt, rr.pol.attempts, delay.Round(time.Millisecond), te.err)
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return n, fmt.Errorf("giving up on %s after %d attempts: %w", spec, attempt, te.err)
		}
	}
}

// backoff is base*2^(attempt-1) capped at max, jittered down by up to half so
// two readers hitting the same hiccup do not retry in step. The server's
// Retry-After wins when longer, up to max.
func (rr *rangeReader) backoff(attempt int, retryAfter time.Duration) time.Duration {
	d := rr.pol.base
	for i := 1; i < attempt && d < rr.pol.max; i++ {
		d *= 2
	}
	d = min(d, rr.pol.max)
	d -= time.Duration(rand.Int64N(int64(d)/2 + 1))
	return max(d, min(retryAfter, rr.pol.max))
}

// get makes one range request for len(p) bytes at first and reads the body
// into p. With initialize it also records the object's identity, and accepts a
// plain 200 by buffering the whole file into the store. Failures are wrapped in
// transientError when a retry may help.
func (rr *rangeReader) get(ctx context.Context, p []byte, first int64, initialize bool) (int, error) {
	last := first + int64(len(p)) - 1
	idle := newIdleWatch(ctx, rr.pol.idle)
	defer idle.stop()

	req := rr.req.Clone(idle.ctx)
	req.Body = nil
	req.ContentLength = 0
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", first, last))

	resp, err := rr.client.Do(req)
	if err != nil {
		return 0, rr.failure(idle, err, true)
	}
	defer resp.Body.Close()

	if initialize && resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		f, l, size, perr := contentrange.Parse(resp.Header.Get("Content-Range"))
		if perr == nil && f == -1 && l == -1 && size == 0 {
			rr.remember(resp, 0)
			return 0, nil
		}
	}
	switch {
	case resp.StatusCode == http.StatusOK && initialize:
		return rr.fallback(resp, idle)
	case resp.StatusCode == http.StatusOK:
		// The probe got a 206, so this is the server flaking, not a policy.
		return 0, transient(errors.New("server answered 200 to a range request it served as 206 before"))
	case resp.StatusCode != http.StatusPartialContent:
		return 0, statusFailure(resp)
	}

	f, l, size, perr := contentrange.Parse(resp.Header.Get("Content-Range"))
	if perr != nil {
		return 0, fmt.Errorf("http request: bad Content-Range %q: %w", resp.Header.Get("Content-Range"), perr)
	}
	if size < 0 {
		return 0, fmt.Errorf("http request: Content-Range %q gives no size", resp.Header.Get("Content-Range"))
	}
	if initialize {
		rr.remember(resp, size)
	} else if err := rr.validate(resp, size); err != nil {
		return 0, err
	}
	if f != first || l > last {
		return 0, fmt.Errorf("received different range than requested (req=%d-%d, resp=%d-%d)", first, last, f, l)
	}
	if l < last {
		return 0, transient(fmt.Errorf("server returned %d-%d for a request of %d-%d", f, l, first, last))
	}
	n, err := io.ReadFull(idle.reader(resp.Body), p)
	if err != nil {
		return n, rr.failure(idle, fmt.Errorf("reading body: %w", err), false)
	}
	return n, nil
}

// fallback buffers a whole-file 200 into the store, for a server that ignores
// ranges. A body shorter than its Content-Length is a transient failure.
func (rr *rangeReader) fallback(resp *http.Response, idle *idleWatch) (int, error) {
	if rr.store == nil {
		return 0, ErrNoRange
	}
	n, err := rr.store.ReadFrom(idle.reader(resp.Body))
	if err != nil {
		return 0, rr.failure(idle, fmt.Errorf("reading body: %w", err), false)
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return 0, transient(fmt.Errorf("reading body: %w", io.ErrUnexpectedEOF))
	}
	rr.remember(resp, n)
	rr.useStore = true
	return 0, nil
}

func (rr *rangeReader) remember(resp *http.Response, size int64) {
	rr.size = size
	rr.etag = resp.Header.Get("ETag")
	rr.modified = resp.Header.Get("Last-Modified")
}

func (rr *rangeReader) validate(resp *http.Response, size int64) error {
	switch {
	case size != rr.size:
		return fmt.Errorf("%w: size %d, was %d", ErrChanged, size, rr.size)
	case resp.Header.Get("ETag") != rr.etag:
		return fmt.Errorf("%w: ETag %q, was %q", ErrChanged, resp.Header.Get("ETag"), rr.etag)
	case resp.Header.Get("Last-Modified") != rr.modified:
		return fmt.Errorf("%w: Last-Modified %q, was %q", ErrChanged, resp.Header.Get("Last-Modified"), rr.modified)
	}
	return nil
}

// statusFailure turns a non-success status into an error, transient for the
// ones that say "try again": 408, 429 and 5xx. Every other status, 4xx above
// all, is final.
func statusFailure(resp *http.Response) error {
	err := &statusError{status: resp.Status}
	switch {
	case resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode >= 500:
		te := &transientError{err: err}
		if s, perr := strconv.Atoi(resp.Header.Get("Retry-After")); perr == nil && s > 0 {
			te.retryAfter = time.Duration(s) * time.Second
		}
		return te
	}
	return err
}

// failure classifies an error from a request or its body. The caller's own
// cancellation and a changed certificate chain are final; a stall, reset,
// timeout or truncated body is worth another go. A failed Do is a url.Error
// whose text would carry the full URL, so it is stripped.
func (rr *rangeReader) failure(idle *idleWatch, err error, fromDo bool) error {
	if fromDo {
		err = fmt.Errorf("http request: %w", unwrapURLErr(err))
	}
	if rr.ctx.Err() != nil {
		return err
	}
	if idle.fired.Load() {
		return transient(fmt.Errorf("no data for %s: %w", rr.pol.idle, err))
	}
	if fromDo {
		var (
			verify  *tls.CertificateVerificationError
			unknown x509.UnknownAuthorityError
			host    x509.HostnameError
			invalid x509.CertificateInvalidError
			dns     *net.DNSError
		)
		switch {
		case errors.Is(err, errTooManyRedirects),
			errors.As(err, &verify), errors.As(err, &unknown),
			errors.As(err, &host), errors.As(err, &invalid),
			errors.As(err, &dns) && dns.IsNotFound:
			return err
		}
	}
	return transient(err)
}

// idleWatch cancels a request that has gone idle for d without a byte
// arriving. Time spent waiting for headers counts. It is cheaper than a
// per-read deadline and covers HTTP/2, where the transport owns the conn.
type idleWatch struct {
	ctx    context.Context
	cancel context.CancelFunc
	timer  *time.Timer
	d      time.Duration
	fired  atomic.Bool
}

func newIdleWatch(parent context.Context, d time.Duration) *idleWatch {
	w := &idleWatch{d: d}
	w.ctx, w.cancel = context.WithCancel(parent)
	w.timer = time.AfterFunc(d, func() {
		w.fired.Store(true)
		w.cancel()
	})
	return w
}

func (w *idleWatch) stop() {
	w.timer.Stop()
	w.cancel()
}

// reader wraps body so each byte received pushes the deadline out.
func (w *idleWatch) reader(body io.Reader) io.Reader { return idleReader{body, w} }

type idleReader struct {
	r io.Reader
	w *idleWatch
}

func (r idleReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		r.w.timer.Reset(r.w.d)
	}
	return n, err
}
