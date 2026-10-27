package serve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jamesbraid/instigator/internal/config"
)

// TestCheckShallowProbesRemoteSources: a reachable remote add-on passes the
// shallow check even though its bytes are no disc image, which only the full
// check reads; a missing one fails, named by its URL.
func TestCheckShallowProbesRemoteSources(t *testing.T) {
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rad4.image" {
			http.NotFound(w, r)
			return
		}
		if r.Method == http.MethodGet {
			gets.Add(1)
		}
		w.Header().Set("Content-Length", "8")
		w.Write([]byte("not efs!"))
	}))
	defer srv.Close()

	cfg := addonConfig(t)
	cfg.CacheDir = t.TempDir()
	cfg.InstallSets[len(cfg.InstallSets)-1].Layers = []config.Layer{{Name: "rad4", Source: srv.URL + "/rad4.image", Dist: "dist"}}
	if err := CheckShallow(cfg); err != nil {
		t.Fatalf("shallow check rejected a reachable source: %v", err)
	}
	if n := gets.Load(); n != 0 {
		t.Errorf("shallow check sent %d GETs", n)
	}
	if err := Check(cfg); err == nil {
		t.Fatal("full check accepted a source that is no disc image")
	}

	cfg.InstallSets[len(cfg.InstallSets)-1].Layers[0].Source = srv.URL + "/gone.image"
	if err := CheckShallow(cfg); err == nil || !strings.Contains(err.Error(), srv.URL+"/gone.image") {
		t.Fatalf("shallow check accepted or did not name a missing source: %v", err)
	}
}
