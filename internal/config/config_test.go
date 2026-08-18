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
      - {name: overlays1, image: /media/6.5.30/overlay1.iso, source_dir: ".", target_dir: "."}
      - {name: overlays2, dir: /media/6.5.30/overlay2, source_dir: dist, target_dir: dist}
    collisions:
      "applications/dist/inst.README": overlays1
  - name: "6.5.22"
    enabled: false
    layers:
      - {name: base, image: /media/6.5.22/base.iso}
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
	if l0.Name != "overlays1" || l0.Image != "/media/6.5.30/overlay1.iso" || l0.Dir != "" {
		t.Fatalf("layer 0 = %+v", l0)
	}
	if l0.SourceDir != "." || l0.TargetDir != "." {
		t.Fatalf("layer 0 dirs = %+v", l0)
	}
	if l1.Name != "overlays2" || l1.Dir != "/media/6.5.30/overlay2" || l1.Image != "" {
		t.Fatalf("layer 1 = %+v", l1)
	}
	if l1.SourceDir != "dist" || l1.TargetDir != "dist" {
		t.Fatalf("layer 1 dirs = %+v", l1)
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
	// source_dir/target_dir omitted in YAML: must default to "."
	if second.Layers[0].SourceDir != "." || second.Layers[0].TargetDir != "." {
		t.Fatalf("install_sets[1].Layers[0] default dirs = %+v", second.Layers[0])
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
      - {name: base, image: /media/m/base.iso}
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
			"install_sets: [{name: m, layers: [{name: base, image: /media/m/base.iso}]}]",
		"bad mac": "server_ip: 192.0.2.10\nclients: [{name: o, mac: nope, ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, image: /media/m/base.iso}]}]",
		"bad ip": "server_ip: 192.0.2.10\nclients: [{name: o, mac: \"08:00:69:00:00:01\", ip: banana}]\n" +
			"install_sets: [{name: m, layers: [{name: base, image: /media/m/base.iso}]}]",
		"no serverip": "clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, image: /media/m/base.iso}]}]",
		"no install sets": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]",
		"layer with both image and dir": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, image: /media/m/base.iso, dir: /media/m/base}]}]",
		"layer with neither image nor dir": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base}]}]",
		"duplicate layer names": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: base, image: /media/m/a.iso}, {name: base, image: /media/m/b.iso}]}]",
		// A set name is one directory under the served root, so anything
		// that is not a single path element cannot be one: "." and ".."
		// name the root and its parent, and a slashed name would build a
		// nested root no set owns.
		"set named dot": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: \".\", layers: [{name: base, image: /media/m/base.iso}]}]",
		"set named dotdot": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: \"..\", layers: [{name: base, image: /media/m/base.iso}]}]",
		"set name with a slash": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: a/b, layers: [{name: base, image: /media/m/base.iso}]}]",
		"duplicate set names": "server_ip: 192.0.2.10\n" +
			"clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\n" +
			"install_sets: [{name: m, layers: [{name: a, image: /media/m/a.iso}]}, " +
			"{name: m, layers: [{name: b, image: /media/m/b.iso}]}]",
	}
	for name, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
