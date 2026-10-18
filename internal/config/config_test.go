package config

import (
	"reflect"
	"testing"
)

const sample = `
server_ip: 192.0.2.10
netmask: 192.0.2.0/24
clients:
  - name: octane
    mac: "08:00:69:0e:af:12"
    ip: 192.0.2.30
install_sets:
  - name: "6.5.30"
    layers:
      - {name: overlays1, source: /media/6.5.30/overlay1.iso, boot: true}
      - {name: overlays2, source: /media/6.5.30/overlay2, dist: dist6.5}
    collisions:
      "applications/dist/inst.README": overlays1
    replacements: {overlays1: overlays2}
  - name: "6.5.22"
    enabled: false
    layers:
      - {name: base, source: /media/6.5.22/base.iso}
services:
  bootp: true
  tftp:
    port_range: [2048, 32767]
  rsh: true
`

func TestParseSample(t *testing.T) {
	c, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if c.ServerIP.String() != "192.0.2.10" {
		t.Fatalf("server_ip = %s", c.ServerIP)
	}
	if c.Netmask.Bits() != 24 {
		t.Fatalf("netmask = %s", c.Netmask)
	}
	if len(c.Clients) != 1 || c.Clients[0].MAC.String() != "08:00:69:0e:af:12" {
		t.Fatalf("clients = %+v", c.Clients)
	}
	if c.Clients[0].IP.String() != "192.0.2.30" {
		t.Fatalf("client ip = %s", c.Clients[0].IP)
	}

	if len(c.InstallSets) != 2 {
		t.Fatalf("install sets = %+v", c.InstallSets)
	}

	first := c.InstallSets[0]
	if first.Name != "6.5.30" {
		t.Fatalf("install_sets[0].Name = %q", first.Name)
	}
	if !first.Enabled {
		t.Fatalf("install_sets[0].Enabled should default true, got %+v", first)
	}
	if len(first.Layers) != 2 {
		t.Fatalf("install_sets[0].Layers = %+v", first.Layers)
	}
	l0, l1 := first.Layers[0], first.Layers[1]
	if l0.Name != "overlays1" || l0.Source != "/media/6.5.30/overlay1.iso" {
		t.Fatalf("layer 0 = %+v", l0)
	}
	// dist omitted: the ordinary "dist" every layer merges into.
	if l0.Dist != "dist" || !l0.Boot {
		t.Fatalf("layer 0 dist/boot = %+v", l0)
	}
	if l1.Name != "overlays2" || l1.Source != "/media/6.5.30/overlay2" {
		t.Fatalf("layer 1 = %+v", l1)
	}
	// A version-stub layer names the catalog it really carries; only the
	// primary layer boots.
	if l1.Dist != "dist6.5" || l1.Boot {
		t.Fatalf("layer 1 dist/boot = %+v", l1)
	}
	if len(first.Collisions) != 1 || first.Collisions["applications/dist/inst.README"] != "overlays1" {
		t.Fatalf("install_sets[0].Collisions = %+v", first.Collisions)
	}

	if first.Replacements["overlays1"] != "overlays2" {
		t.Fatalf("install_sets[0].Replacements = %+v", first.Replacements)
	}

	second := c.InstallSets[1]
	if second.Name != "6.5.22" {
		t.Fatalf("install_sets[1].Name = %q", second.Name)
	}
	if second.Enabled {
		t.Fatalf("install_sets[1].Enabled should be false, got %+v", second)
	}
	if len(second.Layers) != 1 {
		t.Fatalf("install_sets[1].Layers = %+v", second.Layers)
	}
	// dist omitted in YAML: must default to "dist", and a set nobody
	// netboots carries no boot layer at all.
	if second.Layers[0].Dist != "dist" || second.Layers[0].Boot {
		t.Fatalf("install_sets[1].Layers[0] default dist/boot = %+v", second.Layers[0])
	}

	if !c.Services.BOOTP || !c.Services.RSH {
		t.Fatalf("services = %+v", c.Services)
	}
	if c.Services.TFTP.PortRange != [2]int{2048, 32767} {
		t.Fatalf("tftp = %+v", c.Services.TFTP)
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(`
server_ip: 192.0.2.10
clients: [{name: o, mac: "08:00:69:00:00:01", ip: 192.0.2.30}]
install_sets:
  - name: m
    layers:
      - {name: base, source: /media/m/base.iso}
`))
	if err != nil {
		t.Fatal(err)
	}
	// services default on; standard ports. Config.Services and Config.Ports no
	// longer have NFS-related fields at all (nfs, portmap, mount) — removed
	// from the type, not merely defaulted off.
	if !c.Services.BOOTP || !c.Services.TFTP.Enabled || !c.Services.RSH {
		t.Fatalf("service defaults = %+v", c.Services)
	}
	if c.Ports.BOOTP != 67 || c.Ports.TFTP != 69 || c.Ports.RSH != 514 {
		t.Fatalf("port defaults = %+v", c.Ports)
	}
	if !c.InstallSets[0].Enabled {
		t.Fatalf("install set enabled should default true, got %+v", c.InstallSets[0])
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]string{
		"no clients": "server_ip: 192.0.2.10\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}]",
		"bad mac": "server_ip: 192.0.2.10\nclients: [{name: o, mac: nope, ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}]",
		"bad ip": "server_ip: 192.0.2.10\nclients: [{name: o, mac: \"08:00:69:00:00:01\", ip: banana}]\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}]",
		"no serverip": "clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}]",
		"no install sets": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]",
		"layer with no source": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base}]}]",
		// image:/dir: are the retired field names; a config still using them
		// carries no source at all, so it fails the same way an empty layer
		// does rather than being silently accepted.
		"layer using retired image key": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, image: /media/m/base.iso}]}]",
		"layer using retired dir key": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, dir: /media/m/base}]}]",
		"duplicate layer names": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/a.iso}, {name: base, source: /media/m/b.iso}]}]",
		// A set name is one directory under the served root, so anything
		// that is not a single path element cannot be one: "." and ".."
		// name the root and its parent, and a slashed name would build a
		// nested root no set owns.
		"set named dot": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: \".\", layers: [{name: base, source: /media/m/base.iso}]}]",
		"set named dotdot": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: \"..\", layers: [{name: base, source: /media/m/base.iso}]}]",
		"set name with a slash": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: a/b, layers: [{name: base, source: /media/m/base.iso}]}]",
		// Only one stand directory can be served per set, so two layers
		// claiming it would leave the PROM's fx.64 to layer order.
		"two boot layers in one set": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: a, source: /media/m/a.iso, boot: true}, " +
			"{name: b, source: /media/m/b.iso, boot: true}]}]",
		"duplicate set names": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: a, source: /media/m/a.iso}]}, " +
			"{name: m, layers: [{name: b, source: /media/m/b.iso}]}]",
		// install.cmds is always generated for the baseline, so no configured
		// script may claim the "install" name.
		"script named install (reserved)": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}]\n" +
			"install_scripts: [{name: install, install: [dev.sw.dbx]}]",
		"duplicate script names": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}]\n" +
			"install_scripts: [{name: dbg, install: [a]}, {name: dbg, install: [b]}]",
		// A script name is one file directly under the served root, same rule
		// as a set name.
		"script name with a slash": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}]\n" +
			"install_scripts: [{name: a/b, install: [dev.sw.dbx]}]",
		"script with unknown stream": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}]\n" +
			"install_scripts: [{name: dbg, stream: nightly}]",
		// A then pass installs add-ons only, after the first pass has
		// installed the operating system from every other set.
		"then naming a set that is not an addon": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}]\n" +
			"install_scripts: [{name: dbg, then: {sets: [m], install: [a]}}]",
		"then naming an unknown set": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}]\n" +
			"install_scripts: [{name: dbg, then: {sets: [x], install: [a]}}]",
		"then naming a disabled addon": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}, " +
			"{name: x, addon: true, enabled: false, layers: [{name: x, source: /media/x.tar.gz}]}]\n" +
			"install_scripts: [{name: dbg, then: {sets: [x], install: [a]}}]",
		"then without sets": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}, " +
			"{name: x, addon: true, layers: [{name: x, source: /media/x.tar.gz}]}]\n" +
			"install_scripts: [{name: dbg, then: {install: [a]}}]",
		"then without install": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}, " +
			"{name: x, addon: true, layers: [{name: x, source: /media/x.tar.gz}]}]\n" +
			"install_scripts: [{name: dbg, then: {sets: [x]}}]",
		"then entry with a newline": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}, " +
			"{name: x, addon: true, layers: [{name: x, source: /media/x.tar.gz}]}]\n" +
			"install_scripts: [{name: dbg, then: {sets: [x], install: [\"a\\nb\"]}}]",
		// A newline in a selection entry would break inst's one-command-per-line
		// command file.
		"script entry with a newline": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}]\n" +
			"install_scripts: [{name: dbg, install: [\"a\\nb\"]}]",
		// inst runs each post-install command from one command-file line.
		"post_install entry with a newline": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}]\n" +
			"install_scripts: [{name: dbg, post_install: [\"true\\ntrue\"]}]",
		"blank post_install entry": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}]\n" +
			"install_scripts: [{name: dbg, post_install: [\" \"]}]",
	}
	for name, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseSourceAndCredentials(t *testing.T) {
	t.Setenv("FJ", "tok")
	cfg, err := Parse([]byte(`
server_ip: 10.0.0.1
clients: [{name: a, mac: "02:00:00:00:00:01", ip: 10.0.0.2}]
credentials:
  - host: forge.example
    username: ci
    password: ${FJ}
install_sets:
  - name: "6.5.30"
    layers:
      - name: disc1
        source: https://forge.example/x/disc1.tar.gz
        base: disc1
`))
	if err != nil {
		t.Fatal(err)
	}
	l := cfg.InstallSets[0].Layers[0]
	if l.Source == "" || l.Base != "disc1" {
		t.Fatalf("layer = %+v", l)
	}
	if len(cfg.Credentials) != 1 || cfg.Credentials[0].Password != "tok" {
		t.Fatalf("creds = %+v", cfg.Credentials)
	}
}

func TestCredentialLiteralPassword(t *testing.T) {
	// A password that isn't a "${VAR}" reference passes through unchanged -
	// expandEnv only touches the exact-match case.
	cfg, err := Parse([]byte(`
server_ip: 10.0.0.1
clients: [{name: a, mac: "02:00:00:00:00:01", ip: 10.0.0.2}]
credentials:
  - {host: forge.example, username: ci, password: "not-a-var-ref"}
  - {host: other.example, username: bot, password: "${NOT_SET_ANYWHERE_12345}"}
install_sets:
  - name: m
    layers: [{name: base, source: /media/m/base.iso}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Credentials) != 2 {
		t.Fatalf("creds = %+v", cfg.Credentials)
	}
	if cfg.Credentials[0].Password != "not-a-var-ref" {
		t.Fatalf("literal password mangled: %+v", cfg.Credentials[0])
	}
	// An unset env var expands to "", same as os.Getenv would report.
	if cfg.Credentials[1].Password != "" {
		t.Fatalf("unset-var password = %q, want empty", cfg.Credentials[1].Password)
	}
}

func TestParseInstallScripts(t *testing.T) {
	cfg, err := Parse([]byte(`
server_ip: 10.0.0.1
clients: [{name: a, mac: "02:00:00:00:00:01", ip: 10.0.0.2}]
install_sets:
  - name: m
    layers: [{name: base, source: /media/m/base.iso}]
install_scripts:
  - name: debug
    install: [dev.sw.dbx, dev.sw.gdb]
    keep: [java_dev.sw.base]
  - name: maint
    stream: maintenance
    remove: [old.sw.thing]
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.InstallScripts) != 2 {
		t.Fatalf("install_scripts = %+v", cfg.InstallScripts)
	}
	d := cfg.InstallScripts[0]
	if d.Name != "debug" {
		t.Fatalf("script 0 name = %q", d.Name)
	}
	if len(d.Install) != 2 || d.Install[0] != "dev.sw.dbx" || d.Install[1] != "dev.sw.gdb" {
		t.Fatalf("script 0 install = %+v", d.Install)
	}
	if len(d.Keep) != 1 || d.Keep[0] != "java_dev.sw.base" {
		t.Fatalf("script 0 keep = %+v", d.Keep)
	}
	// stream omitted: the feature stream inst runs by default.
	if d.Stream != "" {
		t.Fatalf("script 0 stream default = %q, want empty", d.Stream)
	}
	m := cfg.InstallScripts[1]
	if m.Name != "maint" || m.Stream != "maintenance" {
		t.Fatalf("script 1 = %+v", m)
	}
	if len(m.Remove) != 1 || m.Remove[0] != "old.sw.thing" {
		t.Fatalf("script 1 remove = %+v", m.Remove)
	}
}

func TestParseAddonSetAndThenPass(t *testing.T) {
	cfg, err := Parse([]byte(`
server_ip: 10.0.0.1
clients: [{name: a, mac: "02:00:00:00:00:01", ip: 10.0.0.2}]
install_sets:
  - name: m
    layers: [{name: base, source: /media/m/base.iso}]
  - name: addon-rad4
    addon: true
    layers: [{name: rad4, source: /media/rad4x.tar.gz, dist: dist_6.5}]
install_scripts:
  - name: addon-rad4
    then:
      sets: [addon-rad4]
      install: [rad4x.sw.driver, rad4x.man]
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InstallSets[0].Addon || !cfg.InstallSets[1].Addon || !cfg.InstallSets[1].Enabled {
		t.Fatalf("install sets = %+v", cfg.InstallSets)
	}
	then := cfg.InstallScripts[0].Then
	if then == nil || len(then.Sets) != 1 || then.Sets[0] != "addon-rad4" ||
		len(then.Install) != 2 || then.Install[1] != "rad4x.man" {
		t.Fatalf("then = %+v", then)
	}
}

func TestParsePostInstall(t *testing.T) {
	cfg, err := Parse([]byte(`
server_ip: 10.0.0.1
clients: [{name: a, mac: "02:00:00:00:00:01", ip: 10.0.0.2}]
install_sets: [{name: m, layers: [{name: base, source: /media/m/base.iso}]}]
install_scripts:
  - name: dhcp
    post_install:
      - chkconfig -f autoconfig_ipaddress on
      - grep -q x /etc/hosts || echo "a; b" >> /etc/motd
`))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.InstallScripts[0].PostInstall
	if len(got) != 2 || got[0] != "chkconfig -f autoconfig_ipaddress on" || got[1] != `grep -q x /etc/hosts || echo "a; b" >> /etc/motd` {
		t.Fatalf("post_install = %q", got)
	}
}

func TestInstallScriptsDefaultEmpty(t *testing.T) {
	cfg, err := Parse([]byte(`
server_ip: 10.0.0.1
clients: [{name: a, mac: "02:00:00:00:00:01", ip: 10.0.0.2}]
install_sets:
  - name: m
    layers: [{name: base, source: /media/m/base.iso}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.InstallScripts) != 0 {
		t.Fatalf("install_scripts default = %+v, want empty", cfg.InstallScripts)
	}
}

func TestCacheDir(t *testing.T) {
	cfg, err := Parse([]byte(`
server_ip: 10.0.0.1
clients: [{name: a, mac: "02:00:00:00:00:01", ip: 10.0.0.2}]
cache_dir: /var/cache/instigator
install_sets:
  - name: m
    layers: [{name: base, source: /media/m/base.iso}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CacheDir != "/var/cache/instigator" {
		t.Fatalf("cache_dir = %q", cfg.CacheDir)
	}

	// Unset stays empty - serve supplies its own default.
	cfg2, err := Parse([]byte(`
server_ip: 10.0.0.1
clients: [{name: a, mac: "02:00:00:00:00:01", ip: 10.0.0.2}]
install_sets:
  - name: m
    layers: [{name: base, source: /media/m/base.iso}]
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.CacheDir != "" {
		t.Fatalf("cache_dir default = %q, want empty", cfg2.CacheDir)
	}
}

func TestParseRejectsUnorderedReplacement(t *testing.T) {
	for _, replacement := range []string{"{base: absent}", "{absent: update}", "{update: base}"} {
		_, err := Parse([]byte(`server_ip: 192.0.2.10
clients: [{name: o, mac: "08:00:69:00:00:01", ip: 192.0.2.30}]
install_sets:
  - name: dev
    layers: [{name: base, source: /base}, {name: update, source: /update}]
    replacements: ` + replacement))
		if err == nil {
			t.Fatalf("accepted %s", replacement)
		}
	}
}

const serverPrefix = "server_ip: 192.0.2.10\n" +
	"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n"

// ParseInstallSets reads a file of install sets alone, which Parse
// refuses, and the sets of a full configuration alike. It ignores the
// server's settings, even an invalid install script.
func TestParseInstallSets(t *testing.T) {
	const sets = `
install_scripts: [{name: install}]
cache_dir: /var/x
credentials: [{host: h.example, username: u, password: "${EXTRACT_TEST_PW}"}]
install_sets:
  - name: "6.5.30"
    layers:
      - {name: f1, source: /f1.iso, boot: true}
      - {name: ov1, source: /media/ov1, dist: dist6.5}
    collisions: {"a/b": f1}
    replacements: {f1: ov1}
    after: [spare]
  - name: spare
    enabled: false
    layers: [{name: base, source: /media/spare}]
`
	t.Setenv("EXTRACT_TEST_PW", "secret")
	cfg, err := ParseInstallSets([]byte(sets))
	if err != nil {
		t.Fatal(err)
	}
	want := []InstallSet{
		{Name: "6.5.30", Enabled: true, Layers: []Layer{
			{Name: "f1", Source: "/f1.iso", Dist: "dist", Boot: true},
			{Name: "ov1", Source: "/media/ov1", Dist: "dist6.5"},
		}, Collisions: map[string]string{"a/b": "f1"}, Replacements: map[string]string{"f1": "ov1"}, After: []string{"spare"}},
		{Name: "spare", Layers: []Layer{{Name: "base", Source: "/media/spare", Dist: "dist"}}},
	}
	if !reflect.DeepEqual(cfg.InstallSets, want) {
		t.Fatalf("sets = %+v\nwant %+v", cfg.InstallSets, want)
	}
	if cfg.CacheDir != "/var/x" || len(cfg.Credentials) != 1 || cfg.Credentials[0] != (Credential{Host: "h.example", Username: "u", Password: "secret"}) {
		t.Errorf("cache_dir and credentials = %q, %+v", cfg.CacheDir, cfg.Credentials)
	}
	if _, err := Parse([]byte(sets)); err == nil {
		t.Error("Parse accepted a file with no server_ip or clients")
	}
	full, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseInstallSets([]byte(sample)); err != nil || !reflect.DeepEqual(got.InstallSets, full.InstallSets) {
		t.Errorf("full config: %+v, %v; Parse gives %+v", got, err, full.InstallSets)
	}
}

// ParseInstallSets applies the install-set rules TestRejects covers for
// Parse; the after key adds two of its own.
func TestInstallSetRules(t *testing.T) {
	cases := map[string]string{
		"no install sets":      "cache_dir: /x",
		"layer with no source": "install_sets: [{name: m, layers: [{name: base}]}]",
		"unknown after":        "install_sets: [{name: m, after: [x], layers: [{name: a, source: /a}]}]",
		"after cycle": "install_sets: [{name: m, after: [n], layers: [{name: a, source: /a}]}, " +
			"{name: n, after: [m], layers: [{name: b, source: /b}]}]",
	}
	for name, y := range cases {
		if _, err := ParseInstallSets([]byte(y)); err == nil {
			t.Errorf("%s: ParseInstallSets accepted", name)
		}
		if _, err := Parse([]byte(serverPrefix + y)); err == nil {
			t.Errorf("%s: Parse accepted", name)
		}
	}
}

// A set is installed after every set its after key names, transitively, and
// each set once.
func TestInstallOrder(t *testing.T) {
	cfg, err := ParseInstallSets([]byte(`install_sets:
  - {name: top, after: [mid, root], layers: [{name: a, source: /a}]}
  - {name: mid, after: [root], layers: [{name: b, source: /b}]}
  - {name: root, layers: [{name: c, source: /c}]}
`))
	if err != nil {
		t.Fatal(err)
	}
	sets, err := cfg.InstallOrder("top")
	var got []string
	for _, s := range sets {
		got = append(got, s.Name)
	}
	if err != nil || !reflect.DeepEqual(got, []string{"root", "mid", "top"}) {
		t.Errorf("order = %v, %v", got, err)
	}
	if _, err := cfg.InstallOrder("absent"); err == nil {
		t.Error("an unknown install set has an order")
	}
}
