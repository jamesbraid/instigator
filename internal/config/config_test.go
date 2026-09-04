package config

import (
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
