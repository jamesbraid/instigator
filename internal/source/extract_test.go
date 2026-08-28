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
