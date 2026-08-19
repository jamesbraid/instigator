package instcmd

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/jamesbraid/instigator/internal/vfs"
)

// realMediaImage is a real IRIX 6.5.30 Installation Tools image, served
// here as the single layer of one install set. The test below is skipped
// when it is absent, so CI (which has no SGI media) stays green while a
// developer with the media gets byte-exact coverage of the rsh dd path
// inst actually drives to fetch its distribution.
const realMediaImage = "/storage/software/os/irix/Irix 6.5.30_cdimages/Instalation_Tools_and_Overlays1.image"

// dist/sa on the Installation Tools disc, per the irix-efs-tools oracle
// - the same file and checksum already checked over raw EFS reads in
// internal/vfs's own real-media test. internal/nfsexport is retained,
// experimental, and out of the served path; it carries only synthetic
// coverage, not this checksum.
const (
	realSAPath = "/6.5.30/dist/sa"
	realSADir  = "/6.5.30/dist"
	realSASHA  = "cf4318a234aa2e3216799927d197f556d548e07200bb08eb9d486630dd0f48d5"
	realSASize = 20067840
)

// treeFS adapts a vfs.Tree to instcmd.FileSystem, translating the
// not-found error each expects. Mirrors internal/serve's cmdFS, which
// wires the same adapter into the real rsh server.
type treeFS struct{ t *vfs.Tree }

func (f treeFS) Open(path string) (File, error) {
	file, err := f.t.Open(path)
	if errors.Is(err, vfs.ErrNotFound) {
		return nil, fmt.Errorf("%s: %w", path, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	regular, ok := file.(vfs.File)
	if !ok {
		file.Close()
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	return regular, nil
}

func (f treeFS) ReadDir(path string) ([]string, error) {
	ents, err := f.t.ReadDir(path)
	if errors.Is(err, vfs.ErrNotFound) {
		return nil, fmt.Errorf("%s: %w", path, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names, nil
}

func (f treeFS) Stat(path string) (FileInfo, error) {
	info, err := f.t.Stat(path)
	if errors.Is(err, vfs.ErrNotFound) {
		return FileInfo{}, fmt.Errorf("%s: %w", path, ErrNotFound)
	}
	if err != nil {
		return FileInfo{}, err
	}
	md, ok := info.Sys().(*vfs.Metadata)
	if !ok {
		md = &vfs.Metadata{}
	}
	return FileInfo{
		Ino:   md.Ino,
		IsDir: info.IsDir(),
		Perm:  uint32(info.Mode().Perm()),
		Nlink: md.Nlink,
		UID:   md.UID,
		GID:   md.GID,
		Size:  info.Size(),
		Mtime: info.ModTime(),
	}, nil
}

// countingWriter counts the bytes written through it while forwarding
// them to w, so a 20MB dd transfer can be hashed on the fly instead of
// held in memory.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// TestRSHRealMediaDD runs the same "dd if=... bs=..." command line inst
// issues over rsh straight through instcmd.Run against a real 6.5.30
// image, and checks the streamed stdout against the irix-efs-tools
// oracle checksum for dist/sa. dd's records-in/out summary goes to a
// separate stderr buffer, so stdout here is exactly the file's bytes.
// This proves the rsh distribution path - the table this package
// implements - reads real IRIX media byte-exact, not just the synthetic
// fixture in instcmd_test.go.
func TestRSHRealMediaDD(t *testing.T) {
	if _, err := os.Stat(realMediaImage); err != nil {
		t.Skip("real IRIX 6.5.30 media not present")
	}
	tree, err := vfs.Build([]vfs.SetSpec{{
		Name:   "6.5.30",
		Layers: []vfs.LayerSpec{{Name: "tools", Image: realMediaImage, Boot: true}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Close()
	fs := treeFS{tree}

	h := sha256.New()
	stdout := &countingWriter{w: h}
	var stderr strings.Builder
	if err := Run(fs, "dd if="+realSAPath+" bs=8192", stdout, &stderr); err != nil {
		t.Fatalf("dd: %v (stderr: %s)", err, stderr.String())
	}
	if stdout.n != realSASize {
		t.Fatalf("dd copied %d bytes, want %d", stdout.n, realSASize)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != realSASHA {
		t.Fatalf("sha256 %s, oracle %s", got, realSASHA)
	}
	t.Logf("dist/sa: %d bytes over rsh dd, sha256 matches oracle", stdout.n)

	var ls strings.Builder
	if err := Run(fs, "ls "+realSADir, &ls, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sa", "miniroot"} {
		if !strings.Contains(ls.String(), want) {
			t.Fatalf("ls %s missing %q:\n%s", realSADir, want, ls.String())
		}
	}
}
