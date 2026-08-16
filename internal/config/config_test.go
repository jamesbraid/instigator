package config

import (
	"strings"
	"testing"
)

const sample = `
server_ip: 192.0.2.10
netmask: 192.0.2.0/24
clients:
  - name: octane
    mac: "08:00:69:0e:af:12"
    ip: 192.0.2.30
media:
  - name: "6.5.30"
    discs: /media/6.5.30
    disc_names:
      "IRIX 6.5.30 Overlay 1of3.iso": overlay1
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
	if len(c.Media) != 1 || c.Media[0].DiscNames["IRIX 6.5.30 Overlay 1of3.iso"] != "overlay1" {
		t.Fatalf("media = %+v", c.Media)
	}
	if !c.Services.BOOTP || !c.Services.RSH || c.Services.NFS {
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
media: [{name: m, discs: /media/m}]
`))
	if err != nil {
		t.Fatal(err)
	}
	// services default on except nfs; standard ports
	if !c.Services.BOOTP || !c.Services.TFTP.Enabled || !c.Services.RSH || c.Services.NFS {
		t.Fatalf("service defaults = %+v", c.Services)
	}
	if c.Ports.BOOTP != 67 || c.Ports.TFTP != 69 || c.Ports.RSH != 514 {
		t.Fatalf("port defaults = %+v", c.Ports)
	}
}

func TestRejects(t *testing.T) {
	cases := map[string]string{
		"no clients":  "server_ip: 192.0.2.10\nmedia: [{name: m, discs: /m}]",
		"bad mac":     "server_ip: 192.0.2.10\nclients: [{name: o, mac: nope, ip: 192.0.2.30}]\nmedia: [{name: m, discs: /m}]",
		"bad ip":      "server_ip: 192.0.2.10\nclients: [{name: o, mac: \"08:00:69:00:00:01\", ip: banana}]\nmedia: [{name: m, discs: /m}]",
		"no serverip": "clients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]\nmedia: [{name: m, discs: /m}]",
		"no media":    "server_ip: 192.0.2.10\nclients: [{name: o, mac: \"08:00:69:00:00:01\", ip: 192.0.2.30}]",
	}
	for name, y := range cases {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(strings.ToLower(err.Error()), strings.Split(name, " ")[1]) {
			// error should name the offending field
			t.Logf("%s: error text %q (acceptable, informational)", name, err)
		}
	}
}
