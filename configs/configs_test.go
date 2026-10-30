package configs

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jamesbraid/instigator/internal/config"
)

// withAddons returns the config text with its commented addons list
// switched on for the named add-on files.
func withAddons(t *testing.T, path string, names ...string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	const off = "# addons:\n#   - addons/mipspro.yaml\n"
	if !strings.Contains(s, off) {
		t.Fatalf("%s: commented addons list not found", path)
	}
	on := "addons:\n"
	for _, n := range names {
		on += "  - addons/" + n + ".yaml\n"
	}
	return []byte(strings.Replace(s, off, on, 1))
}

var shipped = []struct{ release, profile string }{
	{"6.5.30", "standard"},
	{"6.5.30", "minimal"},
	{"6.5.22", "standard"},
}

// Each shipped config loads, boots from one layer and serves the irix script,
// alone and with every shipped add-on joining that script's second pass.
func TestShippedConfigsLoad(t *testing.T) {
	addons := []string{"mipspro", "rad4"}
	for _, tc := range shipped {
		t.Run(tc.release+"-"+tc.profile, func(t *testing.T) {
			path := "irix-" + tc.release + "-" + tc.profile + ".yaml"
			cfg, err := config.ParseIn(mustRead(t, path), ".")
			if err != nil {
				t.Fatal(err)
			}
			boot := 0
			for _, s := range cfg.InstallSets {
				for _, l := range s.Layers {
					if l.Boot {
						boot++
					}
				}
			}
			if boot != 1 {
				t.Errorf("%d boot layers, want 1", boot)
			}
			if len(cfg.InstallScripts) != 1 || cfg.InstallScripts[0].Name != "irix" || cfg.InstallScripts[0].Then != nil {
				t.Fatalf("scripts = %+v, want the irix script alone", cfg.InstallScripts)
			}

			cfg, err = config.ParseIn(withAddons(t, path, addons...), ".")
			if err != nil {
				t.Fatal(err)
			}
			then := cfg.InstallScripts[0].Then
			if then == nil || !slices.Equal(then.Sets, []string{"addon-mipspro", "addon-rad4"}) || len(then.Install) == 0 {
				t.Errorf("then = %+v, want both add-ons", then)
			}
		})
	}
}

// Each config ships with placeholder addresses a user must replace.
func TestShippedConfigsUsePlaceholderClient(t *testing.T) {
	for _, tc := range shipped {
		cfg, err := config.ParseIn(mustRead(t, "irix-"+tc.release+"-"+tc.profile+".yaml"), ".")
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Clients) != 1 || cfg.Clients[0].IP.String() != "192.0.2.30" || cfg.ServerIP.String() != "192.0.2.10" {
			t.Errorf("%s-%s: client %+v server %s", tc.release, tc.profile, cfg.Clients, cfg.ServerIP)
		}
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
