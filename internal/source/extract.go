package source

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Kind distinguishes a single materialized file from a materialized
// directory tree, so a caller knows how to present the extracted result.
type Kind int

const (
	File Kind = iota
	Dir
)

// maxExtractBytes bounds the total bytes Extract will write from one
// archive, so a hostile or corrupt source cannot exhaust the disk via a
// compression bomb or an unbounded entry stream.
const maxExtractBytes = 8 << 30

// Extract materializes src into dstDir, dispatching on name's (lowercased)
// extension: a .tar.gz or .tgz is gunzipped and untarred into dstDir, a
// bare .tar is untarred into dstDir, a .gz is gunzipped to a single file
// named after name with the .gz suffix stripped, and anything else is
// copied verbatim to a single file named after name. dstDir is created if
// it does not already exist. The returned path is dstDir itself for a
// directory result, or the file's path for a file result.
func Extract(src io.Reader, name, dstDir string) (Kind, string, error) {
	lower := strings.ToLower(name)
	base := filepath.Base(name)

	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		gz, err := gzip.NewReader(src)
		if err != nil {
			return 0, "", fmt.Errorf("source: %s: open gzip: %w", name, err)
		}
		defer gz.Close()
		if err := untar(gz, dstDir); err != nil {
			return 0, "", fmt.Errorf("source: %s: %w", name, err)
		}
		return Dir, dstDir, nil

	case strings.HasSuffix(lower, ".tar"):
		if err := untar(src, dstDir); err != nil {
			return 0, "", fmt.Errorf("source: %s: %w", name, err)
		}
		return Dir, dstDir, nil

	case strings.HasSuffix(lower, ".gz"):
		gz, err := gzip.NewReader(src)
		if err != nil {
			return 0, "", fmt.Errorf("source: %s: open gzip: %w", name, err)
		}
		defer gz.Close()
		// name matched ".gz" case-insensitively above; base is name's
		// final path element, so it carries the same suffix length and
		// can be trimmed by byte count rather than by (case-sensitive)
		// strings.TrimSuffix.
		unzipped := base[:len(base)-len(".gz")]
		if err := copyToFile(gz, dstDir, unzipped); err != nil {
			return 0, "", fmt.Errorf("source: %s: %w", name, err)
		}
		return File, filepath.Join(dstDir, unzipped), nil

	default:
		if err := copyToFile(src, dstDir, base); err != nil {
			return 0, "", fmt.Errorf("source: %s: %w", name, err)
		}
		return File, filepath.Join(dstDir, base), nil
	}
}

// copyToFile writes src to name under dir, creating dir if necessary,
// streaming through a bounded budget rather than reading src into memory.
func copyToFile(src io.Reader, dir, name string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open root %s: %w", dir, err)
	}
	defer root.Close()

	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}
	defer f.Close()

	if _, err := copyN(f, src, maxExtractBytes); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

// untar reads a tar stream from src and writes its entries beneath dstDir,
// creating dstDir if necessary. Every write goes through an os.Root opened
// on dstDir: root.OpenFile/MkdirAll resolve each path component through
// the kernel (openat2 on Linux) rather than a lexical string check, so a
// "../" entry, an absolute entry, or an entry that nets outside dstDir
// through an existing intermediate directory (e.g. "sub/../../escape") is
// refused with "path escapes from parent" - a hand-rolled ".." check is
// deliberately not duplicated here, since it would be both redundant and
// weaker than what the root already does.
//
// Symlinks are the one case os.Root does not gate at write time: root.
// Symlink happily creates a link whose target is absolute or escapes
// dstDir (it is only refused later, if something tries to follow it
// through a Root). So the target is validated by hand before the link is
// written.
//
// Only directories, regular files and contained symlinks are
// materialized; a hardlink, device, fifo or socket entry is rejected.
// Total extracted bytes across all entries are capped at maxExtractBytes.
func untar(src io.Reader, dstDir string) error {
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dstDir, err)
	}
	root, err := os.OpenRoot(dstDir)
	if err != nil {
		return fmt.Errorf("open root %s: %w", dstDir, err)
	}
	defer root.Close()

	tr := tar.NewReader(src)
	budget := int64(maxExtractBytes)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}

		name := path.Clean(hdr.Name)
		if name == "." {
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", name, err)
			}

		case tar.TypeReg:
			if dir := path.Dir(name); dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return fmt.Errorf("mkdir %s: %w", dir, err)
				}
			}
			f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, hdr.FileInfo().Mode().Perm())
			if err != nil {
				return fmt.Errorf("create %s: %w", name, err)
			}
			n, cerr := copyN(f, tr, budget)
			budget -= n
			closeErr := f.Close()
			if cerr != nil {
				return fmt.Errorf("write %s: %w", name, cerr)
			}
			if closeErr != nil {
				return fmt.Errorf("close %s: %w", name, closeErr)
			}

		case tar.TypeSymlink:
			if path.IsAbs(hdr.Linkname) {
				return fmt.Errorf("symlink %s -> %s: absolute target escapes the archive root", name, hdr.Linkname)
			}
			if target := path.Clean(path.Join(path.Dir(name), hdr.Linkname)); target == ".." || strings.HasPrefix(target, "../") {
				return fmt.Errorf("symlink %s -> %s: target escapes the archive root", name, hdr.Linkname)
			}
			if dir := path.Dir(name); dir != "." {
				if err := root.MkdirAll(dir, 0o755); err != nil {
					return fmt.Errorf("mkdir %s: %w", dir, err)
				}
			}
			if err := root.Symlink(hdr.Linkname, name); err != nil {
				return fmt.Errorf("symlink %s -> %s: %w", name, hdr.Linkname, err)
			}

		default:
			return fmt.Errorf("entry %q has unsupported type %q; only directories, files and symlinks are extracted", hdr.Name, hdr.Typeflag)
		}
	}
}

// copyN copies from src to dst, streaming rather than buffering the whole
// entry, and stops with an error once budget bytes have been written. It
// asks for one byte past budget so that a source with exactly budget bytes
// copies cleanly (io.CopyN then reports io.EOF, which is not an error
// here) while a source with more trips the over-budget check below.
func copyN(dst io.Writer, src io.Reader, budget int64) (int64, error) {
	n, err := io.CopyN(dst, src, budget+1)
	if err != nil && err != io.EOF {
		return n, err
	}
	if n > budget {
		return n, fmt.Errorf("entry exceeds the %d byte extraction limit", maxExtractBytes)
	}
	return n, nil
}
