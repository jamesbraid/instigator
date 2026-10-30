package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/extract"
)

// extractCommand extracts install sets under the last argument. A lone
// source ending in .yaml or .yml is a config file, whose sets (all, or
// those named by sets) are extracted. Any other sources are disc images or
// directories, the layers of one set in the order given.
func extractCommand(w io.Writer, args, sets []string, dist, name string, opts extract.Options) error {
	if len(args) < 2 {
		return fmt.Errorf("extract: need a config file or at least one image or directory, then an output directory")
	}
	srcs, outDir := args[:len(args)-1], args[len(args)-1]
	if len(srcs) == 1 && isConfigArg(srcs[0]) {
		if dist != "" || name != "" {
			return fmt.Errorf("extract: --dist and --name apply only when extracting images or directories directly, not with a config file")
		}
		b, err := os.ReadFile(srcs[0])
		if err != nil {
			return err
		}
		cfg, err := config.ParseInstallSetsIn(b, filepath.Dir(srcs[0]))
		if err != nil {
			return err
		}
		return extract.Run(w, cfg, sets, outDir, opts)
	}
	if len(sets) > 0 {
		return fmt.Errorf("extract: --set needs a config file; with images or directories everything given is one install set (use --name to name it)")
	}
	set, err := directSet(srcs, dist, name)
	if err != nil {
		return err
	}
	return extract.Run(w, &config.Config{InstallSets: []config.InstallSet{set}}, nil, outDir, opts)
}

func isConfigArg(p string) bool {
	e := strings.ToLower(filepath.Ext(p))
	return e == ".yaml" || e == ".yml"
}

// directSet builds the one set direct-mode extraction runs. dist names the
// distribution directory in every source ("dist" when empty, "." for the
// source root itself). The set and each layer are named after their
// source's base name without its extension, the set after the first.
func directSet(sources []string, dist, name string) (config.InstallSet, error) {
	if name == "" {
		name = stripExt(sources[0])
	}
	if !config.IsDirName(name) {
		return config.InstallSet{}, fmt.Errorf("extract: %q is not usable as a set name; give a single directory name with --name", name)
	}
	if dist == "" {
		dist = "dist"
	}
	used := map[string]bool{}
	var layers []config.Layer
	for _, src := range sources {
		ln := stripExt(src)
		for base, n := ln, 2; used[ln]; n++ {
			ln = fmt.Sprintf("%s-%d", base, n)
		}
		used[ln] = true
		layers = append(layers, config.Layer{Name: ln, Source: src, Dist: dist})
	}
	return config.InstallSet{Name: name, Enabled: true, Layers: layers}, nil
}

func stripExt(src string) string {
	base := filepath.Base(filepath.Clean(src))
	if ext := filepath.Ext(base); ext != base {
		return strings.TrimSuffix(base, ext)
	}
	return base
}
