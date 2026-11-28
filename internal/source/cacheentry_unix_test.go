//go:build unix

package source

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// TestHoldExtraction is the child process for the sweep tests: it resolves
// an archive, prints the extraction directory and holds it until stdin closes.
func TestHoldExtraction(t *testing.T) {
	spec := os.Getenv("INSTIGATOR_TEST_HOLD_EXTRACT")
	if spec == "" {
		t.Skip("child process only")
	}
	cache, archive, _ := strings.Cut(spec, "|")
	minSweepAge = 0
	res, err := New(Options{CacheDir: cache}).Resolve(archive)
	if err != nil {
		t.Fatal(err)
	}
	<-startSweep(cache)
	liveEntries.Range(func(dir, _ any) bool {
		fmt.Println(dir)
		return false
	})
	bufio.NewReader(os.Stdin).ReadString('\n')
	res.Closer.Close()
}

type holder struct {
	cmd   *exec.Cmd
	stdin interface{ Close() error }
	dir   string
}

func holdExtraction(t *testing.T, cache, archive string) *holder {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHoldExtraction$")
	cmd.Env = append(os.Environ(), "INSTIGATOR_TEST_HOLD_EXTRACT="+cache+"|"+archive)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("child: %v", err)
	}
	return &holder{cmd: cmd, stdin: stdin, dir: strings.TrimSpace(line)}
}

func writeArchive(t *testing.T, cache string) string {
	t.Helper()
	archive := filepath.Join(cache, "set.tar.gz")
	if err := os.WriteFile(archive, tgz(t, map[string]string{"dist/a": "a"}), 0o644); err != nil {
		t.Fatal(err)
	}
	return archive
}

func TestExtractSweepsDeadOwner(t *testing.T) {
	sweepAnyAge(t)
	cache := t.TempDir()
	archive := writeArchive(t, cache)
	legacy := filepath.Join(cache, "extract-unlocked")
	if err := os.Mkdir(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	h := holdExtraction(t, cache, archive)
	h.cmd.Process.Kill()
	h.cmd.Wait()
	if _, err := os.Stat(h.dir); err != nil {
		t.Fatalf("killed child left no extraction: %v", err)
	}

	res, err := New(Options{CacheDir: cache}).Resolve(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Closer.Close()
	<-startSweep(cache)
	if _, err := os.Stat(h.dir); !os.IsNotExist(err) {
		t.Fatalf("dead owner's extraction survived: %v", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Fatalf("unlocked extraction was removed: %v", err)
	}
}

func TestExtractKeepsLiveOwner(t *testing.T) {
	cache := t.TempDir()
	archive := writeArchive(t, cache)
	h := holdExtraction(t, cache, archive)

	res, err := New(Options{CacheDir: cache}).Resolve(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Closer.Close()
	if _, err := os.Stat(filepath.Join(h.dir, "tree", "dist", "a")); err != nil {
		t.Fatalf("live owner's extraction was removed: %v", err)
	}
	h.stdin.Close()
	if err := h.cmd.Wait(); err != nil {
		t.Fatalf("child: %v", err)
	}
}

func TestConcurrentExtractsKeepEachOther(t *testing.T) {
	cache := t.TempDir()
	archive := writeArchive(t, cache)
	holders := make([]*holder, 8)
	var wg sync.WaitGroup
	for i := range holders {
		wg.Go(func() { holders[i] = holdExtraction(t, cache, archive) })
	}
	wg.Wait()
	for i, h := range holders {
		if _, err := os.Stat(filepath.Join(h.dir, "tree", "dist", "a")); err != nil {
			t.Fatalf("extraction %d was removed: %v", i, err)
		}
	}
}

// posixLock is what flock becomes on NFS: a per-process lock that a process
// can retake, and that closing any of its descriptors for the file drops.
func posixLock(path string) (fileLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if err := syscall.FcntlFlock(f.Fd(), syscall.F_SETLK, &syscall.Flock_t{Type: syscall.F_WRLCK, Whence: io.SeekStart}); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func setHook(t *testing.T, hook func(point, dir string)) {
	swapSeam(t, &testHook, hook)
}

func TestFailedRemovalKeepsLock(t *testing.T) {
	sweepAnyAge(t)
	cache := t.TempDir()
	archive := writeArchive(t, cache)
	res, err := New(Options{CacheDir: cache}).Resolve(archive)
	if err != nil {
		t.Fatal(err)
	}
	var dir string
	liveEntries.Range(func(d, _ any) bool { dir = d.(string); return false })
	sweepsRunning.Wait()
	removeAll = func(path string) error {
		if filepath.Base(path) == "tree" {
			return errors.New("tree not removable")
		}
		return os.RemoveAll(path)
	}
	err = res.Closer.Close()
	removeAll = os.RemoveAll
	if err == nil {
		t.Fatal("failed tree removal reported success")
	}
	if _, err := os.Stat(filepath.Join(dir, lockName)); err != nil {
		t.Fatalf("failed removal lost the lock file: %v", err)
	}

	sweepNow(cache)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("failed removal was not swept: %v", err)
	}
}

func TestPOSIXLocksKeepOwnExtractions(t *testing.T) {
	swapSeam(t, &tryLock, posixLock)
	cache := t.TempDir()
	archive := writeArchive(t, cache)
	results := make([]fs.FS, 8)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			res, err := New(Options{CacheDir: cache}).Resolve(archive)
			if err != nil {
				t.Error(err)
				return
			}
			t.Cleanup(func() { res.Closer.Close() })
			results[i] = res.FS
		})
	}
	wg.Wait()
	for i, fsys := range results {
		if fsys == nil {
			t.FailNow()
		}
		if _, err := fs.ReadFile(fsys, "dist/a"); err != nil {
			t.Fatalf("extraction %d was removed: %v", i, err)
		}
	}
}

// A process sweeping while the lock file exists but is not yet locked must
// not find it.
func TestSweepNeverSeesUnlockedLock(t *testing.T) {
	sweepAnyAge(t)
	cache := t.TempDir()
	archive := writeArchive(t, cache)
	setHook(t, func(point, _ string) {
		if point == "lock-created" {
			h := holdExtraction(t, cache, archive)
			h.stdin.Close()
			h.cmd.Wait()
		}
	})
	res, err := New(Options{CacheDir: cache}).Resolve(archive)
	if err != nil {
		t.Fatalf("extraction removed while it was locked: %v", err)
	}
	defer res.Closer.Close()
	if _, err := fs.ReadFile(res.FS, "dist/a"); err != nil {
		t.Fatalf("extraction was removed: %v", err)
	}
}

// A sweeper that locked a dead extraction must not remove a new one that
// took its name in the meantime.
func TestSweepKeepsReplacedExtraction(t *testing.T) {
	sweepAnyAge(t)
	cache := t.TempDir()
	archive := writeArchive(t, cache)
	h := holdExtraction(t, cache, archive)
	h.cmd.Process.Kill()
	h.cmd.Wait()
	var replaced fileLock
	var removeErr error
	setHook(t, func(point, dir string) {
		if point != "sweep-locked" || dir != h.dir {
			return
		}
		if removeErr = os.RemoveAll(dir); removeErr != nil {
			return
		}
		os.Mkdir(dir, 0o755)
		if err := os.WriteFile(filepath.Join(dir, lockName), nil, 0o600); err != nil {
			t.Errorf("replace extraction: %v", err)
		}
		l, err := tryLock(filepath.Join(dir, lockName))
		if err != nil {
			t.Errorf("replace extraction: %v", err)
		}
		replaced = l
	})
	res, err := New(Options{CacheDir: cache}).Resolve(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Closer.Close()
	<-startSweep(cache)
	if removeErr != nil {
		// NFS cannot remove the directory while the sweeper holds its lock
		// file open, so the name cannot be reused there.
		t.Skipf("replace extraction: %v", removeErr)
	}
	if replaced == nil {
		t.Fatal("the sweep never locked the dead extraction")
	}
	defer replaced.Close()
	if _, err := os.Stat(filepath.Join(h.dir, lockName)); err != nil {
		t.Fatalf("replacement extraction was removed: %v", err)
	}
}

// With a relative cache dir the registry must use the sweep's own paths, or
// on NFS a run sweeps its own live entry.
func TestPOSIXLocksKeepOwnEntryInRelativeCacheDir(t *testing.T) {
	sweepAnyAge(t)
	swapSeam(t, &tryLock, posixLock)
	t.Chdir(t.TempDir())
	if err := os.Mkdir("x", 0o755); err != nil {
		t.Fatal(err)
	}
	e, err := newCacheEntry("./x", "extract", "tree")
	if err != nil {
		t.Fatal(err)
	}
	defer e.remove()
	sweepNow("./x")
	if _, err := os.Stat(filepath.Join(e.dir, lockName)); err != nil {
		t.Fatalf("own live entry was swept: %v", err)
	}
}
