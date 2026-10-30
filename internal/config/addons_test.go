package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const addonBase = `
server_ip: 192.0.2.10
clients:
  - {name: sgi, mac: "08:00:69:00:00:01", ip: 192.0.2.30}
install_sets:
  - name: base
    layers:
      - {name: os, source: /media/os.image, boot: true}
install_scripts:
  - name: irix
    install: [nfs.sw.nfs]
  - name: other
    then: {sets: [addon-first], install: [first.sw]}
`

func writeAddon(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAddonFilesAddSetAndSecondPass(t *testing.T) {
	dir := t.TempDir()
	writeAddon(t, dir, "first.yaml", `
name: first
layers:
  - {name: first, source: "https://media.example.com/first.tar.gz", base: tree, dist: .}
install: [first.sw, first.man]
`)
	writeAddon(t, dir, "second.yaml", `
name: second
layers:
  - {name: second, source: /media/second.image}
install: [second.sw]
`)
	// "other" already has a second pass over its own add-on set, which the
	// file-listed add-ons join rather than replace.
	cfg, err := ParseIn([]byte("addons: [first.yaml, second.yaml]\n"+addonBase), dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range cfg.InstallSets {
		names = append(names, s.Name)
	}
	if want := []string{"base", "addon-first", "addon-second"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("sets = %v, want %v", names, want)
	}
	first := cfg.InstallSets[1]
	if !first.Addon || !first.Enabled || first.Layers[0].Base != "tree" || first.Layers[0].Dist != "." {
		t.Errorf("addon-first = %+v", first)
	}
	wantThen := &ThenPass{Sets: []string{"addon-first", "addon-second"}, Install: []string{"first.sw", "first.man", "second.sw"}}
	if got := cfg.InstallScripts[0].Then; !reflect.DeepEqual(got, wantThen) {
		t.Errorf("irix then = %+v, want %+v", got, wantThen)
	}
}

func TestAddonFilesRelativeToDirAndAbsolute(t *testing.T) {
	dir := t.TempDir()
	writeAddon(t, dir, "a.yaml", "name: a\nlayers: [{name: a, source: /m/a.image}]\ninstall: [a.sw]\n")
	abs := filepath.Join(dir, "a.yaml")
	if _, err := ParseIn([]byte("addons: [\""+abs+"\"]\n"+strings.Replace(addonBase, "  - name: other\n    then: {sets: [addon-first], install: [first.sw]}\n", "", 1)), t.TempDir()); err != nil {
		t.Fatalf("absolute path: %v", err)
	}
}

func TestAddonFileErrors(t *testing.T) {
	dir := t.TempDir()
	writeAddon(t, dir, "ok.yaml", "name: ok\nlayers: [{name: ok, source: /m/ok.image}]\ninstall: [ok.sw]\n")
	writeAddon(t, dir, "noinstall.yaml", "name: x\nlayers: [{name: x, source: /m/x.image}]\n")
	writeAddon(t, dir, "badname.yaml", "name: a/b\nlayers: [{name: x, source: /m/x.image}]\ninstall: [x]\n")
	writeAddon(t, dir, "typo.yaml", "name: t\nlayers: [{name: t, source: /m/t.image}]\ninstal: [t]\n")
	noScripts := "server_ip: 192.0.2.10\nclients:\n  - {name: sgi, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}\ninstall_sets:\n  - {name: base, layers: [{name: os, source: /m/os.image}]}\n"
	for _, tc := range []struct{ name, cfg, want string }{
		{"missing file", "addons: [nope.yaml]\n" + addonBase, "addons[0]"},
		{"no install", "addons: [noinstall.yaml]\n" + addonBase, "required"},
		{"bad name", "addons: [badname.yaml]\n" + addonBase, "required"},
		{"unknown field", "addons: [typo.yaml]\n" + addonBase, "instal"},
		{"no scripts", "addons: [ok.yaml]\n" + noScripts, "install script"},
		{"duplicate", "addons: [ok.yaml, ok.yaml]\n" + addonBase, "duplicate install set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseIn([]byte(tc.cfg), dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestParseInstallSetsInReadsAddonSets(t *testing.T) {
	dir := t.TempDir()
	writeAddon(t, dir, "a.yaml", "name: a\nlayers: [{name: a, source: /m/a.image}]\ninstall: [a.sw]\n")
	cfg, err := ParseInstallSetsIn([]byte("addons: [a.yaml]\ninstall_sets:\n  - {name: base, layers: [{name: os, source: /m/os.image}]}\n"), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.InstallSets) != 2 || cfg.InstallSets[1].Name != "addon-a" {
		t.Fatalf("sets = %+v", cfg.InstallSets)
	}
}
