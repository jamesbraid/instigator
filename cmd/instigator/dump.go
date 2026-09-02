package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jamesbraid/instigator/efs"
	"github.com/jamesbraid/instigator/internal/vfs"
)

// runDump extracts image's srcPath tree to outdir on the host - the
// last-resort fallback for sites where serving over rsh is difficult.
func runDump(w io.Writer, image, srcPath, outdir string) error {
	d, err := vfs.OpenImage(image)
	if err != nil {
		return err
	}
	defer d.Close()

	if srcPath == "" {
		srcPath = "/"
	}
	node, err := d.FS().Lookup(srcPath)
	if err != nil {
		return err
	}

	ex := &dumper{fs: d.FS()}
	target := outdir
	if !node.IsDir() {
		// outdir names the directory to extract into; a single file or
		// symlink keeps its own basename underneath it.
		if err := os.MkdirAll(outdir, 0o777); err != nil {
			return err
		}
		target = filepath.Join(outdir, filepath.Base(strings.TrimRight(srcPath, "/")))
	}
	if err := ex.extract(node, target); err != nil {
		return err
	}
	fmt.Fprintf(w, "extracted %d files, %d bytes\n", ex.files, ex.bytes)
	return nil
}

// dumper walks an EFS subtree onto the host filesystem, tallying the
// regular files and bytes it writes.
type dumper struct {
	fs    *efs.FS
	files int
	bytes int64
}

func (ex *dumper) extract(node *efs.Inode, dst string) error {
	switch {
	case node.IsDir():
		return ex.extractDir(node, dst)
	case node.IsSymlink():
		return ex.extractSymlink(node, dst)
	case node.IsRegular():
		return ex.extractFile(node, dst)
	default:
		fmt.Fprintf(os.Stderr, "instigator: dump: %s: unrecognized inode type, skipping\n", dst)
		return nil
	}
}

func (ex *dumper) extractDir(node *efs.Inode, dst string) error {
	if err := os.MkdirAll(dst, 0o777); err != nil {
		return err
	}
	ents, err := ex.fs.ReadDir(node)
	if err != nil {
		return err
	}
	for _, e := range ents {
		// Skip the self/parent entries; dropping ".." also blocks it as an
		// escape vector.
		if e.Name == "." || e.Name == ".." {
			continue
		}
		if strings.Contains(e.Name, "/") {
			fmt.Fprintf(os.Stderr, "instigator: dump: %s: unsafe entry name %q, skipping\n", dst, e.Name)
			continue
		}
		child, err := ex.fs.Inode(e.Ino)
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Join(dst, e.Name), err)
		}
		if err := ex.extract(child, filepath.Join(dst, e.Name)); err != nil {
			return err
		}
	}
	return os.Chmod(dst, os.FileMode(node.Mode&0o777))
}

func (ex *dumper) extractFile(node *efs.Inode, dst string) error {
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return err
	}
	if err := ex.copyContent(f, node); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	ex.files++
	return os.Chmod(dst, os.FileMode(node.Mode&0o777))
}

// copyContent streams a file's data via ReadAt in fixed chunks, treating
// io.EOF as the normal end of the file per the io.ReaderAt contract.
func (ex *dumper) copyContent(f *os.File, node *efs.Inode) error {
	buf := make([]byte, 64*1024)
	var off int64
	for {
		n, err := ex.fs.ReadAt(node, buf, off)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			off += int64(n)
			ex.bytes += int64(n)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (ex *dumper) extractSymlink(node *efs.Inode, dst string) error {
	target, err := ex.fs.Readlink(node)
	if err != nil {
		return err
	}
	if err := os.Symlink(target, dst); err != nil {
		fmt.Fprintf(os.Stderr, "instigator: dump: %s: %v\n", dst, err)
		return nil
	}
	return nil
}
