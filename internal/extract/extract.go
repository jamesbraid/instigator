// Package extract writes the files that inst products install, plus a JSONL
// manifest describing every entry, from configured install sets.
package extract

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jamesbraid/instigator/inst"
	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/mediatree"
	"github.com/jamesbraid/instigator/internal/vfs"
)

// Options selects what is extracted. An entry is kept when it passes both
// Paths and Subsystems.
type Options struct {
	Paths      []string // keep entries at or under one of these install paths; none keeps all
	Subsystems []string // keep entries whose subsystem matches one of these path.Match patterns; none keeps all
	Unpack     bool     // remove installed .z (pack) and .Z (compress) layers

	// ManifestOnly writes the manifest alone, planned from the .idb files
	// without opening any archive. It excludes Unpack, which needs the data.
	ManifestOnly bool
}

// Record is one manifest line. Field order here is the JSON field order.
type Record struct {
	Path        string `json:"path,omitempty"` // as written, relative to the set root; empty when nothing was
	InstallPath string `json:"installPath"`    // as in the .idb
	Type        string `json:"type,omitempty"`
	Mode        string `json:"mode,omitempty"` // octal, as in the .idb
	Owner       string `json:"owner,omitempty"`
	Group       string `json:"group,omitempty"`
	Product     string `json:"product"`
	Subsystem   string `json:"subsystem,omitempty"`
	Layer       string `json:"layer,omitempty"` // holding a file's archive, or else the product's .idb
	Set         string `json:"set,omitempty"`   // the install set that supplied the entry
	SHA256      string `json:"sha256,omitempty"`
	Symlink     string `json:"symlink,omitempty"` // target as in the .idb
	Target      string `json:"target,omitempty"`  // target as created
	Mach        string `json:"mach,omitempty"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

// Record statuses. A planned entry is one a manifest-only run would
// extract. A superseded entry was replaced by a later install set. An
// unparsed record is a .idb line that did not parse: its installPath holds
// the line and error says why.
const (
	written    = "written"
	duplicate  = "duplicate"
	variant    = "variant"
	planned    = "planned"
	superseded = "superseded"
	skipped    = "skipped"
	failed     = "error"
	unparsed   = "unparsed"
)

// summary names each status's count in Run's summary line.
var summary = [][2]string{{written, "written"}, {duplicate, "duplicate"}, {variant, "variant"}, {planned, "planned"},
	{superseded, "superseded"}, {skipped, "skipped"}, {failed, "errors"}, {unparsed, "unparsed"}}

// Run extracts the install sets of cfg named in names, or all of them,
// into outDir, one directory and one manifest per set, and writes a summary
// line per set to w. Each set is installed over the sets its After names.
// Every selected set is attempted; the error then names those that recorded
// a failed entry. Invalid options, existing output for any selected set and
// a distribution with no products are refused before anything is written.
func Run(w io.Writer, cfg *config.Config, names []string, outDir string, opts Options) error {
	for _, g := range opts.Subsystems {
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("extract: subsystem pattern %q: %w", g, err)
		}
	}
	if opts.ManifestOnly && opts.Unpack {
		return errors.New("extract: unpacking needs archive data, which a manifest-only run does not read")
	}
	if len(names) == 0 {
		for _, s := range cfg.InstallSets {
			names = append(names, s.Name)
		}
	}
	// Build every selected set and the sets it is installed over, once.
	orders := map[string][]config.InstallSet{}
	var build []config.InstallSet
	for _, name := range names {
		order, err := cfg.InstallOrder(name)
		if err != nil {
			return err
		}
		orders[name] = order
		for _, s := range order {
			if !slices.ContainsFunc(build, func(b config.InstallSet) bool { return b.Name == s.Name }) {
				build = append(build, s)
			}
		}
		for _, p := range []string{name, name + ".jsonl"} {
			if _, err := os.Lstat(filepath.Join(outDir, p)); err == nil {
				return fmt.Errorf("extract: %s already exists; refusing to mix old and new files", filepath.Join(outDir, p))
			} else if !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("extract: %w", err)
			}
		}
	}
	tree, err := mediatree.Build(build, cfg.CacheDir, cfg.Credentials)
	if err != nil {
		return err
	}
	defer tree.Close()
	open := map[string]dist{}
	for _, s := range build {
		d, err := openDist(tree, s.Name)
		if err != nil {
			return err
		}
		defer d.Close()
		open[s.Name] = d
	}
	if err := os.MkdirAll(outDir, 0o777); err != nil {
		return err
	}
	var bad []string
	for _, name := range names {
		var dists []dist
		for _, s := range orders[name] {
			dists = append(dists, open[s.Name])
		}
		n, err := extractSet(name, dists, outDir, opts)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		var counts []string
		for _, s := range summary {
			counts = append(counts, fmt.Sprintf("%d %s", n[s[0]], s[1]))
		}
		fmt.Fprintf(w, "%s: %s\n", name, strings.Join(counts, ", "))
		if n[failed] > 0 {
			bad = append(bad, name)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("extraction had errors in %s; see the manifest", strings.Join(bad, ", "))
	}
	return nil
}

// dist is the open distribution directory of one install set.
type dist struct {
	*inst.Dist
	set   string                   // the install set's name, recorded with each of its entries
	layer func(file string) string // the source layer holding a file of the directory
}

// openDist opens the distribution directory of the set name in tree.
func openDist(tree *vfs.Tree, name string) (dist, error) {
	fsys, err := fs.Sub(tree, name+"/dist")
	if err != nil {
		return dist{}, fmt.Errorf("extract: %s: %w", name, err)
	}
	d, err := inst.OpenDist(fsys)
	if err != nil {
		return dist{}, fmt.Errorf("extract: %s: %w", name, err)
	}
	if len(d.Products) == 0 {
		d.Close()
		return dist{}, fmt.Errorf("extract: no products in the distribution directory of %s", name)
	}
	return dist{Dist: d, set: name, layer: func(file string) string {
		o, err := tree.Resolve(name + "/dist/" + file)
		if err != nil {
			return ""
		}
		return o.Source
	}}, nil
}

// item is one manifest entry and what becomes of it.
type item struct {
	set int         // index of its dist
	e   *inst.Entry // nil for an unparsed line
	rel string      // install path relative to the set root, cleaned
	rec Record      // its status is set once the entry's fate is known
}

func (it *item) settle(status, reason string) {
	it.rec.Status, it.rec.Error = status, reason
}

func (it *item) fail(err error) { it.settle(failed, err.Error()) }

// extractSet extracts dists, install sets in the order they are installed,
// into <outDir>/<name>/ and writes the manifest to <outDir>/<name>.jsonl.
// It returns the number of records of each status, and an error only when
// an output cannot be created. A bad entry is recorded with status error.
//
// Records appear in set order, then product order, then .idb order, then
// the product's unparsed lines. plan settles everything that needs no
// archive data; writeAll does the rest.
func extractSet(name string, dists []dist, outDir string, opts Options) (map[string]int, error) {
	setDir := filepath.Join(outDir, name)
	manPath := filepath.Join(outDir, name+".jsonl")
	items := plan(dists, opts)
	var pending []*item
	for _, it := range items {
		if it.rec.Status == "" {
			pending = append(pending, it)
		}
	}
	if opts.ManifestOnly {
		for _, it := range pending {
			it.rec.Status = planned
		}
	} else {
		if err := os.Mkdir(setDir, 0o755); err != nil {
			return nil, fmt.Errorf("extract: %w", err)
		}
		root, err := os.OpenRoot(setDir)
		if err != nil {
			return nil, fmt.Errorf("extract: %w", err)
		}
		defer root.Close()
		w := &writer{root: root, dists: dists, unpack: opts.Unpack, dirs: map[string]bool{}, unpacked: map[string]string{},
			links: map[string]string{}}
		w.writeAll(pending)
	}
	f, err := os.OpenFile(manPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("extract: %w", err)
	}
	bw := bufio.NewWriter(f)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	n := map[string]int{}
	for _, it := range items {
		n[it.rec.Status]++
		if err == nil {
			err = enc.Encode(it.rec)
		}
	}
	if err == nil {
		err = bw.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("extract: writing manifest: %w", err)
	}
	return n, nil
}

// plan lists the entries the options keep, unrecorded otherwise, and
// settles every status that needs no archive data. Device, socket, type X
// and delhist entries are skipped. The rest are left pending, with no
// status.
//
// A product that a later install set ships again replaces the earlier
// product whole: every entry of the earlier one is superseded. The
// remaining entries are grouped by install path. Where every claimant is in
// one set, or every claimant is a directory, the writer's duplicate and
// variant rules apply. Where a directory and a file or link meet across
// sets, the entries of every set after the earliest are errors: replacing
// one with the other would silently drop or bury what the earlier set
// installed. Otherwise every entry of an earlier set than the latest is
// superseded, and never read.
//
// This is a union with later-wins, not an inst run: inst also removes files
// of subsystems an overlay product declares it replaces, using rules in the
// binary product descriptions, which are not read here. A file the base
// installs and an overlay retires without reinstalling stays.
func plan(dists []dist, opts Options) []*item {
	var paths []string
	for _, p := range opts.Paths {
		if p = strings.Trim(p, "/"); p != "" {
			paths = append(paths, path.Clean(p))
		}
	}
	last := map[string]int{} // product name -> the last set shipping it
	for s, d := range dists {
		for _, p := range d.Products {
			last[p.Name] = s
		}
	}
	var items []*item
	for s, d := range dists {
		for _, p := range d.Products {
			for i := range p.Entries {
				e := &p.Entries[i]
				if !matchAny(opts.Subsystems, e.Subsystem) {
					continue
				}
				it := &item{set: s, e: e, rec: Record{
					InstallPath: e.Path, Type: string(e.Type), Mode: fmt.Sprintf("%04o", e.Mode),
					Owner: e.Owner, Group: e.Group, Product: p.Name, Subsystem: e.Subsystem,
					Layer: d.layer(p.Name + ".idb"), Set: d.set, Symlink: e.Symlink, Mach: e.Mach,
				}}
				if e.Type == 'f' {
					it.rec.Layer = d.layer(p.Name + "." + e.Image())
				}
				rel, ok := cleanInstallPath(e.Path)
				switch {
				case !ok && rel == "." && e.Type == 'd':
					if len(paths) > 0 {
						continue // outside every --path
					}
					it.settle(skipped, "the set root itself; already created")
				case !ok:
					it.settle(failed, "install path escapes the set root")
				case len(paths) > 0 && !underAny(rel, paths):
					continue
				case last[p.Name] > s:
					it.settle(superseded, "")
				case slices.Contains(e.Flags, "delhist"):
					// These lines name paths an older release installed:
					// they carry no archive data, and they contradict the
					// product's own entries, such as a directory beneath a
					// path it installs as a link. They are taken to be
					// removal history for upgrades.
					it.settle(skipped, "delhist entry records a path an older release installed")
				case e.Type == 'X':
					// These lines name a path but carry no off(), size() or
					// data. What inst does with them is unconfirmed.
					it.settle(skipped, "type X entry carries no data")
				case e.Type != 'd' && e.Type != 'l' && e.Type != 'f':
					it.settle(skipped, "special files are not created")
				}
				it.rel = rel
				items = append(items, it)
			}
			for _, b := range p.Bad {
				items = append(items, &item{rec: Record{InstallPath: b.Text, Product: p.Name, Layer: d.layer(p.Name + ".idb"),
					Set: d.set, Status: unparsed, Error: fmt.Sprintf("line %d: %s", b.Line, b.Reason)}})
			}
		}
	}

	byPath := map[string][]*item{}
	for _, it := range items {
		if it.rec.Status == "" {
			byPath[it.rel] = append(byPath[it.rel], it)
		}
	}
	for _, its := range byPath {
		lo, hi := its[0].set, its[0].set
		dirs, files := false, false
		for _, it := range its {
			lo, hi = min(lo, it.set), max(hi, it.set)
			dirs, files = dirs || it.e.Type == 'd', files || it.e.Type != 'd'
		}
		if lo == hi || !files {
			continue
		}
		for _, it := range its {
			switch {
			case dirs && it.set != lo:
				kind, other := "file or link", "directory"
				if it.e.Type == 'd' {
					kind, other = other, kind
				}
				it.settle(failed, fmt.Sprintf("installs a %s where an earlier install set installs a %s", kind, other))
			case !dirs && it.set < hi:
				it.settle(superseded, "")
			}
		}
	}
	return items
}

func matchAny(globs []string, s string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, s); ok {
			return true
		}
	}
	return len(globs) == 0
}

// cleanInstallPath turns an install path into a path relative to the set
// root. It reports false for the root itself, returned as ".", and for a
// path that escapes the root.
func cleanInstallPath(p string) (string, bool) {
	rel := path.Clean(strings.TrimLeft(p, "/"))
	return rel, rel != "." && rel != ".." && !strings.HasPrefix(rel, "../")
}

// underAny reports whether rel is one of dirs or lies under one, matching
// whole path components: usr/include keeps usr/include/x, not usr/include2.
func underAny(rel string, dirs []string) bool {
	for _, d := range dirs {
		if rel == d || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}
