// Package config parses instigator's YAML configuration into validated,
// strongly typed values: addresses as netip types, MACs parsed, service
// toggles and ports defaulted.
package config

import (
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Client is one machine instigator answers.
type Client struct {
	Name string
	MAC  net.HardwareAddr
	IP   netip.Addr
}

// Layer is one ordered source (local path or http(s) URL) contributing
// files to an install set. Base, when set, rebases the layer to a subtree
// of Source. Dist and Stand default to "dist" and "stand" within that
// source; a version-stub disc sets Dist to its real catalog (e.g.
// "dist6.5") to unwrap a .redirect. Boot marks the at-most-one layer per
// set whose stand directory the PROM fetches fx.64 from.
type Layer struct {
	Name   string
	Source string
	Base   string
	Dist   string
	Stand  string
	Sha256 string
	Boot   bool
}

// InstallSet is one served IRIX install tree, assembled by layering Layers
// in the given order. Every configured set is always built and served
// (browsable); Enabled controls only whether the set is offered in the
// generated command file and Inst> instructions. Collisions records, for a
// full logical path written by more than one layer, which layer's copy
// wins.
type InstallSet struct {
	Name       string
	Enabled    bool
	Layers     []Layer
	Collisions map[string]string
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
}

// Ports holds the listening ports, overridable for unprivileged runs.
type Ports struct {
	BOOTP int
	TFTP  int
	RSH   int
}

// Credential supplies HTTP Basic auth to one host when a layer's Source
// is fetched over https. Password may be a literal or an exact "${VAR}"
// environment reference, expanded at parse time.
type Credential struct {
	Host, Username, Password string
}

// Config is a validated instigator configuration.
type Config struct {
	ServerIP    netip.Addr
	Netmask     netip.Prefix
	Clients     []Client
	InstallSets []InstallSet
	Services    Services
	Ports       Ports
	Credentials []Credential
	// CacheDir is where a fetched or extracted remote source is cached;
	// empty means the caller (serve) supplies its own default.
	CacheDir string
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
	Credentials []struct {
		Host     string `yaml:"host"`
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"credentials"`
	CacheDir    string `yaml:"cache_dir"`
	InstallSets []struct {
		Name    string `yaml:"name"`
		Enabled *bool  `yaml:"enabled"`
		Layers  []struct {
			Name   string `yaml:"name"`
			Source string `yaml:"source"`
			Base   string `yaml:"base"`
			Dist   string `yaml:"dist"`
			Stand  string `yaml:"stand"`
			Sha256 string `yaml:"sha256"`
			Boot   bool   `yaml:"boot"`
		} `yaml:"layers"`
		Collisions map[string]string `yaml:"collisions"`
	} `yaml:"install_sets"`
	Services *struct {
		BOOTP *bool `yaml:"bootp"`
		TFTP  *struct {
			Enabled   *bool  `yaml:"enabled"`
			PortRange [2]int `yaml:"port_range"`
		} `yaml:"tftp"`
		RSH *bool `yaml:"rsh"`
	} `yaml:"services"`
	Ports *struct {
		BOOTP int `yaml:"bootp"`
		TFTP  int `yaml:"tftp"`
		RSH   int `yaml:"rsh"`
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
		Ports: Ports{BOOTP: 67, TFTP: 69, RSH: 514},
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

	for _, rc := range r.Credentials {
		c.Credentials = append(c.Credentials, Credential{
			Host:     rc.Host,
			Username: rc.Username,
			Password: expandEnv(rc.Password),
		})
	}
	c.CacheDir = r.CacheDir

	if len(r.InstallSets) == 0 {
		return nil, fmt.Errorf("config: at least one install set is required")
	}
	setNames := make(map[string]bool, len(r.InstallSets))
	for i, rs := range r.InstallSets {
		if rs.Name == "" || len(rs.Layers) == 0 {
			return nil, fmt.Errorf("config: install_sets[%d]: name and at least one layer are required", i)
		}
		// A set is served as one directory directly under the tree root,
		// so its name has to be a single path element. "." and ".." name
		// the root and its parent, and "a/b" would build a nested root no
		// set owns.
		if !fs.ValidPath(rs.Name) || rs.Name == "." || strings.Contains(rs.Name, "/") {
			return nil, fmt.Errorf("config: install_sets[%d] (%s): name must be a single directory name", i, rs.Name)
		}
		if setNames[rs.Name] {
			return nil, fmt.Errorf("config: install_sets[%d]: duplicate install set name %q", i, rs.Name)
		}
		setNames[rs.Name] = true
		enabled := true
		if rs.Enabled != nil {
			enabled = *rs.Enabled
		}
		seen := make(map[string]bool, len(rs.Layers))
		layers := make([]Layer, 0, len(rs.Layers))
		boot := ""
		for j, rl := range rs.Layers {
			if rl.Name == "" {
				return nil, fmt.Errorf("config: install_sets[%d] (%s): layers[%d]: name is required", i, rs.Name, j)
			}
			if seen[rl.Name] {
				return nil, fmt.Errorf("config: install_sets[%d] (%s): layers[%d]: duplicate layer name %q", i, rs.Name, j, rl.Name)
			}
			seen[rl.Name] = true
			if rl.Source == "" {
				return nil, fmt.Errorf("config: install_sets[%d] (%s): layers[%d] (%s): source is required", i, rs.Name, j, rl.Name)
			}
			// One stand directory can be served per set, so two layers
			// claiming the boot role would leave which media the PROM
			// fetches fx.64 from up to layer order.
			if rl.Boot {
				if boot != "" {
					return nil, fmt.Errorf("config: install_sets[%d] (%s): layers[%d] (%s): layer %q already boots this set", i, rs.Name, j, rl.Name, boot)
				}
				boot = rl.Name
			}
			dist := rl.Dist
			if dist == "" {
				dist = "dist"
			}
			layers = append(layers, Layer{
				Name:   rl.Name,
				Source: rl.Source,
				Base:   rl.Base,
				Dist:   dist,
				Stand:  rl.Stand,
				Sha256: rl.Sha256,
				Boot:   rl.Boot,
			})
		}
		c.InstallSets = append(c.InstallSets, InstallSet{
			Name:       rs.Name,
			Enabled:    enabled,
			Layers:     layers,
			Collisions: rs.Collisions,
		})
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
	}
	return c, nil
}

// expandEnv returns os.Getenv(NAME) when v is exactly "${NAME}"; any other
// value, including one merely containing such a reference, passes through
// unchanged. Kept local rather than shared with internal/source so config
// never has to import the source package just to parse credentials.
func expandEnv(v string) string {
	if len(v) < 4 || !strings.HasPrefix(v, "${") || !strings.HasSuffix(v, "}") {
		return v
	}
	return os.Getenv(v[2 : len(v)-1])
}
