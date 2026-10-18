package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jamesbraid/instigator/internal/extract"
	"github.com/jamesbraid/instigator/internal/insttest"
)

type manifestLine struct {
	Path, InstallPath, Product, Subsystem, Layer, Set, Status, Error string
}

func readManifest(t *testing.T, p string) map[string][]manifestLine {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]manifestLine{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var m manifestLine
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("manifest line %q: %v", l, err)
		}
		out[m.InstallPath] = append(out[m.InstallPath], m)
	}
	return out
}

func checkFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, want := range files {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if want == "!" {
			if _, err := os.Lstat(filepath.Join(root, rel)); err == nil {
				t.Errorf("%s exists", rel)
			}
		} else if err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want %q", rel, b, err, want)
		}
	}
}

func writeConfig(t *testing.T, dir, yaml string) string {
	t.Helper()
	p := filepath.Join(dir, "sets.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// discs lays one product out across two directory layers, the way two
// discs split a distribution: disc1 holds the manifest and the sw image,
// disc2 the man image.
func discs(t *testing.T) (disc1, disc2 string) {
	dist := insttest.Dist(map[string][]insttest.Entry{"eoe": {
		{Type: 'd', Path: "usr", Sub: "eoe.sw.base"},
		{Path: "usr/plain", Sub: "eoe.sw.base", Data: "hello"},
		{Path: "usr/packed.z", Sub: "eoe.sw.base", Data: string(insttest.Packed('A'))},
		{Path: "usr/doc", Sub: "eoe.man.base", Data: string(insttest.CompressedAB), Compress: true},
		{Type: 'l', Path: "usr/alias", Sub: "eoe.sw.base", Symval: "plain"},
	}})
	root := t.TempDir()
	disc1, disc2 = filepath.Join(root, "disc1"), filepath.Join(root, "disc2")
	man := dist["eoe.man"]
	delete(dist, "eoe.man")
	insttest.WriteDir(t, filepath.Join(disc1, "dist"), dist)
	insttest.WriteDir(t, filepath.Join(disc2, "dist"), fstest.MapFS{"eoe.man": man})
	return disc1, disc2
}

func TestExtractConfig(t *testing.T) {
	disc1, disc2 := discs(t)
	cfg := writeConfig(t, t.TempDir(), fmt.Sprintf(`install_sets:
  - name: alpha
    layers:
      - {name: disc1, source: %s}
      - {name: disc2, source: %s}
  - name: beta
    layers:
      - {name: only, source: %s}
`, disc1, disc2, disc1))

	out := filepath.Join(t.TempDir(), "out")
	var w bytes.Buffer
	if err := extractCommand(&w, []string{cfg, out}, []string{"alpha"}, "", "", extract.Options{Unpack: true}); err != nil {
		t.Fatalf("%v\n%s", err, w.String())
	}
	// --unpack removes the installed pack layer, and the archive's own
	// compression always comes off.
	checkFiles(t, filepath.Join(out, "alpha"), map[string]string{
		"usr/plain": "hello", "usr/packed": "AAAA", "usr/doc": "ab", "usr/alias": "hello"})
	ms := readManifest(t, filepath.Join(out, "alpha.jsonl"))
	if ms["usr/plain"][0].Layer != "disc1" || ms["usr/doc"][0].Layer != "disc2" {
		t.Errorf("layers: %+v, %+v", ms["usr/plain"], ms["usr/doc"])
	}
	if !strings.HasPrefix(w.String(), "alpha: 5 written,") || strings.Contains(w.String(), "beta") {
		t.Errorf("summary = %q", w.String())
	}

	// With no --set every set runs. beta lacks the man layer, so its man
	// entry fails: the run still writes both sets, then reports beta.
	out = t.TempDir()
	err := extractCommand(io.Discard, []string{cfg, out}, nil, "", "", extract.Options{})
	if err == nil || !strings.Contains(err.Error(), "beta") || strings.Contains(err.Error(), "alpha") {
		t.Errorf("err = %v, want one naming beta only", err)
	}
	if m := readManifest(t, filepath.Join(out, "beta.jsonl"))["usr/doc"]; m[0].Status != "error" {
		t.Errorf("beta usr/doc = %+v", m)
	}
	checkFiles(t, filepath.Join(out, "alpha"), map[string]string{"usr/packed.z": string(insttest.Packed('A'))})
}

func TestExtractDirect(t *testing.T) {
	root := t.TempDir()
	// Two sources with one base name: the layers become disc and disc-2,
	// and the set takes the first one's name.
	one := insttest.WriteDir(t, filepath.Join(root, "x", "disc.image", "dist"),
		insttest.Dist(map[string][]insttest.Entry{"p1": {{Path: "usr/a", Sub: "p1.sw.base", Data: "alpha"}}}))
	two := insttest.WriteDir(t, filepath.Join(root, "y", "disc.image", "dist"),
		insttest.Dist(map[string][]insttest.Entry{"p2": {{Path: "usr/b", Sub: "p2.sw.base", Data: "beta"}}}))
	out := t.TempDir()
	var w bytes.Buffer
	if err := extractCommand(&w, []string{filepath.Dir(one), filepath.Dir(two), out}, nil, "", "", extract.Options{Paths: []string{"/usr/b"}}); err != nil {
		t.Fatalf("%v\n%s", err, w.String())
	}
	checkFiles(t, filepath.Join(out, "disc"), map[string]string{"usr/a": "!", "usr/b": "beta"})
	if m := readManifest(t, filepath.Join(out, "disc.jsonl"))["usr/b"]; m[0].Layer != "disc-2" {
		t.Errorf("usr/b = %+v", m)
	}

	// --dist . reads products at the source root, as in an unpacked
	// tardist; --name names the output.
	out = t.TempDir()
	if err := extractCommand(io.Discard, []string{one, out}, nil, ".", "mine", extract.Options{}); err != nil {
		t.Fatal(err)
	}
	checkFiles(t, filepath.Join(out, "mine"), map[string]string{"usr/a": "alpha"})
}

// A set extracts over the sets it is installed after as one installed
// view, which a later set's products win; the earlier set is not written
// on its own.
func TestExtractAfter(t *testing.T) {
	root := t.TempDir()
	base := insttest.WriteDir(t, filepath.Join(root, "base", "dist"), insttest.Dist(map[string][]insttest.Entry{
		"eoe": {{Path: "usr/eoe", Sub: "eoe.sw.base", Data: "old"}, {Path: "usr/retired", Sub: "eoe.sw.base", Data: "old"}},
		"x":   {{Path: "usr/x", Sub: "x.sw.base", Data: "x"}},
	}))
	over := insttest.WriteDir(t, filepath.Join(root, "over", "dist"), insttest.Dist(map[string][]insttest.Entry{
		"eoe": {{Path: "usr/eoe", Sub: "eoe.sw.base", Data: "new"}},
	}))
	cfg := writeConfig(t, root, fmt.Sprintf(`install_sets:
  - {name: base, layers: [{name: b, source: %s}]}
  - {name: over, after: [base], layers: [{name: o, source: %s}]}
`, filepath.Dir(base), filepath.Dir(over)))
	out := filepath.Join(root, "out")
	var w bytes.Buffer
	if err := extractCommand(&w, []string{cfg, out}, []string{"over"}, "", "", extract.Options{}); err != nil {
		t.Fatalf("%v\n%s", err, w.String())
	}
	if w.String() != "over: 2 written, 0 duplicate, 0 variant, 0 planned, 2 superseded, 0 skipped, 0 errors, 0 unparsed\n" {
		t.Errorf("summary = %q", w.String())
	}
	checkFiles(t, out, map[string]string{"over/usr/eoe": "new", "over/usr/x": "x", "over/usr/retired": "!", "base": "!"})
	if m := readManifest(t, filepath.Join(out, "over.jsonl"))["usr/eoe"]; m[0].Status != "superseded" ||
		m[0].Set != "base" || m[0].Layer != "b" || m[1].Set != "over" {
		t.Errorf("usr/eoe = %+v", m)
	}
}

func TestExtractRefuses(t *testing.T) {
	root := t.TempDir()
	disc1, _ := discs(t)
	cfg := writeConfig(t, root, fmt.Sprintf("install_sets: [{name: alpha, layers: [{name: d, source: %s}]}]\n", disc1))
	flat := insttest.WriteDir(t, filepath.Join(root, "flat"),
		insttest.Dist(map[string][]insttest.Entry{"p1": {{Path: "usr/a", Sub: "p1.sw.base", Data: "a"}}}))
	other := insttest.WriteDir(t, filepath.Join(root, "other", "dist"),
		insttest.Dist(map[string][]insttest.Entry{"eoe": {{Path: "usr/plain", Sub: "eoe.sw.base", Data: "bye"}}}))
	cases := []struct {
		name       string
		srcs, sets []string
		dist, nm   string
		want       string
	}{
		{"no sources", nil, nil, "", "", "output directory"},
		{"--set in direct mode", []string{disc1}, []string{"alpha"}, "", "", "--set"},
		{"--set with two configs", []string{cfg, cfg}, []string{"alpha"}, "", "", "--set"},
		{"--dist in config mode", []string{cfg}, nil, "dist", "", "--dist"},
		{"--name in config mode", []string{cfg}, nil, "", "x", "--name"},
		{"unknown set", []string{cfg}, []string{"alpha", "gamma"}, "", "", "gamma"},
		{"name ..", []string{disc1}, nil, "", "..", "--name"},
		{"name with a slash", []string{disc1}, nil, "", "a/b", "--name"},
		{"no dist directory", []string{flat}, nil, "", "", "dist"},
		{"conflict", []string{disc1, filepath.Dir(other)}, nil, "", "", "collisions"},
	}
	for _, c := range cases {
		out := filepath.Join(t.TempDir(), "out")
		err := extractCommand(io.Discard, append(c.srcs, out), c.sets, c.dist, c.nm, extract.Options{})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want mention of %q", c.name, err, c.want)
		}
		if _, e := os.Stat(out); e == nil {
			t.Errorf("%s: output was created", c.name)
		}
	}
}
