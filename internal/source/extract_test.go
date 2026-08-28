package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// tgz builds an in-memory .tar.gz containing entries (path -> body), each
// written as a regular file.
func tgz(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestExtractTarGzTree(t *testing.T) {
	dst := t.TempDir()
	kind, root, err := Extract(bytes.NewReader(tgz(t, map[string]string{"disc1/dist/pkg": "P"})), "disc1.tar.gz", dst)
	if err != nil || kind != Dir {
		t.Fatalf("kind=%v err=%v, want Dir", kind, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "disc1", "dist", "pkg")); string(b) != "P" {
		t.Errorf("extracted pkg = %q, want P", b)
	}
}

func TestExtractRejectsTraversal(t *testing.T) {
	dst := t.TempDir()
	if _, _, err := Extract(bytes.NewReader(tgz(t, map[string]string{"../escape": "x"})), "e.tar.gz", dst); err == nil {
		t.Fatal("Extract accepted a ../ entry; want rejection")
	}
}

// tarWith builds an in-memory .tar.gz from explicit tar headers, for cases
// tgz's plain-regular-file map can't express: symlinks, hardlinks, device
// nodes, and any other entry needing a specific Typeflag or Linkname.
func tarWith(t *testing.T, entries []tar.Header, bodies map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		body := bodies[h.Name]
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestExtractRejectsAbsoluteSymlink(t *testing.T) {
	dst := t.TempDir()
	archive := tarWith(t, []tar.Header{
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/x"},
	}, nil)
	if _, _, err := Extract(bytes.NewReader(archive), "a.tar.gz", dst); err == nil {
		t.Fatal("Extract accepted an absolute symlink target; want rejection")
	}
	if _, err := os.Lstat(filepath.Join(dst, "link")); err == nil {
		t.Error("rejected symlink was written into dstDir")
	}
}

// TestExtractRejectsBackslashSymlink guards the Windows-style escape
// vectors a forward-slash-only path.IsAbs/path.Clean check would miss: a
// drive-letter-absolute target and a backslash-separated "..\..\" climb.
// Both contain a backslash, which Extract rejects outright regardless of
// host OS, so this is verifiable on Linux even though the vectors describe
// a Windows target.
func TestExtractRejectsBackslashSymlink(t *testing.T) {
	for _, linkname := range []string{`C:\Windows\x`, `..\..\loot`} {
		dst := t.TempDir()
		archive := tarWith(t, []tar.Header{
			{Name: "link", Typeflag: tar.TypeSymlink, Linkname: linkname},
		}, nil)
		if _, _, err := Extract(bytes.NewReader(archive), "b.tar.gz", dst); err == nil {
			t.Errorf("Extract accepted backslash symlink target %q; want rejection", linkname)
		}
		if _, err := os.Lstat(filepath.Join(dst, "link")); err == nil {
			t.Errorf("rejected symlink %q was written into dstDir", linkname)
		}
	}
}

func TestExtractRejectsSymlinkEscape(t *testing.T) {
	dst := t.TempDir()
	archive := tarWith(t, []tar.Header{
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../../loot"},
	}, nil)
	if _, _, err := Extract(bytes.NewReader(archive), "e.tar.gz", dst); err == nil {
		t.Fatal("Extract accepted a symlink escaping dstDir; want rejection")
	}
	if _, err := os.Lstat(filepath.Join(dst, "link")); err == nil {
		t.Error("rejected symlink was written into dstDir")
	}
}

func TestExtractAllowsContainedSymlink(t *testing.T) {
	dst := t.TempDir()
	archive := tarWith(t, []tar.Header{
		{Name: "dir/a", Typeflag: tar.TypeReg},
		{Name: "dir/b", Typeflag: tar.TypeSymlink, Linkname: "a"},
	}, map[string]string{"dir/a": "A"})
	kind, root, err := Extract(bytes.NewReader(archive), "c.tar.gz", dst)
	if err != nil || kind != Dir {
		t.Fatalf("kind=%v err=%v, want Dir", kind, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "dir", "b")); err != nil || string(b) != "A" {
		t.Errorf("read via contained symlink = %q, err=%v, want A", b, err)
	}
}

func TestExtractRejectsHardlink(t *testing.T) {
	dst := t.TempDir()
	archive := tarWith(t, []tar.Header{
		{Name: "real", Typeflag: tar.TypeReg},
		{Name: "hard", Typeflag: tar.TypeLink, Linkname: "real"},
	}, map[string]string{"real": "X"})
	if _, _, err := Extract(bytes.NewReader(archive), "h.tar.gz", dst); err == nil {
		t.Fatal("Extract accepted a hardlink entry; want rejection")
	}
}

func TestExtractRejectsCharDevice(t *testing.T) {
	dst := t.TempDir()
	archive := tarWith(t, []tar.Header{
		{Name: "dev", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3},
	}, nil)
	if _, _, err := Extract(bytes.NewReader(archive), "d.tar.gz", dst); err == nil {
		t.Fatal("Extract accepted a char-device entry; want rejection")
	}
}

func TestExtractEnforcesByteCap(t *testing.T) {
	orig := maxExtractBytes
	maxExtractBytes = 5
	t.Cleanup(func() { maxExtractBytes = orig })

	dst := t.TempDir()
	if _, _, err := Extract(bytes.NewReader(tgz(t, map[string]string{"big": "123456"})), "big.tar.gz", dst); err == nil {
		t.Fatal("Extract accepted an entry over the byte cap; want rejection")
	}

	dst2 := t.TempDir()
	kind, root, err := Extract(bytes.NewReader(tgz(t, map[string]string{"ok": "12345"})), "ok.tar.gz", dst2)
	if err != nil || kind != Dir {
		t.Fatalf("kind=%v err=%v, want Dir for an entry at the cap", kind, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "ok")); string(b) != "12345" {
		t.Errorf("extracted ok = %q, want 12345", b)
	}
}
