package configs

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/jamesbraid/instigator/internal/config"
)

// recipe is the part of Origami's IRIX release recipes that these configs
// restate: the snapshots in testdata are copies of its
// instigator-irix-<release>.toml files. Origami read them to generate an
// instigator configuration, so each shipped config must describe the same
// sets, layer URLs, install and keep lists, collisions and add-ons.
type recipe struct {
	BaseURL    string                       `toml:"base_url"`
	Selections map[string]selection         `toml:"package_selections"`
	Addons     map[string]recipeAddon       `toml:"addons"`
	Collisions map[string]map[string]string `toml:"collisions"`
	Layers     []recipeLayer                `toml:"layers"`
}

type selection struct {
	Sets    []string `toml:"sets"`
	Install []string `toml:"install"`
	Keep    []string `toml:"keep"`
}

type recipeAddon struct {
	Path    string   `toml:"path"`
	Base    string   `toml:"base"`
	Dist    string   `toml:"dist"`
	Install []string `toml:"install"`
}

type recipeLayer struct {
	Set        string `toml:"set"`
	Name       string `toml:"name"`
	RemotePath string `toml:"remote_path"`
	Boot       bool   `toml:"boot"`
	Base       string `toml:"base"`
	Dist       string `toml:"dist"`
}

func loadRecipe(t *testing.T, release string) recipe {
	t.Helper()
	var r recipe
	if _, err := toml.DecodeFile(filepath.Join("testdata", "instigator-irix-"+release+".toml"), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

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

func TestShippedConfigsMatchRecipes(t *testing.T) {
	for _, tc := range shipped {
		t.Run(tc.release+"-"+tc.profile, func(t *testing.T) {
			r := loadRecipe(t, tc.release)
			sel, ok := r.Selections[tc.profile]
			if !ok {
				t.Fatalf("recipe has no %s profile", tc.profile)
			}
			path := "irix-" + tc.release + "-" + tc.profile + ".yaml"
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := config.ParseIn(raw, ".")
			if err != nil {
				t.Fatal(err)
			}

			var gotSets []string
			for _, s := range cfg.InstallSets {
				gotSets = append(gotSets, s.Name)
				if !s.Enabled || s.Addon {
					t.Errorf("set %s: enabled=%v addon=%v", s.Name, s.Enabled, s.Addon)
				}
				var want []recipeLayer
				for _, l := range r.Layers {
					if l.Set == s.Name {
						want = append(want, l)
					}
				}
				if len(s.Layers) != len(want) {
					t.Errorf("set %s: %d layers, recipe has %d", s.Name, len(s.Layers), len(want))
					continue
				}
				for i, l := range s.Layers {
					w := want[i]
					wantDist := w.Dist
					if wantDist == "" {
						wantDist = "dist"
					}
					if l.Name != w.Name || l.Source != r.BaseURL+w.RemotePath || l.Boot != w.Boot || l.Base != w.Base || l.Dist != wantDist {
						t.Errorf("set %s layer %d = %+v, recipe %+v", s.Name, i, l, w)
					}
				}
				if got, want := s.Collisions, r.Collisions[s.Name]; len(got)+len(want) > 0 && !mapsEqual(got, want) {
					t.Errorf("set %s collisions = %v, recipe %v", s.Name, got, want)
				}
			}
			if !slices.Equal(gotSets, sel.Sets) {
				t.Errorf("sets = %v, recipe %v", gotSets, sel.Sets)
			}

			if len(cfg.InstallScripts) != 1 || cfg.InstallScripts[0].Name != "irix" {
				t.Fatalf("scripts = %+v, want the irix script", cfg.InstallScripts)
			}
			sc := cfg.InstallScripts[0]
			if !slices.Equal(sc.Install, sel.Install) || !slices.Equal(sc.Keep, sel.Keep) || sc.Then != nil {
				t.Errorf("irix script = %+v, recipe install %v keep %v", sc, sel.Install, sel.Keep)
			}
		})
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestShippedAddonsMatchRecipes(t *testing.T) {
	for _, tc := range shipped {
		t.Run(tc.release+"-"+tc.profile, func(t *testing.T) {
			r := loadRecipe(t, tc.release)
			names := []string{"mipspro", "rad4"}
			cfg, err := config.ParseIn(withAddons(t, "irix-"+tc.release+"-"+tc.profile+".yaml", names...), ".")
			if err != nil {
				t.Fatal(err)
			}
			var wantInstall []string
			var wantSets []string
			for _, name := range names {
				a := r.Addons[name]
				set := "addon-" + name
				wantSets = append(wantSets, set)
				wantInstall = append(wantInstall, a.Install...)
				i := slices.IndexFunc(cfg.InstallSets, func(s config.InstallSet) bool { return s.Name == set })
				if i < 0 {
					t.Fatalf("no set %s", set)
				}
				s := cfg.InstallSets[i]
				wantDist := a.Dist
				if len(s.Layers) != 1 || !s.Addon || s.Layers[0].Source != r.BaseURL+a.Path ||
					s.Layers[0].Base != a.Base || s.Layers[0].Dist != wantDist || s.Layers[0].Name != name {
					t.Errorf("%s = %+v, recipe %+v", set, s, a)
				}
			}
			then := cfg.InstallScripts[0].Then
			if then == nil || !slices.Equal(then.Sets, wantSets) || !slices.Equal(then.Install, wantInstall) {
				t.Errorf("then = %+v, want sets %v install %v", then, wantSets, wantInstall)
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
