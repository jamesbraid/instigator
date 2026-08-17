// Package vfs assembles the read-only filesystem instigator exports: one
// logical install set per configured name, each the ordered merge of its
// layers - SGI CD images opened in place and pre-extracted directories -
// plus the files instigator generates in memory. The assembled tree is an
// io/fs.FS, and every path in it resolves to exactly one Origin naming the
// layer or generator its bytes came from.
package vfs

import (
	"fmt"
	"os"

	"github.com/jamesbraid/instigator/dvh"
	"github.com/jamesbraid/instigator/efs"
)

// Disc is one opened CD image: its volume header and the EFS filesystem
// inside it.
type Disc struct {
	f   *os.File
	hdr *dvh.Header
	fs  *efs.FS
}

// OpenImage opens an SGI CD image: parse the volume header at block 0,
// locate the filesystem partition, open the EFS inside it.
func OpenImage(path string) (*Disc, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	hb := make([]byte, 512)
	if _, err := f.ReadAt(hb, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: reading volume header: %w", path, err)
	}
	hdr, err := dvh.Parse(hb)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	part, ok := hdr.EFS()
	if !ok {
		f.Close()
		return nil, fmt.Errorf("%s: no EFS partition in volume header", path)
	}
	fsys, err := efs.Open(f, int64(part.First))
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &Disc{f: f, hdr: hdr, fs: fsys}, nil
}

// FS returns the disc's filesystem.
func (d *Disc) FS() *efs.FS { return d.fs }

// Header returns the disc's volume header.
func (d *Disc) Header() *dvh.Header { return d.hdr }

// Close releases the underlying image file.
func (d *Disc) Close() error { return d.f.Close() }
