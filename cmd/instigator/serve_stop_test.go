//go:build unix

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A container runtime kills a few seconds after the stop, skipping cleanup,
// so a stop must finish well inside that and leave the cache empty.
const stopWithin = 2 * time.Second

func extractDirs(t *testing.T, cache string) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(cache, "extract-*"))
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

// zeroTarGz returns a gzipped tar holding one file of size zero bytes.
func zeroTarGz(t *testing.T, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "dist/sa", Mode: 0o444, Size: int64(size), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	zeros := make([]byte, 1<<20)
	for n := 0; n < size; n += len(zeros) {
		if _, err := tw.Write(zeros[:min(len(zeros), size-n)]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func archiveConfig(t *testing.T, source, cache string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "instigator.yaml")
	config := fmt.Sprintf(`
server_ip: 192.0.2.10
cache_dir: %q
clients:
  - {name: octane, mac: "08:00:69:0e:af:12", ip: 127.0.0.1}
install_sets:
  - name: "6.5.30"
    layers:
      - {name: base, source: %q}
services: {bootp: false, tftp: {enabled: false}, rsh: false}
`, cache, source)
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func startServe(t *testing.T, config string) (*exec.Cmd, *lockedBuffer, <-chan error) {
	t.Helper()
	cmd := exec.Command(instigator(t), "serve", config)
	log := &lockedBuffer{}
	// Hide the embedded Buffer: exec would copy through its ReadFrom, past the lock.
	cmd.Stderr = struct{ io.Writer }{log}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill() })
	return cmd, log, done
}

func waitFor(t *testing.T, what string, log *lockedBuffer, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s:\n%s", what, log)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func stopAndCheck(t *testing.T, cmd *exec.Cmd, log *lockedBuffer, done <-chan error, cache string) {
	t.Helper()
	start := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve exited with %v after SIGTERM:\n%s", err, log)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("serve still running 15s after SIGTERM:\n%s", log)
	}
	if took := time.Since(start); took > stopWithin {
		t.Errorf("serve took %v to stop, want under %v:\n%s", took, stopWithin, log)
	}
	if left := extractDirs(t, cache); len(left) > 0 {
		t.Errorf("extraction directories left behind: %v", left)
	}
}

// Stopping a serving instance removes the extracted media.
func TestServeStopRemovesExtractedMedia(t *testing.T) {
	cache := t.TempDir()
	archive := filepath.Join(t.TempDir(), "dist.tar.gz")
	if err := os.WriteFile(archive, zeroTarGz(t, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, log, done := startServe(t, archiveConfig(t, archive, cache))
	waitFor(t, "serving", log, func() bool { return strings.Contains(log.String(), "serving; stop with") })
	if len(extractDirs(t, cache)) == 0 {
		t.Fatalf("no extraction while serving; the test checks nothing:\n%s", log)
	}
	stopAndCheck(t, cmd, log, done, cache)
}

// Stopping mid-extraction abandons it. The archive is large but compresses to
// almost nothing, so extraction outlasts the gap before the signal arrives.
func TestServeStopDuringExtractionAbandonsIt(t *testing.T) {
	cache := t.TempDir()
	archive := filepath.Join(t.TempDir(), "big.tar.gz")
	if err := os.WriteFile(archive, zeroTarGz(t, 128<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, log, done := startServe(t, archiveConfig(t, archive, cache))
	waitFor(t, "extraction to start", log, func() bool { return len(extractDirs(t, cache)) > 0 })
	stopAndCheck(t, cmd, log, done, cache)
	if strings.Contains(log.String(), "serving; stop with") {
		t.Errorf("server finished extracting and started serving before it stopped:\n%s", log)
	}
}
