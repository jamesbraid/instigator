// Package vfs assembles the read-only filesystem instigator exports: one
// logical install set per configured name, each the ordered merge of its
// layers - SGI CD images opened in place and pre-extracted directories -
// plus the files instigator generates in memory. The assembled tree is an
// io/fs.FS, and every path in it resolves to exactly one Origin naming the
// layer or generator its bytes came from.
package vfs

import (
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/jamesbraid/instigator/dvh"
	"github.com/jamesbraid/instigator/efs"
)

// Disc is one opened CD image: its volume header and the EFS filesystem
// inside it.
type Disc struct {
	c   io.Closer
	hdr *dvh.Header
	fs  *efs.FS
}

// OpenImage opens an SGI CD image at a filesystem path.
func OpenImage(path string) (*Disc, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	d, err := OpenImageReader(f, f, path)
	if err != nil {
		f.Close()
		return nil, err
	}
	return d, nil
}

// OpenImageReader opens an SGI CD image from an arbitrary reader: parse the
// volume header at block 0, locate the EFS partition, open the EFS inside
// it. c is closed by Disc.Close; name appears in errors.
func OpenImageReader(r io.ReaderAt, c io.Closer, name string) (*Disc, error) {
	hb := make([]byte, 512)
	if _, err := r.ReadAt(hb, 0); err != nil {
		return nil, fmt.Errorf("%s: reading volume header: %w", name, err)
	}
	hdr, err := dvh.Parse(hb)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	part, ok := hdr.EFS()
	if !ok {
		return nil, fmt.Errorf("%s: no EFS partition in volume header", name)
	}
	fsys, err := efs.Open(r, int64(part.First))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return &Disc{c: c, hdr: hdr, fs: fsys}, nil
}

// FS returns the disc's filesystem.
func (d *Disc) FS() *efs.FS { return d.fs }

// FSys returns the disc's filesystem as an io/fs.FS.
func (d *Disc) FSys() fs.FS { return d.fs.FSys() }

// Header returns the disc's volume header.
func (d *Disc) Header() *dvh.Header { return d.hdr }

// Close releases the disc's underlying resource.
func (d *Disc) Close() error { return d.c.Close() }
