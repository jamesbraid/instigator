package source

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestProbe: a probe answers from HEAD, or from a one-byte ranged GET when
// HEAD is refused, and never asks for the whole body.
func TestProbe(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 1<<20)
	var mu sync.Mutex
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path+" "+r.Header.Get("Range"))
		mu.Unlock()
		switch {
		case r.URL.Path == "/missing":
			http.NotFound(w, r)
		case r.URL.Path == "/nohead" && r.Method == http.MethodHead:
			w.WriteHeader(http.StatusMethodNotAllowed)
		default:
			http.ServeContent(w, r, "disc", time.Time{}, bytes.NewReader(body))
		}
	}))
	defer srv.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	unreachable := closed.URL + "/disc"
	closed.Close()

	r := New(Options{CacheDir: t.TempDir(), Client: srv.Client()})
	for _, tc := range []struct {
		name, url, wantErr string
		wantRequests       []string
	}{
		{name: "ok", url: srv.URL + "/disc", wantRequests: []string{"HEAD /disc "}},
		{name: "head refused", url: srv.URL + "/nohead", wantRequests: []string{"HEAD /nohead ", "GET /nohead bytes=0-0"}},
		{name: "missing", url: srv.URL + "/missing", wantErr: "404"},
		{name: "unreachable", url: unreachable, wantErr: "connect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			requests = nil
			mu.Unlock()
			size, err := r.Probe(context.Background(), tc.url)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), tc.url) {
					t.Fatalf("err = %v, want %q naming %s", err, tc.wantErr, tc.url)
				}
				return
			}
			if err != nil || size != int64(len(body)) {
				t.Fatalf("size=%d err=%v", size, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if strings.Join(requests, "|") != strings.Join(tc.wantRequests, "|") {
				t.Errorf("requests = %q, want %q", requests, tc.wantRequests)
			}
		})
	}
}

// TestProbeRedactsURL: an error never echoes credentials or a query.
func TestProbeRedactsURL(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	ref := strings.Replace(srv.URL, "http://", "http://user:secret@", 1) + "/disc?token=hidden"
	_, err := New(Options{CacheDir: t.TempDir(), Client: srv.Client()}).Probe(context.Background(), ref)
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "hidden") {
		t.Fatalf("err = %v", err)
	}
}
