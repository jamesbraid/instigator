package extract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jamesbraid/instigator/inst"
	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/insttest"
)

type ent = insttest.Entry

func h8(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// run extracts dists as set "set" and returns the output directory, the
// manifest and the status counts.
func run(t *testing.T, opts Options, dists ...dist) (string, []Record, map[string]int) {
	t.Helper()
	out := t.TempDir()
	res, err := extractSet("set", dists, out, opts)
	if err != nil {
		t.Fatal(err)
	}
	man, err := os.ReadFile(filepath.Join(out, "set.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var rs []Record
	for _, l := range strings.Split(strings.TrimSpace(string(man)), "\n") {
		var r Record
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("manifest line %q: %v", l, err)
		}
		rs = append(rs, r)
	}
	return out, rs, res
}

// openTest opens fsys as the distribution of set. layers names the source
// layer of each file.
func openTest(t *testing.T, set string, fsys fs.FS, layers map[string]string) dist {
	t.Helper()
	d, err := inst.OpenDist(fsys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return dist{Dist: d, set: set, layer: func(file string) string { return layers[file] }}
}

// configOf returns a config of one install set per distribution, named s0,
// s1 and so on, each a directory layer.
func configOf(t *testing.T, dists ...fstest.MapFS) *config.Config {
	cfg := &config.Config{}
	for i, d := range dists {
		src := insttest.WriteDir(t, t.TempDir(), d)
		cfg.InstallSets = append(cfg.InstallSets, config.InstallSet{Name: fmt.Sprint("s", i),
			Layers: []config.Layer{{Name: "l", Source: src, Dist: "."}}})
	}
	return cfg
}

// brief is a record as "installPath status [path] [-> target]".
func brief(r Record) string {
	s := strings.TrimSpace(r.InstallPath + " " + r.Status + " " + r.Path)
	if r.Target != "" {
		s += " -> " + r.Target
	}
	return s
}

func TestSet(t *testing.T) {
	cases := []struct {
		name  string
		base  []map[string][]ent // sets installed before prods, in order
		prods map[string][]ent
		opts  Options
		want  []string          // brief records in manifest order
		files map[string]string // contents under the set root, read through links; "!" for absent

		// unread: no archive of the base sets is opened, or with
		// ManifestOnly no archive at all.
		unread bool
	}{
		{
			name: "paths match whole components",
			prods: map[string][]ent{"eoe": {
				{Path: "usr/a", Sub: "eoe.sw.base", Data: "A"},
				{Path: "var/b", Sub: "eoe.sw.base", Data: "B"},
				{Path: "/usr/c", Sub: "eoe.sw.base", Data: "C"},
				{Path: "usr2/d", Sub: "eoe.sw.base", Data: "D"},
				{Type: 'd', Path: "usr", Sub: "eoe.sw.base"},
			}},
			opts:  Options{Paths: []string{"/usr", "usr/"}},
			want:  []string{"usr/a written usr/a", "/usr/c written usr/c", "usr written usr"},
			files: map[string]string{"usr/a": "A", "usr/c": "C", "var/b": "!", "usr2/d": "!"},
		},
		{
			// Both kinds of filter must match, before anything is read:
			// the sw image's record is missing.
			name: "subsystems",
			prods: map[string][]ent{"eoe": {
				{Type: 'd', Path: "/", Sub: "eoe.man.base"},
				{Path: "usr/share/man/a", Sub: "eoe.man.base", Data: "A"},
				{Path: "usr/share/man/b", Sub: "eoe.sw.base", Missing: true},
				{Path: "usr/lib/c", Sub: "eoe.man.relnotes", Data: "C"},
				{Path: "usr/share/d", Sub: "eoe.books.base", Data: "D"},
			}},
			opts: Options{Subsystems: []string{"*.man.*", "eoe.books.x*"}, Paths: []string{"usr/share"}, Unpack: true},
			want: []string{"usr/share/man/a written usr/share/man/a"},
		},
		{
			name: "entry kinds",
			prods: map[string][]ent{"eoe": {
				{Type: 'd', Path: "usr/bin", Sub: "eoe.sw.base"},
				{Path: "usr/bin/run", Sub: "eoe.sw.base", Data: "#!/bin/sh\n"},
				{Type: 'c', Path: "dev/null", Sub: "eoe.sw.base"},
				{Type: 'X', Path: "usr/gone", Sub: "eoe.sw.base"},
				{Line: "this is not a valid line"},
			}},
			want: []string{"usr/bin written usr/bin", "usr/bin/run written usr/bin/run", "dev/null skipped",
				"usr/gone skipped", "this is not a valid line unparsed"},
			files: map[string]string{"usr/bin/run": "#!/bin/sh\n", "dev/null": "!", "usr/gone": "!"},
		},
		{
			name: "links are relative and stay inside the root",
			prods: map[string][]ent{"eoe": {
				{Path: "usr/lib/real", Sub: "eoe.sw.base", Data: "x"},
				{Type: 'l', Path: "usr/lib/rel", Sub: "eoe.sw.base", Symval: "real"},
				{Type: 'l', Path: "usr/lib/abs", Sub: "eoe.sw.base", Symval: "/usr/lib/real"},
				{Type: 'l', Path: "usr/lib/up", Sub: "eoe.sw.base", Symval: "../../.."},
				// "/.." is "/", so this one stays inside.
				{Type: 'l', Path: "usr/lib/absup", Sub: "eoe.sw.base", Symval: "/../../etc"},
			}},
			want: []string{"usr/lib/real written usr/lib/real", "usr/lib/rel written usr/lib/rel -> real",
				"usr/lib/abs written usr/lib/abs -> real", "usr/lib/up skipped",
				"usr/lib/absup written usr/lib/absup -> ../../etc"},
			files: map[string]string{"usr/lib/rel": "x", "usr/lib/abs": "x", "usr/lib/up": "!"},
		},
		{
			name: "duplicates and variants",
			prods: map[string][]ent{
				"0": {{Path: "etc/lib", Sub: "0.sw.base", Data: "one"}},
				"a": {{Path: "etc/conf", Sub: "a.sw.base", Data: "same"}},
				"b": {
					{Path: "etc/conf", Sub: "b.sw.base", Data: "same"},
					{Path: "etc/lib", Sub: "b.sw.base", Data: "two", Mach: "CPUBOARD=IP27"},
				},
				"c": {{Path: "etc/lib", Sub: "c.sw.base", Data: "three"}},
			},
			want: []string{"etc/lib written etc/lib", "etc/conf written etc/conf", "etc/conf duplicate etc/conf",
				"etc/lib variant etc/lib.CPUBOARD=IP27", "etc/lib variant etc/lib." + h8("three")},
			files: map[string]string{"etc/lib": "one", "etc/lib.CPUBOARD=IP27": "two", "etc/lib." + h8("three"): "three"},
		},
		{
			// No name is staged beside a file, so any install path is
			// free to use.
			name: "a path ending like a staging name",
			prods: map[string][]ent{"eoe": {
				{Path: "etc/foo.extract-tmp", Sub: "eoe.sw.base", Data: "a"},
				{Path: "etc/foo", Sub: "eoe.sw.base", Data: "b"},
			}},
			want:  []string{"etc/foo.extract-tmp written etc/foo.extract-tmp", "etc/foo written etc/foo"},
			files: map[string]string{"etc/foo.extract-tmp": "a", "etc/foo": "b"},
		},
		{
			// A mach tag listing every graphics board makes a name longer
			// than a file system allows; the digest names it instead.
			name: "variant with a long mach tag",
			prods: map[string][]ent{"eoe": {
				{Path: "usr/gfx/init", Sub: "eoe.sw.base", Data: "one"},
				{Path: "usr/gfx/init", Sub: "eoe.sw.base", Data: "two", Mach: strings.Repeat("GFXBOARD=X_", 30)},
			}},
			want:  []string{"usr/gfx/init written usr/gfx/init", "usr/gfx/init variant usr/gfx/init." + h8("two")},
			files: map[string]string{"usr/gfx/init." + h8("two"): "two"},
		},
		{
			name: "link variants",
			prods: map[string][]ent{"eoe": {
				{Type: 'l', Path: "var/boot", Sub: "eoe.sw.base", Symval: "IP27boot", Mach: "CPUBOARD=IP27"},
				{Type: 'l', Path: "var/boot", Sub: "eoe.sw.base", Symval: "IP30boot", Mach: "CPUBOARD=IP30"},
				{Type: 'l', Path: "var/boot", Sub: "eoe.sw.base", Symval: "IP27boot", Mach: "CPUBOARD=IP27"},
				{Type: 'l', Path: "var/boot", Sub: "eoe.sw.base", Symval: "other"},
			}},
			want: []string{"var/boot written var/boot -> IP27boot", "var/boot variant var/boot.CPUBOARD=IP30 -> IP30boot",
				"var/boot duplicate var/boot -> IP27boot", "var/boot variant var/boot." + h8("other") + " -> other"},
		},
		{
			// Links are written after files, so the file takes the path
			// whichever comes first.
			name: "a file and a link at one path, either way round",
			prods: map[string][]ent{"p": {
				{Path: "usr/x", Sub: "p.sw.base", Data: "data"},
				{Type: 'l', Path: "usr/x", Sub: "p.sw.base", Symval: "x"},
				{Type: 'l', Path: "usr/y", Sub: "p.sw.base", Symval: "t"},
				{Path: "usr/y", Sub: "p.sw.base", Data: "data"},
			}},
			want: []string{"usr/x written usr/x", "usr/x variant usr/x." + h8("x") + " -> x",
				"usr/y variant usr/y." + h8("t") + " -> t", "usr/y written usr/y"},
			files: map[string]string{"usr/y": "data"},
		},
		{
			// An entry beneath the path that is never written does not
			// move the link ahead of the file.
			name: "a skipped entry does not reorder a file and a link",
			prods: map[string][]ent{"p": {
				{Path: "usr/x", Sub: "p.sw.base", Data: "data"},
				{Type: 'l', Path: "usr/x", Sub: "p.sw.base", Symval: "x"},
				{Type: 'c', Path: "usr/x/dev", Sub: "p.sw.base"},
			}},
			want:  []string{"usr/x written usr/x", "usr/x variant usr/x." + h8("x") + " -> x", "usr/x/dev skipped"},
			files: map[string]string{"usr/x": "data"},
		},
		{
			// A path through a directory link can land on a file written
			// under another name; that is a conflict like any other.
			name: "path through an installed link",
			prods: map[string][]ent{"p": {
				{Path: "inc/Mrm/a.h", Sub: "p.sw.base", Data: "header"},
				{Type: 'l', Path: "inc/X11/Mrm", Sub: "p.sw.base", Symval: "../Mrm"},
				{Type: 'l', Path: "inc/X11/Mrm/a.h", Sub: "p.sw.base", Symval: "../Mrm/a.h"},
			}},
			want: []string{"inc/Mrm/a.h written inc/Mrm/a.h", "inc/X11/Mrm written inc/X11/Mrm -> ../Mrm",
				"inc/X11/Mrm/a.h variant inc/X11/Mrm/a.h." + h8("../Mrm/a.h") + " -> ../Mrm/a.h"},
			files: map[string]string{"inc/Mrm/a.h": "header", "inc/X11/Mrm/a.h." + h8("../Mrm/a.h"): "header"},
		},
		{
			// delhist lines name paths an older release installed. They
			// carry no data and contradict the product's own entries:
			// written, they would land through the links that replaced
			// them and block the real files.
			name: "delhist entries are not installed",
			prods: map[string][]ent{"eoe": {
				{Type: 'l', Path: "usr/spool", Sub: "eoe.sw.base", Symval: "../var/spool"},
				{Line: "d 0644 root sys usr/spool/tabs/adm src delhist eoe.sw.acct"},
				{Path: "var/spool/tabs/adm", Sub: "eoe.sw.acct", Data: "tab"},
				{Type: 'd', Path: "var/tmp", Sub: "eoe.sw.base"},
				{Line: "l 0000 root sys var/tmp src eoe.sw.base symval(/XYZ) delhist"},
				{Path: "inc/a.h", Sub: "eoe.sw.base", Data: "h"},
				{Line: "l 0000 root sys inc/a.h src eoe.sw.base symval(../b.h) delhist"},
			}},
			want: []string{"usr/spool written usr/spool -> ../var/spool", "usr/spool/tabs/adm skipped",
				"var/spool/tabs/adm written var/spool/tabs/adm",
				"var/tmp written var/tmp", "var/tmp skipped",
				"inc/a.h written inc/a.h", "inc/a.h skipped"},
			files: map[string]string{"var/spool/tabs/adm": "tab",
				"inc/a.h." + h8("../b.h"): "!"},
		},
		{
			// A product sorting before eoe installs beneath a path eoe
			// installs as a link to a directory. The link is in place
			// first, as on a system the base product is already on.
			name: "entries beneath a link land through it",
			prods: map[string][]ent{
				"apple": {
					{Type: 'd', Path: "usr/adm/apple", Sub: "apple.sw.base"},
					{Path: "usr/adm/apple/vols", Sub: "apple.sw.base", Data: "v"},
				},
				"eoe": {
					{Type: 'l', Path: "usr/adm", Sub: "eoe.sw.base", Symval: "../var/adm"},
					{Type: 'd', Path: "var/adm", Sub: "eoe.sw.base"},
				},
			},
			want: []string{"usr/adm/apple written usr/adm/apple", "usr/adm/apple/vols written usr/adm/apple/vols",
				"usr/adm written usr/adm -> ../var/adm", "var/adm written var/adm"},
			files: map[string]string{"var/adm/apple/vols": "v"},
		},
		{
			name: "a decoder error leaves no partial file",
			prods: map[string][]ent{"eoe": {
				{Path: "usr/x.z", Sub: "eoe.sw.base", Data: string(insttest.Packed('A')[:6])},
				{Path: "usr/ok", Sub: "eoe.sw.base", Data: "ok"},
			}},
			opts:  Options{Unpack: true},
			want:  []string{"usr/x.z error", "usr/ok written usr/ok"},
			files: map[string]string{"usr/x": "!", "usr/x.z": "!", "usr/ok": "ok"},
		},
		{
			name: "hostile install paths",
			prods: map[string][]ent{"eoe": {
				{Path: "../escape", Sub: "eoe.sw.base", Data: "1"},
				{Path: "a/../../escape2", Sub: "eoe.sw.base", Data: "2"},
				{Path: "/../escape3", Sub: "eoe.sw.base", Data: "3"},
				{Path: "/abs/file", Sub: "eoe.sw.base", Data: "4"},
				{Path: "dbl//slash/./x", Sub: "eoe.sw.base", Data: "5"},
				{Path: "a/../fine", Sub: "eoe.sw.base", Data: "6"},
				{Type: 'd', Path: "..", Sub: "eoe.sw.base"},
				{Type: 'l', Path: "../ln", Sub: "eoe.sw.base", Symval: "x"},
			}},
			want: []string{"../escape error", "a/../../escape2 error", "/../escape3 error", "/abs/file written abs/file",
				"dbl//slash/./x written dbl/slash/x", "a/../fine written fine", ".. error", "../ln error"},
			files: map[string]string{"abs/file": "4", "dbl/slash/x": "5", "fine": "6"},
		},
		{
			name:  "unpack",
			prods: map[string][]ent{"eoe": {{Path: "usr/p.z", Sub: "eoe.sw.base", Data: string(insttest.Packed('A'))}}},
			opts:  Options{Unpack: true},
			want:  []string{"usr/p.z written usr/p"},
			files: map[string]string{"usr/p": "AAAA", "usr/p.z": "!"},
		},
		{
			// The first link comes before its target: renaming must not
			// depend on order.
			name: "unpack renames links",
			prods: map[string][]ent{"eoe": {
				{Type: 'l', Path: "man/b.z", Sub: "eoe.man.base", Symval: "a.z"},
				{Path: "man/a.z", Sub: "eoe.man.base", Data: string(insttest.Packed('A'))},
				{Type: 'l', Path: "man/abs.z", Sub: "eoe.man.base", Symval: "/man/a.z"},
				{Type: 'l', Path: "man/up.z", Sub: "eoe.man.base", Symval: "/../man/a.z"},
				{Type: 'l', Path: "man/plain", Sub: "eoe.man.base", Symval: "a.z"},
				{Type: 'l', Path: "man/chain.z", Sub: "eoe.man.base", Symval: "b.z"},
				{Path: "man/raw.z", Sub: "eoe.man.base", Data: "not packed"},
				{Type: 'l', Path: "man/r.z", Sub: "eoe.man.base", Symval: "raw.z"},
				{Type: 'l', Path: "man/out.z", Sub: "eoe.man.base", Symval: "../elsewhere/x.z"},
			}},
			opts: Options{Unpack: true},
			want: []string{"man/b.z written man/b -> a", "man/a.z written man/a", "man/abs.z written man/abs -> a",
				"man/up.z written man/up -> a",
				"man/plain written man/plain -> a", "man/chain.z written man/chain -> b", "man/raw.z written man/raw.z",
				"man/r.z written man/r.z -> raw.z", "man/out.z written man/out.z -> ../elsewhere/x.z"},
			files: map[string]string{"man/chain": "AAAA", "man/abs": "AAAA", "man/r.z": "not packed"},
		},
		{
			// A directory entry for / is the set root, which exists
			// already; a file there would replace it.
			name: "set root",
			prods: map[string][]ent{"eoe": {
				{Type: 'd', Path: "/", Sub: "eoe.sw.base"},
				{Path: "/", Sub: "eoe.sw.base", Data: "x"},
				{Type: 'd', Path: "usr", Sub: "eoe.sw.base"},
			}},
			want: []string{"/ skipped", "/ error", "usr written usr"},
		},
		{
			name: "set root outside --path",
			prods: map[string][]ent{"eoe": {
				{Type: 'd', Path: "/", Sub: "eoe.sw.base"},
				{Type: 'd', Path: "usr", Sub: "eoe.sw.base"},
			}},
			opts: Options{Paths: []string{"usr"}},
			want: []string{"usr written usr"},
		},
		{
			// A later set's product wins a path an earlier product
			// installed, and replaces every machine variant there. A
			// product the later set ships again is replaced whole.
			name: "later set wins",
			base: []map[string][]ent{{
				"dmedia_dev": {
					{Path: "man/a", Sub: "dmedia_dev.man.base", Data: "old"},
					{Path: "man/only", Sub: "dmedia_dev.man.base", Data: "only"},
					{Path: "boot", Sub: "dmedia_dev.sw.base", Data: "ip27", Mach: "CPUBOARD=IP27"},
					{Path: "boot", Sub: "dmedia_dev.sw.base", Data: "ip30", Mach: "CPUBOARD=IP30"},
				},
				"eoe": {{Path: "usr/eoe", Sub: "eoe.sw.base", Data: "old"}, {Path: "usr/retired", Sub: "eoe.sw.base", Data: "old"}},
			}},
			prods: map[string][]ent{
				"dmedia_dev_6530m": {
					{Path: "man/a", Sub: "dmedia_dev_6530m.man.base", Data: "new"},
					{Path: "boot", Sub: "dmedia_dev_6530m.sw.base", Data: "new boot"},
				},
				"eoe": {{Path: "usr/eoe", Sub: "eoe.sw.base", Data: "new"}},
			},
			want: []string{"man/a superseded", "man/only written man/only", "boot superseded", "boot superseded",
				"usr/eoe superseded", "usr/retired superseded", "man/a written man/a", "boot written boot",
				"usr/eoe written usr/eoe"},
			files: map[string]string{"man/a": "new", "boot": "new boot", "boot.CPUBOARD=IP30": "!", "usr/eoe": "new",
				"usr/retired": "!"},
		},
		{
			name:  "file meeting a directory across sets",
			base:  []map[string][]ent{{"base": {{Type: 'd', Path: "x", Sub: "base.sw.base"}, {Path: "y", Sub: "base.sw.base", Data: "y"}}}},
			prods: map[string][]ent{"over": {{Path: "x", Sub: "over.sw.base", Data: "x"}, {Type: 'd', Path: "y", Sub: "over.sw.base"}}},
			want:  []string{"x written x", "y written y", "x error", "y error"},
		},
		{
			// A later set's delhist line neither replaces nor clashes
			// with what an earlier set installs.
			name: "delhist across sets",
			base: []map[string][]ent{{"base": {{Path: "x", Sub: "base.sw.base", Data: "x"}, {Path: "y", Sub: "base.sw.base", Data: "y"}}}},
			prods: map[string][]ent{"over": {
				{Line: "d 0755 root sys x src over.sw.base delhist"},
				{Line: "l 0000 root sys y src over.sw.base symval(z) delhist"},
				{Path: "z", Sub: "over.sw.base", Data: "z"},
			}},
			want:  []string{"x written x", "y written y", "x skipped", "y skipped", "z written z"},
			files: map[string]string{"x": "x", "y": "y"},
		},
		{
			// Links are renamed against the winning pages.
			name: "unpack follows the winner",
			base: []map[string][]ent{{"base": {
				{Path: "man/a.z", Sub: "base.man.base", Data: string(insttest.Packed('A'))},
				{Type: 'l', Path: "man/b.z", Sub: "base.man.base", Symval: "a.z"},
				{Path: "man/c.z", Sub: "base.man.base", Data: "plain"},
				{Type: 'l', Path: "man/d.z", Sub: "base.man.base", Symval: "c.z"},
			}}},
			prods: map[string][]ent{"over": {
				{Path: "man/a.z", Sub: "over.man.base", Data: "plain"},
				{Path: "man/c.z", Sub: "over.man.base", Data: string(insttest.Packed('A'))},
			}},
			opts: Options{Unpack: true},
			want: []string{"man/a.z superseded", "man/b.z written man/b.z -> a.z", "man/c.z superseded",
				"man/d.z written man/d -> c", "man/a.z written man/a.z", "man/c.z written man/c"},
			files: map[string]string{"man/b.z": "plain", "man/d": "AAAA"},
		},
		{
			name: "three sets",
			base: []map[string][]ent{
				{"a": {{Path: "p", Sub: "a.sw.base", Data: "a"}, {Path: "q", Sub: "a.sw.base", Data: "a"}}},
				{"c": {{Path: "q", Sub: "c.sw.base", Data: "c"}, {Path: "p", Sub: "c.sw.base", Data: "c"}}},
			},
			prods: map[string][]ent{"b": {{Path: "p", Sub: "b.sw.base", Data: "b"}}},
			want:  []string{"p superseded", "q superseded", "q written q", "p superseded", "p written p"},
			files: map[string]string{"p": "b", "q": "c"},
		},
		{
			// Superseded entries are never read: their records are absent,
			// so any read would be an error, and their archive is never
			// opened, not even to follow an unpacked link.
			name: "superseded entries are not read",
			base: []map[string][]ent{{"base": {
				{Path: "man/a.z", Sub: "base.man.base", Missing: true},
				{Path: "man/b", Sub: "base.man.base", Missing: true},
			}}},
			prods: map[string][]ent{"over": {
				{Path: "man/a.z", Sub: "over.man.base", Data: string(insttest.Packed('A'))},
				{Path: "man/b", Sub: "over.man.base", Data: "b"},
			}},
			opts:   Options{Unpack: true},
			want:   []string{"man/a.z superseded", "man/b superseded", "man/a.z written man/a", "man/b written man/b"},
			unread: true,
		},
		{
			// A manifest-only run plans from the .idb files alone and
			// writes nothing but the manifest. It honours filters and
			// later sets.
			name: "manifest only",
			base: []map[string][]ent{{"eoe": {{Path: "usr/a", Sub: "eoe.sw.base", Data: "old"}}}},
			prods: map[string][]ent{"eoe": {
				{Type: 'd', Path: "usr", Sub: "eoe.sw.base"},
				{Path: "usr/a", Sub: "eoe.sw.base", Data: "new"},
				{Path: "usr/a", Sub: "eoe.sw.base", Data: "variant"},
				{Type: 'l', Path: "usr/b", Sub: "eoe.sw.base", Symval: "a"},
				{Type: 'X', Path: "usr/x", Sub: "eoe.sw.base"},
				{Path: "usr/missing", Sub: "eoe.sw.base", Missing: true},
				{Path: "usr/man", Sub: "eoe.man.base", Data: "page"},
			}},
			opts: Options{ManifestOnly: true, Subsystems: []string{"*.sw.*"}},
			want: []string{"usr/a superseded", "usr planned", "usr/a planned", "usr/a planned", "usr/b planned",
				"usr/x skipped", "usr/missing planned"},
			unread: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var dists []dist
			var logs []*openLog
			for i, p := range append(c.base, c.prods) {
				logs = append(logs, &openLog{FS: insttest.Dist(p), opened: map[string]bool{}})
				dists = append(dists, openTest(t, fmt.Sprint("b", i), logs[i], nil))
			}
			dists[len(dists)-1].set = "set"
			out, rs, res := run(t, c.opts, dists...)
			var got []string
			count := map[string]int{}
			for _, r := range rs {
				got = append(got, brief(r))
				count[r.Status]++
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("records:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(c.want, "\n"))
			}
			if !reflect.DeepEqual(res, count) {
				t.Errorf("counts %v, records count %v", res, count)
			}
			for p, want := range c.files {
				b, err := os.ReadFile(filepath.Join(out, "set", p))
				if want == "!" {
					if _, err := os.Lstat(filepath.Join(out, "set", p)); err == nil {
						t.Errorf("%s exists", p)
					}
				} else if err != nil || string(b) != want {
					t.Errorf("%s = %q, %v; want %q", p, b, err, want)
				}
			}
			if c.unread {
				if !c.opts.ManifestOnly {
					logs = logs[:len(logs)-1]
				}
				for _, log := range logs {
					for name := range log.opened {
						if !strings.HasSuffix(name, ".idb") && name != "." {
							t.Errorf("opened %s", name)
						}
					}
				}
			}
			// Nothing lands outside the set.
			want := 2
			if c.opts.ManifestOnly {
				want = 1
			}
			if ents, _ := os.ReadDir(out); len(ents) != want {
				t.Errorf("output holds %v", ents)
			}
		})
	}
}

func TestRecordFields(t *testing.T) {
	fsys := insttest.Dist(map[string][]ent{"eoe": {
		{Type: 'd', Mode: "0755", Path: "usr", Sub: "eoe.sw.base"},
		{Mode: "0755", Path: "usr/run", Sub: "eoe.sw.base", Data: "#!/bin/sh\n", Mach: "CPUBOARD=IP27"},
		{Mode: "0444", Path: "usr/page", Sub: "eoe.man.base", Data: "page"},
		{Line: "bad"},
	}})
	out, rs, _ := run(t, Options{}, openTest(t, "set", fsys,
		map[string]string{"eoe.idb": "disc1", "eoe.sw": "disc1", "eoe.man": "disc2"}))
	sum := sha256.Sum256([]byte("#!/bin/sh\n"))
	want := []Record{
		{Path: "usr", InstallPath: "usr", Type: "d", Mode: "0755", Owner: "root", Group: "sys", Product: "eoe",
			Subsystem: "eoe.sw.base", Layer: "disc1", Set: "set", Status: written},
		{Path: "usr/run", InstallPath: "usr/run", Type: "f", Mode: "0755", Owner: "root", Group: "sys", Product: "eoe",
			Subsystem: "eoe.sw.base", Layer: "disc1", Set: "set", SHA256: hex.EncodeToString(sum[:]),
			Mach: "CPUBOARD=IP27", Status: written},
		// A file's layer is that of its archive.
		{Path: "usr/page", InstallPath: "usr/page", Type: "f", Mode: "0444", Owner: "root", Group: "sys", Product: "eoe",
			Subsystem: "eoe.man.base", Layer: "disc2", Set: "set", SHA256: rs[2].SHA256, Status: written},
		{InstallPath: "bad", Product: "eoe", Layer: "disc1", Set: "set", Status: unparsed,
			Error: "line 4: want at least 6 fields, got 1"},
	}
	if !reflect.DeepEqual(rs, want) {
		t.Errorf("records:\n%+v\nwant:\n%+v", rs, want)
	}
	if fi, err := os.Stat(filepath.Join(out, "set/usr/run")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("usr/run: %v %v", fi, err)
	}
	man, _ := os.ReadFile(filepath.Join(out, "set.jsonl"))
	if !strings.HasPrefix(string(man), `{"path":"usr","installPath":"usr","type":"d","mode":"0755",`) {
		t.Errorf("field order: %s", man)
	}
}

// Run refuses, before writing anything, options no run can use, output
// that already exists for any selected set, and a distribution with no
// products.
func TestRunRefuses(t *testing.T) {
	good := insttest.Dist(map[string][]ent{"eoe": {{Path: "a", Sub: "eoe.sw.base", Data: "A"}}})
	cases := []struct {
		name   string
		dists  []fstest.MapFS
		opts   Options
		exists string // made in the output first
	}{
		{"subsystem pattern", []fstest.MapFS{good}, Options{Subsystems: []string{"["}}, ""},
		{"manifest only with unpack", []fstest.MapFS{good}, Options{ManifestOnly: true, Unpack: true}, ""},
		{"existing set directory", []fstest.MapFS{good, good}, Options{}, "s1"},
		{"existing manifest", []fstest.MapFS{good, good}, Options{}, "s1.jsonl"},
		{"no products", []fstest.MapFS{good, insttest.Dist(nil)}, Options{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := t.TempDir()
			want := 0
			if c.exists != "" {
				if err := os.Mkdir(filepath.Join(out, c.exists), 0o755); err != nil {
					t.Fatal(err)
				}
				want = 1
			}
			if err := Run(io.Discard, configOf(t, c.dists...), nil, out, c.opts); err == nil {
				t.Error("accepted")
			}
			if ents, _ := os.ReadDir(out); len(ents) != want {
				t.Errorf("output holds %v", ents)
			}
		})
	}
}

// openLog records which files of a distribution are opened.
type openLog struct {
	fs.FS
	mu     sync.Mutex
	opened map[string]bool
}

func (o *openLog) Open(name string) (fs.File, error) {
	o.mu.Lock()
	o.opened[name] = true
	o.mu.Unlock()
	return o.FS.Open(name)
}
