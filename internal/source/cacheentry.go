package source

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

// lockName is the file inside a cache entry whose exclusive lock shows its
// owner is alive, named so a sweep never takes another tool's directory.
const lockName = ".instigator-lock"

// entryKinds prefix the temporary entries at the cache root that are swept.
var entryKinds = []string{"extract-", "fallback-", "download-"}

// liveEntries maps this process's entries to their locks. On NFS flock is a
// per-process lock, which would not stop a process sweeping its own.
var liveEntries sync.Map

// sweeps maps each cleaned cache dir to a channel closed when its one sweep
// in this process ends.
var sweeps sync.Map

// sweepsRunning lets tests wait out background sweeps before swapping seams.
var sweepsRunning sync.WaitGroup

// minSweepAge is how old an unlocked lock file must be before a sweep takes it.
// Tests shorten it.
var minSweepAge = time.Minute

// removeAll is swapped by tests to make a removal fail.
var removeAll = os.RemoveAll

// testHook lets tests act where another process could interleave.
var testHook = func(point, dir string) {}

// fileLock is an exclusive lock on one file, held until Close.
type fileLock interface {
	Stat() (fs.FileInfo, error)
	Close() error
}

var errLocked = errors.New("locked by another owner")

// tryLock takes the existing file's exclusive lock without waiting. Tests swap
// in POSIX locks, which is what flock becomes on NFS.
var tryLock = func(path string) (fileLock, error) {
	l := flock.New(path, flock.SetFlag(os.O_RDWR))
	ok, err := l.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errLocked
	}
	return l, nil
}

// cacheEntry is a temporary extract-*, fallback-* or download-* directory at
// the cache root, holding its lock file and the payload at name.
type cacheEntry struct {
	dir, name string
}

// newCacheEntry creates a locked kind-* directory in cacheDir for a payload at
// name, registered before its lock file exists so our own sweep skips it.
func newCacheEntry(cacheDir, kind, payload string) (*cacheEntry, error) {
	if cacheDir == "" {
		cacheDir = os.TempDir()
	}
	cacheDir = filepath.Clean(cacheDir)
	startSweep(cacheDir)
	made, err := os.MkdirTemp(cacheDir, kind+"-*")
	if err != nil {
		return nil, err
	}
	// The key must match the sweep's own join of the cache dir and name.
	dir := filepath.Join(cacheDir, filepath.Base(made))
	liveEntries.Store(dir, nil)
	liveEntries.Store(dir, lockEntry(dir))
	return &cacheEntry{dir: dir, name: filepath.Join(dir, payload)}, nil
}

// startSweep sweeps cacheDir once per process, in the background so removing
// gigabytes never delays an entry or a stop. The channel closes when it ends.
func startSweep(cacheDir string) <-chan struct{} {
	done := make(chan struct{})
	if prev, ok := sweeps.LoadOrStore(cacheDir, done); ok {
		return prev.(chan struct{})
	}
	sweepsRunning.Add(1)
	go func() {
		defer sweepsRunning.Done()
		defer close(done)
		sweepCache(cacheDir)
	}()
	return done
}

// lockEntry creates dir's lock file and locks it. Where locking fails, nil: the
// entry is never swept.
func lockEntry(dir string) fileLock {
	final := filepath.Join(dir, lockName)
	if runtime.GOOS == "windows" {
		// Windows cannot rename an open file, so lock in place; the sweep's age
		// guard covers the moment before the lock is taken.
		f, err := os.OpenFile(final, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil
		}
		f.Close()
		return lockOrRemove(dir, final)
	}
	// Lock under a temporary name and rename, so a sweep never sees the lock file
	// unlocked.
	f, err := os.CreateTemp(dir, lockName+"-*")
	if err != nil {
		return nil
	}
	tmp := f.Name()
	f.Close()
	l := lockOrRemove(dir, tmp)
	if l != nil && os.Rename(tmp, final) != nil {
		l.Close()
		os.Remove(tmp)
		return nil
	}
	return l
}

// lockOrRemove locks path, removing it if that fails.
func lockOrRemove(dir, path string) fileLock {
	testHook("lock-created", dir)
	l, err := tryLock(path)
	if err != nil {
		os.Remove(path)
		return nil
	}
	return l
}

// remove deletes this process's entry.
func (e *cacheEntry) remove() error {
	return removeEntry(e.dir, func() {
		if lock, ok := liveEntries.LoadAndDelete(e.dir); ok && lock != nil {
			lock.(fileLock).Close()
		}
	})
}

// removeEntry deletes dir's payload, and only then its lock file, so a failed
// removal stays sweepable. NFS keeps open files, so unlock comes first.
func removeEntry(dir string, unlock func()) error {
	ents, err := os.ReadDir(dir)
	for _, ent := range ents {
		if ent.Name() != lockName {
			err = errors.Join(err, removeAll(filepath.Join(dir, ent.Name())))
		}
	}
	unlock()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return removeAll(dir)
}

// sweepCache removes entries whose owner has exited, keeping any it cannot lock
// and any with no lock file: those predate the locks and may still be in use.
func sweepCache(cacheDir string) {
	testHook("sweep-start", cacheDir)
	ents, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	for _, ent := range ents {
		if !ent.IsDir() || !isEntryName(ent.Name()) {
			continue
		}
		dir := filepath.Join(cacheDir, ent.Name())
		if _, live := liveEntries.Load(dir); !live {
			removeAbandoned(dir)
		}
	}
}

func isEntryName(name string) bool {
	for _, k := range entryKinds {
		if strings.HasPrefix(name, k) {
			return true
		}
	}
	return false
}

func removeAbandoned(dir string) {
	path := filepath.Join(dir, lockName)
	// A creator locks its lock file within minSweepAge of making it, so an
	// unlocked one that young may be about to be locked: leave it for later.
	if fi, err := os.Stat(path); err != nil || time.Since(fi.ModTime()) < minSweepAge {
		return
	}
	lock, err := tryLock(path)
	if err != nil {
		return
	}
	// Another sweeper may have removed the directory, and a new owner
	// reused its name, since this one opened the lock.
	testHook("sweep-locked", dir)
	held, err1 := lock.Stat()
	now, err2 := os.Stat(path)
	if err1 != nil || err2 != nil || !os.SameFile(held, now) {
		lock.Close()
		return
	}
	removeEntry(dir, func() { lock.Close() })
}
