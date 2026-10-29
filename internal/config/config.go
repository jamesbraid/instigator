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
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Client is one machine instigator answers.
type Client struct {
	Name string
	MAC  net.HardwareAddr
	IP   netip.Addr
	// Hostname, when set, is offered to the machine in the BOOTP reply
	// (RFC 2132 option 12) as the name it should take. Empty offers none.
	Hostname string
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
	Boot   bool
}

// InstallSet is one served IRIX install tree, assembled by layering Layers
// in the given order. Every configured set is always built and served
// (browsable); Enabled controls only whether the set is offered in the
// generated command file and Inst> instructions. Collisions records, for a
// full logical path written by more than one layer, which layer's copy
// wins. Replacements names a later layer that replaces files from an
// earlier layer across the set; an exact collision winner takes precedence.
// Addon marks a set that no first pass opens: only a script's Then pass
// installs from it, after the operating system is on the target.
// After names the install sets this one is installed after, such as a base
// release under its overlays. Only extraction reads it; see InstallOrder.
type InstallSet struct {
	Name         string
	Enabled      bool
	Addon        bool
	Layers       []Layer
	Collisions   map[string]string
	Replacements map[string]string
	After        []string
}

// InstallScript is one named inst command file generated in addition to the
// baseline install.cmds. Its selections extend the proven baseline (keep *,
// install standard, the Java conflict keep) rather than replace it: Install,
// Keep, and Remove add inst install/keep/remove lines. Stream selects the IRIX
// release stream - "" or "feature" is the default feature stream and emits no
// extra directive, "maintenance" emits "install maint". The script is served at
// /<Name>.cmds. Then, when set, is a second pass run after the first go
// returns. PostInstall lists shell commands run in the installed system
// after the last go returns.
type InstallScript struct {
	Name        string
	Install     []string
	Keep        []string
	Remove      []string
	Stream      string
	Then        *ThenPass
	PostInstall []string
}

// ThenPass opens add-on sets after the first pass has installed the
// operating system and installs only the named products from them. An
// add-on whose files go below paths that the operating system makes
// symbolic links, such as /var/sysgen/boot, needs those links in place
// first, which a single pass does not guarantee.
type ThenPass struct {
	Sets    []string
	Install []string
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
	ServerIP       netip.Addr
	Netmask        netip.Prefix
	Clients        []Client
	InstallSets    []InstallSet
	InstallScripts []InstallScript
	Services       Services
	Ports          Ports
	Credentials    []Credential
	// CacheDir is where a fetched or extracted remote source is cached;
	// empty means the default chosen where sources are opened.
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
		Host string `yaml:"hostname"`
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
		Addon   bool   `yaml:"addon"`
		Layers  []struct {
			Name   string `yaml:"name"`
			Source string `yaml:"source"`
			Base   string `yaml:"base"`
			Dist   string `yaml:"dist"`
			Stand  string `yaml:"stand"`
			Boot   bool   `yaml:"boot"`
		} `yaml:"layers"`
		Collisions   map[string]string `yaml:"collisions"`
		Replacements map[string]string `yaml:"replacements"`
		After        []string          `yaml:"after"`
	} `yaml:"install_sets"`
	InstallScripts []struct {
		Name    string   `yaml:"name"`
		Install []string `yaml:"install"`
		Keep    []string `yaml:"keep"`
		Remove  []string `yaml:"remove"`
		Stream  string   `yaml:"stream"`
		Then    *struct {
			Sets    []string `yaml:"sets"`
			Install []string `yaml:"install"`
		} `yaml:"then"`
		PostInstall []string `yaml:"post_install"`
	} `yaml:"install_scripts"`
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
	c, err := r.installSets()
	if err != nil {
		return nil, err
	}
	// SGI PROM tftp wants low source ports; the range caps under
	// 32768 by default
	c.Services = Services{
		BOOTP: true,
		TFTP:  TFTPService{Enabled: true, PortRange: [2]int{2048, 32767}},
		RSH:   true,
	}
	c.Ports = Ports{BOOTP: 67, TFTP: 69, RSH: 514}

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
		if err := checkHostname(rc.Host); err != nil {
			return nil, fmt.Errorf("config: clients[%d] (%s): hostname: %w", i, rc.Name, err)
		}
		c.Clients = append(c.Clients, Client{Name: rc.Name, MAC: mac, IP: cip, Hostname: rc.Host})
	}

	addonSets := make(map[string]bool)
	for _, set := range c.InstallSets {
		if set.Addon && set.Enabled {
			addonSets[set.Name] = true
		}
	}
	scriptNames := make(map[string]bool, len(r.InstallScripts))
	for i, rsc := range r.InstallScripts {
		if rsc.Name == "" {
			return nil, fmt.Errorf("config: install_scripts[%d]: name is required", i)
		}
		// A script is served as one file directly under the tree root, so its
		// name has the same single-path-element rule as a set name.
		if !fs.ValidPath(rsc.Name) || rsc.Name == "." || strings.Contains(rsc.Name, "/") {
			return nil, fmt.Errorf("config: install_scripts[%d] (%s): name must be a single directory name", i, rsc.Name)
		}
		// install.cmds is always generated for the baseline selection, so no
		// configured script may claim that name.
		if rsc.Name == "install" {
			return nil, fmt.Errorf("config: install_scripts[%d]: name %q is reserved for the baseline install.cmds", i, rsc.Name)
		}
		if scriptNames[rsc.Name] {
			return nil, fmt.Errorf("config: install_scripts[%d]: duplicate install script name %q", i, rsc.Name)
		}
		scriptNames[rsc.Name] = true
		switch rsc.Stream {
		case "", "feature", "maintenance":
		default:
			return nil, fmt.Errorf("config: install_scripts[%d] (%s): stream %q must be \"feature\" or \"maintenance\"", i, rsc.Name, rsc.Stream)
		}
		var then *ThenPass
		var thenInstall []string
		if rsc.Then != nil {
			if len(rsc.Then.Sets) == 0 || len(rsc.Then.Install) == 0 {
				return nil, fmt.Errorf("config: install_scripts[%d] (%s): then needs at least one set and one install entry", i, rsc.Name)
			}
			for _, name := range rsc.Then.Sets {
				if !addonSets[name] {
					return nil, fmt.Errorf("config: install_scripts[%d] (%s): then set %q is not an enabled addon set", i, rsc.Name, name)
				}
			}
			then = &ThenPass{Sets: rsc.Then.Sets, Install: rsc.Then.Install}
			thenInstall = rsc.Then.Install
		}
		// Each selection entry becomes one inst command line, so an empty or
		// multi-line token would corrupt the generated command file.
		for _, g := range []struct {
			field   string
			entries []string
		}{{"install", rsc.Install}, {"keep", rsc.Keep}, {"remove", rsc.Remove}, {"then.install", thenInstall}} {
			for _, e := range g.entries {
				if e == "" || strings.ContainsAny(e, "\n\r") {
					return nil, fmt.Errorf("config: install_scripts[%d] (%s): %s entry %q must be a single non-empty token", i, rsc.Name, g.field, e)
				}
			}
		}
		// inst runs each post-install command from one command-file line.
		for _, cmd := range rsc.PostInstall {
			if strings.TrimSpace(cmd) == "" || strings.ContainsAny(cmd, "\n\r") {
				return nil, fmt.Errorf("config: install_scripts[%d] (%s): post_install entry %q must be a single non-empty line", i, rsc.Name, cmd)
			}
		}
		c.InstallScripts = append(c.InstallScripts, InstallScript{
			Name:        rsc.Name,
			Install:     rsc.Install,
			Keep:        rsc.Keep,
			Remove:      rsc.Remove,
			Stream:      rsc.Stream,
			Then:        then,
			PostInstall: rsc.PostInstall,
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

// ParseInstallSets decodes and validates only what building install sets
// reads: the sets, and the credentials and cache directory their sources
// are fetched with. Every other key is ignored, so a file holding only
// install_sets is accepted. Extraction uses it.
func ParseInstallSets(b []byte) (*Config, error) {
	var r raw
	if err := yaml.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return r.installSets()
}

// installSets decodes and validates the install sets, and the credentials
// and cache directory their sources are fetched with.
func (r *raw) installSets() (*Config, error) {
	c := &Config{}
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
		if !IsDirName(rs.Name) {
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
				Boot:   rl.Boot,
			})
		}
		for from, to := range rs.Replacements {
			fromIndex, toIndex := -1, -1
			for index, layer := range layers {
				if layer.Name == from {
					fromIndex = index
				}
				if layer.Name == to {
					toIndex = index
				}
			}
			if fromIndex < 0 || toIndex <= fromIndex {
				return nil, fmt.Errorf("config: install_sets[%d] (%s): replacement %q -> %q must name layers in source-then-replacement order", i, rs.Name, from, to)
			}
		}
		c.InstallSets = append(c.InstallSets, InstallSet{
			Name:         rs.Name,
			Enabled:      enabled,
			Addon:        rs.Addon,
			Layers:       layers,
			Collisions:   rs.Collisions,
			Replacements: rs.Replacements,
			After:        rs.After,
		})
	}

	for _, set := range c.InstallSets {
		if _, err := c.InstallOrder(set.Name); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// InstallOrder returns the install set name and the sets it is installed
// over, each once, every set after the sets its After names. It fails on an
// unknown name or a cycle.
func (c *Config) InstallOrder(name string) ([]InstallSet, error) {
	var order []InstallSet
	done, visiting := map[string]bool{}, map[string]bool{}
	var visit func(name, from string) error
	visit = func(name, from string) error {
		switch {
		case done[name]:
			return nil
		case visiting[name]:
			return fmt.Errorf("config: install set %q: after %q forms a cycle", from, name)
		}
		i := slices.IndexFunc(c.InstallSets, func(s InstallSet) bool { return s.Name == name })
		switch {
		case i < 0 && from == "":
			return fmt.Errorf("config: no install set %q", name)
		case i < 0:
			return fmt.Errorf("config: install set %q: after %q is not an install set", from, name)
		}
		visiting[name] = true
		for _, b := range c.InstallSets[i].After {
			if err := visit(b, name); err != nil {
				return err
			}
		}
		done[name] = true
		order = append(order, c.InstallSets[i])
		return nil
	}
	if err := visit(name, ""); err != nil {
		return nil, err
	}
	return order, nil
}

// IsDirName reports whether name can be served directly under the tree
// root: a single path element, not "." or "..".
func IsDirName(name string) bool {
	return fs.ValidPath(name) && name != "." && !strings.Contains(name, "/")
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

// maxHostname keeps option 12 inside the 64-byte BOOTP vendor area beside
// the cookie, the subnet mask and the end marker.
const maxHostname = 40

// checkHostname accepts an empty name, or letters, digits, hyphens and dots
// that fit the vendor area.
func checkHostname(h string) error {
	if len(h) > maxHostname {
		return fmt.Errorf("%q is longer than %d bytes", h, maxHostname)
	}
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
		default:
			return fmt.Errorf("%q has %q, which a host name cannot", h, r)
		}
	}
	if strings.HasPrefix(h, "-") || strings.HasPrefix(h, ".") || strings.HasSuffix(h, ".") {
		return fmt.Errorf("%q starts or ends with a hyphen or dot", h)
	}
	return nil
}
