package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jamesbraid/instigator/efs/efstest"
)

func writeDumpImage(t *testing.T, img *efstest.Builder) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disc1.iso")
	if err := os.WriteFile(p, img.CDImage(64, nil), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDumpFile(t *testing.T) {
	img := efstest.New()
	f := img.AddFile(0o640, []byte("hello world"))
	img.SetRoot(map[string]uint32{"greeting": f})
	imgPath := writeDumpImage(t, img)
	outdir := filepath.Join(t.TempDir(), "out")

	var sb strings.Builder
	if err := runDump(context.Background(), &sb, imgPath, "/greeting", outdir); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(outdir, "greeting"))
	if err != nil {
		t.Fatalf("extracted file missing: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("content = %q, want %q", got, "hello world")
	}
	fi, err := os.Stat(filepath.Join(outdir, "greeting"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o640 {
		t.Fatalf("perm = %o, want 0640", fi.Mode().Perm())
	}
	if !strings.Contains(sb.String(), "extracted 1 files, 11 bytes") {
		t.Fatalf("summary missing expected counts:\n%s", sb.String())
	}
}

func TestDumpDirRecursive(t *testing.T) {
	img := efstest.New()
	deep := img.AddFile(0o644, []byte("deep"))
	docs := img.AddDir(map[string]uint32{"deep.txt": deep})
	fx := img.AddFile(0o755, []byte("fx dummy"))
	stand := img.AddDir(map[string]uint32{"fx.64": fx, "docs": docs})
	img.SetRoot(map[string]uint32{"stand": stand})
	imgPath := writeDumpImage(t, img)
	outdir := filepath.Join(t.TempDir(), "out")

	var sb strings.Builder
	if err := runDump(context.Background(), &sb, imgPath, "/", outdir); err != nil {
		t.Fatal(err)
	}

	deepGot, err := os.ReadFile(filepath.Join(outdir, "stand", "docs", "deep.txt"))
	if err != nil {
		t.Fatalf("nested file missing: %v", err)
	}
	if string(deepGot) != "deep" {
		t.Fatalf("nested content = %q, want %q", deepGot, "deep")
	}
	fxGot, err := os.ReadFile(filepath.Join(outdir, "stand", "fx.64"))
	if err != nil {
		t.Fatalf("fx.64 missing: %v", err)
	}
	if string(fxGot) != "fx dummy" {
		t.Fatalf("fx.64 content = %q, want %q", fxGot, "fx dummy")
	}
	fi, err := os.Stat(filepath.Join(outdir, "stand", "fx.64"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o755 {
		t.Fatalf("fx.64 perm = %o, want 0755", fi.Mode().Perm())
	}
	if !strings.Contains(sb.String(), "extracted 2 files") {
		t.Fatalf("summary wrong file count:\n%s", sb.String())
	}
}

func TestDumpSymlink(t *testing.T) {
	img := efstest.New()
	target := img.AddFile(0o644, []byte("target data"))
	link := img.AddSymlink("real.txt")
	img.SetRoot(map[string]uint32{"real.txt": target, "link": link})
	imgPath := writeDumpImage(t, img)
	outdir := filepath.Join(t.TempDir(), "out")

	var sb strings.Builder
	if err := runDump(context.Background(), &sb, imgPath, "/", outdir); err != nil {
		t.Fatal(err)
	}

	got, err := os.Readlink(filepath.Join(outdir, "link"))
	if err != nil {
		t.Fatalf("symlink missing: %v", err)
	}
	if got != "real.txt" {
		t.Fatalf("symlink target = %q, want %q", got, "real.txt")
	}
}

// TestDumpSymlinkFailureContinues: a symlink that can't be recreated logs a
// warning and the extraction continues instead of aborting.
func TestDumpSymlinkFailureContinues(t *testing.T) {
	img := efstest.New()
	link := img.AddSymlink("somewhere")
	other := img.AddFile(0o644, []byte("still here"))
	img.SetRoot(map[string]uint32{"link": link, "other.txt": other})
	imgPath := writeDumpImage(t, img)
	outdir := filepath.Join(t.TempDir(), "out")
	if err := os.MkdirAll(outdir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Pre-create a plain file where the symlink would go, so os.Symlink
	// fails with "file exists" instead of succeeding.
	if err := os.WriteFile(filepath.Join(outdir, "link"), []byte("blocker"), 0o644); err != nil {
		t.Fatal(err)
	}

	var sb strings.Builder
	if err := runDump(context.Background(), &sb, imgPath, "/", outdir); err != nil {
		t.Fatalf("runDump aborted on symlink failure: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(outdir, "other.txt"))
	if err != nil {
		t.Fatalf("sibling file missing after symlink failure: %v", err)
	}
	if string(got) != "still here" {
		t.Fatalf("sibling content = %q", got)
	}
}

// TestDumpRejectsUnsafeEntryNames checks that directory entry names crafted
// to escape outdir (a slash, or a bare "..") are refused rather than
// followed, and that refusing them does not abort extraction of the rest
// of the tree.
func TestDumpRejectsUnsafeEntryNames(t *testing.T) {
	img := efstest.New()
	good := img.AddFile(0o644, []byte("ok"))
	evil := img.AddFile(0o644, []byte("should not escape"))
	img.SetRoot(map[string]uint32{
		"good.txt":   good,
		"../../evil": evil,
		"..":         evil,
	})
	imgPath := writeDumpImage(t, img)
	base := t.TempDir()
	outdir := filepath.Join(base, "out")

	var sb strings.Builder
	if err := runDump(context.Background(), &sb, imgPath, "/", outdir); err != nil {
		t.Fatal(err)
	}

	if _, err := os.ReadFile(filepath.Join(outdir, "good.txt")); err != nil {
		t.Fatalf("legitimate sibling missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "evil")); !os.IsNotExist(err) {
		t.Fatalf("unsafe entry escaped outdir: stat err = %v", err)
	}
	entries, err := os.ReadDir(outdir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "good.txt" {
		t.Fatalf("outdir contents = %v, want only good.txt", entries)
	}
}

// A dump stopped partway must not leave a truncated file under the real name.
func TestDumpStoppedLeavesNoPartialFile(t *testing.T) {
	img := efstest.New()
	f := img.AddFile(0o644, []byte("hello world"))
	img.SetRoot(map[string]uint32{"greeting": f})
	imgPath := writeDumpImage(t, img)
	outdir := filepath.Join(t.TempDir(), "out")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runDump(ctx, io.Discard, imgPath, "/greeting", outdir); !errors.Is(err, context.Canceled) {
		t.Fatalf("runDump = %v, want context.Canceled", err)
	}
	if _, err := os.Lstat(filepath.Join(outdir, "greeting")); !os.IsNotExist(err) {
		t.Fatalf("greeting after a stopped dump: %v, want it absent", err)
	}
}

// A symlink already where a file goes is replaced, so a stopped dump never
// truncates the file it pointed at.
func TestDumpStoppedLeavesSymlinkTargetIntact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	img := efstest.New()
	f := img.AddFile(0o644, []byte("hello world"))
	img.SetRoot(map[string]uint32{"greeting": f})
	imgPath := writeDumpImage(t, img)
	outdir := filepath.Join(t.TempDir(), "out")
	if err := os.Mkdir(outdir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "precious")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(outdir, "greeting")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runDump(ctx, io.Discard, imgPath, "/greeting", outdir); !errors.Is(err, context.Canceled) {
		t.Fatalf("runDump = %v, want context.Canceled", err)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "keep" {
		t.Fatalf("symlink target = %q, %v; want it untouched", b, err)
	}
}
