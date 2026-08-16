// Package config parses instigator's YAML configuration into validated,
// strongly typed values: addresses as netip types, MACs parsed, service
// toggles and ports defaulted.
package config

import (
	"fmt"
	"net"
	"net/netip"

	"gopkg.in/yaml.v3"
)

// Client is one machine instigator answers.
type Client struct {
	Name string
	MAC  net.HardwareAddr
	IP   netip.Addr
}

// Media is one served set of CD images.
type Media struct {
	Name      string
	Dir       string
	DiscNames map[string]string
}

// TFTPService holds the tftp toggle and its transfer port range.
type TFTPService struct {
	Enabled   bool
	PortRange [2]int
}

// Services holds the per-protocol toggles.
type Services struct {
	BOOTP bool
	TFTP  TFTPService
	RSH   bool
	NFS   bool
}

// Ports holds the listening ports, overridable for unprivileged runs.
type Ports struct {
	BOOTP   int
	TFTP    int
	RSH     int
	Portmap int
	Mount   int
	NFS     int
}

// Combined serves several disc images as one distribution set under
// /<Name>/, each disc kept whole at /<Name>/<slug>/. The primary disc
// (the one carrying dist/.related_dists) gets a synthesized .related_dists
// that chains the rest, so inst auto-opens the whole set from one path.
// Layers is the disc images, in the order they are chained.
type Combined struct {
	Name   string
	Layers []string
}

// Config is a validated instigator configuration.
type Config struct {
	ServerIP netip.Addr
	Netmask  netip.Prefix
	Clients  []Client
	Media    []Media
	Combined []Combined
	Services Services
	Ports    Ports
}

// raw mirrors the YAML shape before validation.
type raw struct {
	ServerIP string `yaml:"server_ip"`
	Netmask  string `yaml:"netmask"`
	Clients  []struct {
		Name string `yaml:"name"`
		MAC  string `yaml:"mac"`
		IP   string `yaml:"ip"`
	} `yaml:"clients"`
	Media []struct {
		Name      string            `yaml:"name"`
		Discs     string            `yaml:"discs"`
		DiscNames map[string]string `yaml:"disc_names"`
	} `yaml:"media"`
	Combined []struct {
		Name   string   `yaml:"name"`
		Layers []string `yaml:"layers"`
	} `yaml:"combined"`
	Services *struct {
		BOOTP *bool `yaml:"bootp"`
		TFTP  *struct {
			Enabled   *bool  `yaml:"enabled"`
			PortRange [2]int `yaml:"port_range"`
		} `yaml:"tftp"`
		RSH *bool `yaml:"rsh"`
		NFS *bool `yaml:"nfs"`
	} `yaml:"services"`
	Ports *struct {
		BOOTP   int `yaml:"bootp"`
		TFTP    int `yaml:"tftp"`
		RSH     int `yaml:"rsh"`
		Portmap int `yaml:"portmap"`
		Mount   int `yaml:"mount"`
		NFS     int `yaml:"nfs"`
	} `yaml:"ports"`
}

// Parse decodes and validates a configuration.
func Parse(b []byte) (*Config, error) {
	var r raw
	if err := yaml.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	c := &Config{
		// SGI PROM tftp wants low source ports; the range caps under
		// 32768 by default
		Services: Services{
			BOOTP: true,
			TFTP:  TFTPService{Enabled: true, PortRange: [2]int{2048, 32767}},
			RSH:   true,
		},
		Ports: Ports{BOOTP: 67, TFTP: 69, RSH: 514, Portmap: 111, Mount: 635, NFS: 2049},
	}

	if r.ServerIP == "" {
		return nil, fmt.Errorf("config: server_ip is required")
	}
	ip, err := netip.ParseAddr(r.ServerIP)
	if err != nil {
		return nil, fmt.Errorf("config: server_ip: %w", err)
	}
	c.ServerIP = ip

	if r.Netmask != "" {
		p, err := netip.ParsePrefix(r.Netmask)
		if err != nil {
			return nil, fmt.Errorf("config: netmask: %w", err)
		}
		c.Netmask = p
	}

	if len(r.Clients) == 0 {
		return nil, fmt.Errorf("config: at least one client is required")
	}
	for i, rc := range r.Clients {
		mac, err := net.ParseMAC(rc.MAC)
		if err != nil {
			return nil, fmt.Errorf("config: clients[%d] (%s): mac: %w", i, rc.Name, err)
		}
		cip, err := netip.ParseAddr(rc.IP)
		if err != nil {
			return nil, fmt.Errorf("config: clients[%d] (%s): ip: %w", i, rc.Name, err)
		}
		c.Clients = append(c.Clients, Client{Name: rc.Name, MAC: mac, IP: cip})
	}

	if len(r.Media) == 0 {
		return nil, fmt.Errorf("config: at least one media set is required")
	}
	for i, rm := range r.Media {
		if rm.Name == "" || rm.Discs == "" {
			return nil, fmt.Errorf("config: media[%d]: name and discs are required", i)
		}
		c.Media = append(c.Media, Media{Name: rm.Name, Dir: rm.Discs, DiscNames: rm.DiscNames})
	}

	for i, rc := range r.Combined {
		if rc.Name == "" || len(rc.Layers) == 0 {
			return nil, fmt.Errorf("config: combined[%d]: name and at least one layer are required", i)
		}
		c.Combined = append(c.Combined, Combined{Name: rc.Name, Layers: rc.Layers})
	}

	if r.Services != nil {
		if r.Services.BOOTP != nil {
			c.Services.BOOTP = *r.Services.BOOTP
		}
		if r.Services.TFTP != nil {
			if r.Services.TFTP.Enabled != nil {
				c.Services.TFTP.Enabled = *r.Services.TFTP.Enabled
			}
			if r.Services.TFTP.PortRange != [2]int{} {
				c.Services.TFTP.PortRange = r.Services.TFTP.PortRange
			}
		}
		if r.Services.RSH != nil {
			c.Services.RSH = *r.Services.RSH
		}
		if r.Services.NFS != nil {
			c.Services.NFS = *r.Services.NFS
		}
	}
	if r.Ports != nil {
		if r.Ports.BOOTP != 0 {
			c.Ports.BOOTP = r.Ports.BOOTP
		}
		if r.Ports.TFTP != 0 {
			c.Ports.TFTP = r.Ports.TFTP
		}
		if r.Ports.RSH != 0 {
			c.Ports.RSH = r.Ports.RSH
		}
		if r.Ports.Portmap != 0 {
			c.Ports.Portmap = r.Ports.Portmap
		}
		if r.Ports.Mount != 0 {
			c.Ports.Mount = r.Ports.Mount
		}
		if r.Ports.NFS != 0 {
			c.Ports.NFS = r.Ports.NFS
		}
	}
	return c, nil
}
