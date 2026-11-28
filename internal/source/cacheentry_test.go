package source

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// Creating an entry sweeps every kind a dead owner left, keeping a live one, a
// lockless or foreign one, and a flat fallback file.
func TestSweepRemovesAbandonedEntriesOfEveryKind(t *testing.T) {
	sweepAnyAge(t)
	cache := t.TempDir()
	mk := func(name string, lock bool) {
		if err := os.MkdirAll(filepath.Join(cache, name, "data"), 0o755); err != nil {
			t.Fatal(err)
		}
		if lock {
			if err := os.WriteFile(filepath.Join(cache, name, lockName), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, name := range []string{"extract-1", "fallback-2", "extract-live"} {
		mk(name, true)
	}
	mk("extract-legacy", false)
	// Another tool's directory, with a lock file of its own.
	mk("extract-other", false)
	if err := os.WriteFile(filepath.Join(cache, "extract-other", ".lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "fallback-flat"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	live := flock.New(filepath.Join(cache, "extract-live", lockName))
	if err := live.Lock(); err != nil {
		t.Fatal(err)
	}
	defer live.Unlock()

	e, err := newCacheEntry(cache, "extract", "tree")
	if err != nil {
		t.Fatal(err)
	}
	defer e.remove()
	<-startSweep(cache)
	ents, err := os.ReadDir(cache)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, ent := range ents {
		if ent.Name() != filepath.Base(e.dir) {
			left = append(left, ent.Name())
		}
	}
	if want := []string{"extract-legacy", "extract-live", "extract-other", "fallback-flat"}; !slices.Equal(left, want) {
		t.Fatalf("cache holds %v besides the new entry, want %v", left, want)
	}
}

// sweepAnyAge lets a sweep take an abandoned entry made moments ago.
func sweepAnyAge(t *testing.T) {
	swapSeam(t, &minSweepAge, 0)
}

// swapSeam sets *p to v for the test, once no background sweep can read it.
func swapSeam[T any](t *testing.T, p *T, v T) {
	sweepsRunning.Wait()
	saved := *p
	*p = v
	t.Cleanup(func() { sweepsRunning.Wait(); *p = saved })
}

// sweepNow runs a fresh sweep of cache to completion.
func sweepNow(cache string) {
	sweeps.Delete(filepath.Clean(cache))
	<-startSweep(filepath.Clean(cache))
}

// An unlocked lock file younger than minSweepAge may be one its creator is about
// to lock, so it is kept; once older it is swept.
func TestSweepKeepsYoungUnlockedEntry(t *testing.T) {
	cache := t.TempDir()
	dir := filepath.Join(cache, "extract-young")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, lockName), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sweepCache(cache)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("young entry was swept: %v", err)
	}
	old := time.Now().Add(-2 * minSweepAge)
	if err := os.Chtimes(filepath.Join(dir, lockName), old, old); err != nil {
		t.Fatal(err)
	}
	sweepCache(cache)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("old abandoned entry survived: %v", err)
	}
}

// Creating an entry never waits for the sweep, which may be deleting
// gigabytes while a stop is pending.
func TestNewCacheEntryDoesNotWaitForSweep(t *testing.T) {
	cache := t.TempDir()
	release := make(chan struct{})
	swapSeam(t, &testHook, func(point, _ string) {
		if point == "sweep-start" {
			<-release
		}
	})
	defer close(release)
	made := make(chan error, 1)
	go func() {
		e, err := newCacheEntry(cache, "extract", "tree")
		if err == nil {
			e.remove()
		}
		made <- err
	}()
	select {
	case err := <-made:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("newCacheEntry waited for the sweep")
	}
}
