package sharefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jamesbraid/instigator/nfs"
)

// newShare makes dir/share holding one file and dir/outside, which
// nothing the share does may touch.
func newShare(t *testing.T, opts Options) (*FS, string) {
	t.Helper()
	base := t.TempDir()
	share := filepath.Join(base, "share")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{share, filepath.Join(outside, "inner")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share, "f"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if opts.PathTable == "" && !inodesUsable {
		opts.PathTable = filepath.Join(base, "handles")
	}
	f, err := Open("s", share, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f, outside
}

func root(t *testing.T, f *FS) nfs.Node {
	t.Helper()
	n, err := f.MountRoot("/s")
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func lookup(t *testing.T, f *FS, dir nfs.Node, name string) nfs.Node {
	t.Helper()
	n, err := f.Lookup(dir, name)
	if err != nil {
		t.Fatalf("Lookup(%q): %v", name, err)
	}
	return n
}

func wantErr(t *testing.T, what string, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("%s: %v, want %v", what, got, want)
	}
}

// outsideUntouched fails if anything was created in or changed under
// the outside directory.
func outsideUntouched(t *testing.T, outside string) {
	t.Helper()
	ents, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 2 {
		t.Fatalf("outside holds %d entries, want inner and secret", len(ents))
	}
	if ents, _ := os.ReadDir(filepath.Join(outside, "inner")); len(ents) != 0 {
		t.Fatalf("outside/inner gained %s", ents[0].Name())
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "secret")); string(b) != "secret" {
		t.Fatalf("outside/secret = %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(outside, "secret")); fi.Mode().Perm() != 0o644 {
		t.Fatalf("outside/secret mode = %v", fi.Mode())
	}
}

func TestNamesCannotLeaveTheShare(t *testing.T) {
	f, outside := newShare(t, Options{})
	r := root(t, f)

	if up := lookup(t, f, r, ".."); up.(*node).path != "." {
		t.Fatalf(`".." of the root is %q, want the root`, up.(*node).path)
	}
	for _, name := range []string{"../outside", "../outside/secret", "a/b", ""} {
		if _, err := f.Lookup(r, name); !errors.Is(err, nfs.ErrNotFound) {
			t.Fatalf("Lookup(%q) = %v, want ErrNotFound", name, err)
		}
	}
	for _, name := range []string{"..", ".", "../escape", "a/b", ""} {
		_, err := f.Create(r, name, nfs.Unchecked, nfs.SetAttr{}, [8]byte{})
		wantErr(t, "Create "+name, err, nfs.ErrInval)
		_, err = f.Mkdir(r, name, nfs.SetAttr{})
		wantErr(t, "Mkdir "+name, err, nfs.ErrInval)
		wantErr(t, "Rename to "+name, f.Rename(r, "f", r, name), nfs.ErrInval)
		wantErr(t, "Remove "+name, f.Remove(r, name), nfs.ErrInval)
	}
	if _, err := f.MountRoot("/s/../outside"); err == nil {
		t.Fatal("MountRoot through .. succeeded")
	}
	if _, err := f.MountRoot("/other"); !errors.Is(err, nfs.ErrNotFound) {
		t.Fatalf("MountRoot of another name: %v", err)
	}
	outsideUntouched(t, outside)
}

// A symlink in the share that leads out is served as a link; nothing
// the server does follows it.
func TestSymlinkOutIsNeverFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	f, outside := newShare(t, Options{})
	share := f.Dir()
	if err := os.Symlink(outside, filepath.Join(share, "dir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(share, "file")); err != nil {
		t.Fatal(err)
	}
	r := root(t, f)
	dir := lookup(t, f, r, "dir")
	file := lookup(t, f, r, "file")
	if dir.Mode()&0o170000 != 0o120000 || file.Mode()&0o170000 != 0o120000 {
		t.Fatalf("links served as modes %#o and %#o", dir.Mode(), file.Mode())
	}
	if got, err := f.Readlink(dir); err != nil || got != filepath.ToSlash(outside) {
		t.Fatalf("Readlink = %q, %v", got, err)
	}

	_, err := f.Lookup(dir, "secret")
	wantErr(t, "Lookup through a link", err, nfs.ErrNotDir)
	_, err = f.Create(dir, "x", nfs.Unchecked, nfs.SetAttr{}, [8]byte{})
	wantErr(t, "Create through a link", err, nfs.ErrNotDir)
	_, err = f.ReadDir(dir)
	wantErr(t, "ReadDir of a link", err, nfs.ErrNotDir)
	_, err = f.ReadAt(file, make([]byte, 8), 0)
	wantErr(t, "ReadAt of a link", err, nfs.ErrInval)
	wantErr(t, "WriteAt to a link", f.WriteAt(file, []byte("x"), 0), nfs.ErrInval)
	size := uint64(0)
	wantErr(t, "truncating a link", f.SetAttr(file, nfs.SetAttr{Size: &size}), nfs.ErrInval)
	mode := uint32(0o777)
	if err := f.SetAttr(file, nfs.SetAttr{Mode: &mode}); err != nil {
		t.Fatalf("chmod of a link: %v", err)
	}
	wantErr(t, "Rmdir of a link", f.Rmdir(r, "dir"), nfs.ErrNotDir)
	if err := f.Remove(r, "file"); err != nil {
		t.Fatalf("Remove of a link: %v", err)
	}
	outsideUntouched(t, outside)
}

// A directory the client holds a handle under, replaced on the host by
// a link out of the share, still can't be written through.
func TestSwappedDirectoryCannotEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	for _, opts := range []Options{{}, {PathTable: filepath.Join(t.TempDir(), "handles")}} {
		f, outside := newShare(t, opts)
		r := root(t, f)
		sub, err := f.Mkdir(r, "sub", nfs.SetAttr{})
		if err != nil {
			t.Fatal(err)
		}
		inner, err := f.Mkdir(sub, "inner", nfs.SetAttr{})
		if err != nil {
			t.Fatal(err)
		}
		g, err := f.Create(inner, "g", nfs.Unchecked, nfs.SetAttr{}, [8]byte{})
		if err != nil {
			t.Fatal(err)
		}
		share := f.Dir()
		if err := os.Rename(filepath.Join(share, "sub"), filepath.Join(share, "moved")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(share, "sub")); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Create(inner, "x", nfs.Unchecked, nfs.SetAttr{}, [8]byte{}); err == nil {
			t.Fatal("Create under a swapped directory succeeded")
		}
		if _, err := f.Mkdir(inner, "x", nfs.SetAttr{}); err == nil {
			t.Fatal("Mkdir under a swapped directory succeeded")
		}
		f.WriteAt(g, []byte("x"), 0)
		f.Rename(inner, "g", r, "stolen")
		outsideUntouched(t, outside)
	}
}

func TestGuestSymlinksStayText(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	f, outside := newShare(t, Options{})
	r := root(t, f)
	for name, target := range map[string]string{"abs": "/etc", "up": "../outside", "deep": "../../../../outside/secret"} {
		n, err := f.Symlink(r, name, target, nfs.SetAttr{})
		if err != nil {
			t.Fatalf("Symlink %s: %v", name, err)
		}
		if got, _ := f.Readlink(n); got != target {
			t.Fatalf("Readlink %s = %q, want %q", name, got, target)
		}
		_, err = f.Create(n, "x", nfs.Unchecked, nfs.SetAttr{}, [8]byte{})
		wantErr(t, "Create in "+name, err, nfs.ErrNotDir)
		wantErr(t, "WriteAt "+name, f.WriteAt(n, []byte("x"), 0), nfs.ErrInval)
	}
	_, err := f.Symlink(r, "empty", "", nfs.SetAttr{})
	wantErr(t, "empty target", err, nfs.ErrInval)
	outsideUntouched(t, outside)
}

func hostMode(t *testing.T, f *FS, name string) fs.FileMode {
	t.Helper()
	fi, err := os.Lstat(filepath.Join(f.Dir(), name))
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode()
}

func TestSetuidIsStripped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no setuid on Windows")
	}
	f, _ := newShare(t, Options{})
	r := root(t, f)
	mode := uint32(0o4755)
	n, err := f.Create(r, "su", nfs.Guarded, nfs.SetAttr{Mode: &mode}, [8]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if got := hostMode(t, f, "su"); got != 0o755 {
		t.Fatalf("created with setuid asked: host mode %v, want -rwxr-xr-x", got)
	}
	if err := f.SetAttr(n, nfs.SetAttr{Mode: &mode}); err != nil {
		t.Fatal(err)
	}
	if got := hostMode(t, f, "su"); got != 0o755 {
		t.Fatalf("chmod 4755: host mode %v, want -rwxr-xr-x", got)
	}

	// One set on the host is hidden, then cleared by a write, with
	// setgid on a group-executable file.
	path := filepath.Join(f.Dir(), "su")
	if err := os.Chmod(path, 0o755|fs.ModeSetuid|fs.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	n = lookup(t, f, r, "su")
	if n.Mode()&0o4000 != 0 || n.Mode()&0o2000 == 0 {
		t.Fatalf("served mode %#o, want setgid shown and setuid hidden", n.Mode())
	}
	if err := f.WriteAt(n, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	if got := hostMode(t, f, "su"); got != 0o755 {
		t.Fatalf("after a write: host mode %v, want -rwxr-xr-x", got)
	}

	// A setgid directory keeps it: that bit means group inheritance.
	dmode := uint32(0o2775)
	if _, err := f.Mkdir(r, "d", nfs.SetAttr{Mode: &dmode}); err != nil {
		t.Fatal(err)
	}
	if got := hostMode(t, f, "d"); got != fs.ModeDir|fs.ModeSetgid|0o775 {
		t.Fatalf("setgid directory: host mode %v", got)
	}
}

// The mode a client asks for is the mode the file gets, whatever the
// server's umask.
func TestCreateModeIgnoresUmask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps only a read-only bit")
	}
	f, _ := newShare(t, Options{})
	mode := uint32(0o666)
	if _, err := f.Create(root(t, f), "g", nfs.Guarded, nfs.SetAttr{Mode: &mode}, [8]byte{}); err != nil {
		t.Fatal(err)
	}
	if got := hostMode(t, f, "g"); got != 0o666 {
		t.Fatalf("host mode %v, want -rw-rw-rw-", got)
	}
}

func handleOf(t *testing.T, f *FS, n nfs.Node) []byte {
	fh := f.Handle(n)
	if len(fh) != 32 {
		t.Fatalf("handle is %d bytes", len(fh))
	}
	return fh
}

func pathOf(t *testing.T, f *FS, fh []byte) string {
	t.Helper()
	n, err := f.NodeByHandle(fh)
	if err != nil {
		t.Fatalf("NodeByHandle: %v", err)
	}
	return n.(*node).path
}

func TestHandlesSurviveRestart(t *testing.T) {
	base := t.TempDir()
	cases := map[string]Options{"path table": {PathTable: filepath.Join(base, "handles")}}
	if inodesUsable {
		cases["inodes"] = Options{}
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(base, name)
			if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
				t.Fatal(err)
			}
			f, err := Open("s", dir, opts)
			if err != nil {
				t.Fatal(err)
			}
			r := root(t, f)
			a := lookup(t, f, r, "a")
			b, err := f.Create(a, "b", nfs.Guarded, nfs.SetAttr{}, [8]byte{})
			if err != nil {
				t.Fatal(err)
			}
			hb, ha := handleOf(t, f, b), handleOf(t, f, a)
			if err := f.Rename(r, "a", r, "c"); err != nil {
				t.Fatal(err)
			}
			if got := pathOf(t, f, hb); got != "c/b" {
				t.Fatalf("after rename: %q", got)
			}
			f.Close()

			f, err = Open("s", dir, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if got := pathOf(t, f, hb); got != "c/b" {
				t.Fatalf("after restart: %q", got)
			}
			if got := pathOf(t, f, ha); got != "c" {
				t.Fatalf("directory after restart: %q", got)
			}
			if err := f.Remove(lookup(t, f, root(t, f), "c"), "b"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.NodeByHandle(hb); !errors.Is(err, nfs.ErrStale) {
				t.Fatalf("handle of a removed file: %v, want ErrStale", err)
			}
		})
	}
}

// With inodes, a handle finds its file after the host moves it, and goes
// stale when the host replaces it.
func TestInodeHandlesFollowTheHost(t *testing.T) {
	if !inodesUsable {
		t.Skip("no inodes on this host")
	}
	f, _ := newShare(t, Options{})
	r := root(t, f)
	fh := handleOf(t, f, lookup(t, f, r, "f"))
	share := f.Dir()
	if err := os.Mkdir(filepath.Join(share, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(share, "f"), filepath.Join(share, "d", "f2")); err != nil {
		t.Fatal(err)
	}
	if got := pathOf(t, f, fh); got != "d/f2" {
		t.Fatalf("after a host rename: %q", got)
	}
	if err := os.Remove(filepath.Join(share, "d", "f2")); err != nil {
		t.Fatal(err)
	}
	// Birth times come from the kernel's coarse clock; a file replaced
	// within one tick of its predecessor can't be told apart from it.
	time.Sleep(50 * time.Millisecond)
	if err := os.WriteFile(filepath.Join(share, "d", "f2"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	n, err := f.NodeByHandle(fh)
	if err == nil {
		// The new file reused the inode; only the generation can tell.
		if n.(*node).key.gen == 0 {
			t.Skip("inode reused and this filesystem reports no birth time")
		}
		t.Fatalf("handle of a replaced file resolved to %q", n.(*node).path)
	}
	wantErr(t, "replaced file", err, nfs.ErrStale)
}

func TestBadHandles(t *testing.T) {
	f, _ := newShare(t, Options{})
	fh := handleOf(t, f, root(t, f))
	_, err := f.NodeByHandle(fh[:16])
	wantErr(t, "short handle", err, nfs.ErrBadHandle)
	bad := append([]byte(nil), fh...)
	bad[0] = 9
	_, err = f.NodeByHandle(bad)
	wantErr(t, "unknown format", err, nfs.ErrBadHandle)
	other := append([]byte(nil), fh...)
	other[7] ^= 1
	_, err = f.NodeByHandle(other)
	wantErr(t, "another share's handle", err, nfs.ErrStale)

	g, err := Open("s", f.Dir(), Options{PathTable: filepath.Join(t.TempDir(), "handles")})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if inodesUsable {
		_, err = g.NodeByHandle(fh)
		wantErr(t, "an inode handle given to a path-table share", err, nfs.ErrStale)
	}
}

func TestExclusiveCreate(t *testing.T) {
	f, _ := newShare(t, Options{})
	r := root(t, f)
	verf := [8]byte{0, 0, 0, 1, 0, 0, 0, 2}
	n, err := f.Create(r, "x", nfs.Exclusive, nfs.SetAttr{}, verf)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.Create(r, "x", nfs.Exclusive, nfs.SetAttr{}, verf)
	if err != nil {
		t.Fatalf("retransmission: %v", err)
	}
	if again.(*node).path != n.(*node).path {
		t.Fatal("retransmission found another file")
	}
	verf[7]++
	_, err = f.Create(r, "x", nfs.Exclusive, nfs.SetAttr{}, verf)
	wantErr(t, "another verifier", err, nfs.ErrExist)
	_, err = f.Create(r, "x", nfs.Guarded, nfs.SetAttr{}, verf)
	wantErr(t, "guarded", err, nfs.ErrExist)
}

// A READDIR cookie stays with its name, so a client listing a directory
// while removing what it has seen loses nothing.
func TestReadDirCookiesSurviveRemoval(t *testing.T) {
	f, _ := newShare(t, Options{})
	r := root(t, f)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		if _, err := f.Create(r, name, nfs.Guarded, nfs.SetAttr{}, [8]byte{}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := f.ReadDir(r)
	if err != nil {
		t.Fatal(err)
	}
	cookies := map[string]uint32{}
	for i, e := range before {
		cookies[e.Name] = e.Cookie
		if i > 0 && e.Cookie <= before[i-1].Cookie {
			t.Fatal("entries not in cookie order")
		}
	}
	for _, e := range before[:3] {
		if err := f.Remove(r, e.Name); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := f.ReadDir(r)
	if len(after) != len(before)-3 {
		t.Fatalf("%d entries after removing 3 of %d", len(after), len(before))
	}
	for _, e := range after {
		if cookies[e.Name] != e.Cookie {
			t.Fatalf("%s moved from cookie %d to %d", e.Name, cookies[e.Name], e.Cookie)
		}
	}
}

func TestPathTableFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "sub", "handles")
	tb, err := openPathTable(file)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := tb.idFor("a")
	b, _ := tb.idFor("a/b")
	c, _ := tb.idFor("c d\n")
	tb.move("a", "x")
	tb.remove("c d\n")
	tb.close()

	tb, err = openPathTable(file)
	if err != nil {
		t.Fatal(err)
	}
	defer tb.close()
	if p, _ := tb.path(a); p != "x" {
		t.Fatalf("id %d = %q, want x", a, p)
	}
	if p, _ := tb.path(b); p != "x/b" {
		t.Fatalf("id %d = %q, want x/b", b, p)
	}
	if _, ok := tb.path(c); ok {
		t.Fatal("removed path survived")
	}
	if d, _ := tb.idFor("new"); d <= c {
		t.Fatalf("new id %d reuses one at or below %d", d, c)
	}
}

// On a filesystem without birth times (an NFS mount) the host gives a
// recreated file the inode of the one before it. Without a generation to
// tell them apart the share numbers paths itself, and a name recreated
// gets a handle the old one cannot reach.
func TestRecreatedNameGetsANewHandleWithoutBirthTime(t *testing.T) {
	table := filepath.Join(t.TempDir(), "handles")
	f, _ := newShare(t, Options{NoBirthTime: true, TableFallback: table})
	r := root(t, f)
	if f.table == nil {
		t.Fatal("no path table on a host without birth times")
	}
	recreate := func(how string, replace func()) {
		t.Helper()
		n, err := f.Create(r, "t", nfs.Guarded, nfs.SetAttr{}, [8]byte{})
		if err != nil {
			t.Fatal(err)
		}
		old := handleOf(t, f, n)
		replace()
		n2, err := f.Create(r, "t", nfs.Guarded, nfs.SetAttr{}, [8]byte{})
		if err != nil {
			t.Fatal(err)
		}
		if h := handleOf(t, f, n2); string(h) == string(old) {
			t.Fatalf("%s: the recreated file has the old handle %x", how, old)
		}
		if _, err := f.NodeByHandle(old); !errors.Is(err, nfs.ErrStale) {
			t.Fatalf("%s: the old handle: %v, want ErrStale", how, err)
		}
		if err := f.Remove(r, "t"); err != nil {
			t.Fatal(err)
		}
	}
	recreate("remove", func() {
		if err := f.Remove(r, "t"); err != nil {
			t.Fatal(err)
		}
	})
	recreate("rename over", func() {
		if _, err := f.Create(r, "u", nfs.Guarded, nfs.SetAttr{}, [8]byte{}); err != nil {
			t.Fatal(err)
		}
		if err := f.Rename(r, "u", r, "t"); err != nil {
			t.Fatal(err)
		}
		if err := f.Remove(r, "t"); err != nil {
			t.Fatal(err)
		}
	})
}

// A handle to a removed file stays dead across any number of restarts,
// even when it had the highest id the table ever gave out.
func TestRemovedHandleStaysDeadAcrossRestarts(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "share")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	opts := Options{NoBirthTime: true, TableFallback: filepath.Join(base, "handles")}
	open := func() *FS {
		t.Helper()
		f, err := Open("s", dir, opts)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	f := open()
	r := root(t, f)
	n, err := f.Create(r, "t", nfs.Guarded, nfs.SetAttr{}, [8]byte{})
	if err != nil {
		t.Fatal(err)
	}
	old := handleOf(t, f, n)
	if err := f.Remove(r, "t"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	for range 2 { // the first restart replays the removal, the second does not
		f = open()
		if _, err := f.NodeByHandle(old); !errors.Is(err, nfs.ErrStale) {
			t.Fatalf("old handle after restart: %v, want ErrStale", err)
		}
		n2, err := f.Create(root(t, f), "t", nfs.Guarded, nfs.SetAttr{}, [8]byte{})
		if err != nil {
			t.Fatal(err)
		}
		if string(handleOf(t, f, n2)) == string(old) {
			t.Fatal("a file made after a restart reuses the removed file's handle")
		}
		if _, err := f.NodeByHandle(old); !errors.Is(err, nfs.ErrStale) {
			t.Fatalf("old handle after a create: %v, want ErrStale", err)
		}
		if err := f.Remove(root(t, f), "t"); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
}

func TestNoBirthTimeNeedsATable(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open("s", dir, Options{NoBirthTime: true}); err == nil {
		t.Fatal("Open succeeded with no birth time and no table to fall back to")
	}
	// An explicit table wins over the fallback.
	explicit := filepath.Join(t.TempDir(), "explicit")
	f, err := Open("s", dir, Options{NoBirthTime: true, PathTable: explicit, TableFallback: filepath.Join(t.TempDir(), "other")})
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := os.Stat(explicit); err != nil {
		t.Fatalf("explicit table not used: %v", err)
	}
}

// The handle keeps the documented path-table layout when a share falls back.
func TestFallbackHandleLayout(t *testing.T) {
	f, _ := newShare(t, Options{NoBirthTime: true, TableFallback: filepath.Join(t.TempDir(), "handles")})
	h := handleOf(t, f, lookup(t, f, root(t, f), "f"))
	if len(h) != 32 || h[0] != fmtTable || h[1]|h[2]|h[3] != 0 {
		t.Fatalf("handle %x is not a path-table handle", h)
	}
	for _, b := range append(h[8:16:16], h[24:]...) {
		if b != 0 {
			t.Fatalf("device and generation are not zero in %x", h)
		}
	}
}
