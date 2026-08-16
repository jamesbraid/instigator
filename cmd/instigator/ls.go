package main

import (
	"fmt"
	"io"
	"sort"

	"github.com/jamesbraid/instigator/internal/vfs"
)

// runLs prints a listing of path within an SGI CD image: directories list
// their entries, files print their metadata. The root listing also shows
// the volume header's raw boot files.
func runLs(w io.Writer, image, path string) error {
	d, err := vfs.OpenImage(image)
	if err != nil {
		return err
	}
	defer d.Close()

	if path == "" {
		path = "/"
	}
	if path == "/" && len(d.Header().BootFiles) > 0 {
		fmt.Fprintln(w, "volume header:")
		for _, f := range d.Header().BootFiles {
			fmt.Fprintf(w, "  b %10d  %s\n", f.Bytes, f.Name)
		}
	}

	node, err := d.FS().Lookup(path)
	if err != nil {
		return err
	}
	if !node.IsDir() {
		fmt.Fprintf(w, "%s: inode %d mode %04o size %d mtime %d\n",
			path, node.Num, node.Mode&0o7777, node.Size, node.Mtime)
		return nil
	}
	ents, err := d.FS().ReadDir(node)
	if err != nil {
		return err
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name < ents[j].Name })
	for _, e := range ents {
		n, err := d.FS().Inode(e.Ino)
		if err != nil {
			return fmt.Errorf("%s/%s: %w", path, e.Name, err)
		}
		t := "-"
		switch {
		case n.IsDir():
			t = "d"
		case n.IsSymlink():
			t = "l"
		}
		name := e.Name
		if n.IsSymlink() {
			if target, err := d.FS().Readlink(n); err == nil {
				name += " -> " + target
			}
		}
		fmt.Fprintf(w, "%s %04o %10d  %s\n", t, n.Mode&0o7777, n.Size, name)
	}
	return nil
}
