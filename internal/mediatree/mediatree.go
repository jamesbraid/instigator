// Package mediatree builds the media tree for configured install sets. It
// is the one place that maps config types onto the source and vfs packages,
// so serve and the extract command build trees the same way and the lower
// packages stay free of config.
package mediatree

import (
	"os"
	"path/filepath"

	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/logging"
	"github.com/jamesbraid/instigator/internal/source"
	"github.com/jamesbraid/instigator/internal/vfs"
)

// Build opens every set in sets, fetching remote layers through cacheDir
// (empty for the default) with the given host credentials.
func Build(sets []config.InstallSet, cacheDir string, creds []config.Credential, logger *logging.Logger) (*vfs.Tree, error) {
	return vfs.Build(setSpecs(sets), NewResolver(cacheDir, creds, logger))
}

// NewResolver returns the resolver Build opens sources with, caching under
// cacheDir (empty for the default) and offering the given host credentials.
// Retried reads are logged to logger, which may be nil.
func NewResolver(cacheDir string, creds []config.Credential, logger *logging.Logger) *source.Resolver {
	return source.New(source.Options{
		CacheDir:    defaultCacheDir(cacheDir),
		Credentials: credentials(creds),
		Logger:      logger,
	})
}

// defaultCacheDir returns where fetched and extracted remote media are
// cached: the configured directory, else the user's cache directory
// (~/.cache/instigator on Linux), else - in a scratch container with no
// HOME or XDG_CACHE_HOME - a fixed fallback.
func defaultCacheDir(configured string) string {
	if configured != "" {
		return configured
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "/var/cache/instigator"
	}
	return filepath.Join(d, "instigator")
}

// credentials maps configured host credentials onto source's. The two
// types are shaped identically; the copy lets config and source diverge
// without sharing a struct.
func credentials(creds []config.Credential) source.Credentials {
	out := make(source.Credentials, 0, len(creds))
	for _, c := range creds {
		out = append(out, source.Credential{Host: c.Host, Username: c.Username, Password: c.Password})
	}
	return out
}

// setSpecs maps configured install sets onto the tree builder's own shape.
// Every set is built, enabled or not: a set left out of an install stays
// browsable, and Enabled decides only what a server generates and offers
// around the tree.
func setSpecs(sets []config.InstallSet) []vfs.SetSpec {
	specs := make([]vfs.SetSpec, 0, len(sets))
	for _, set := range sets {
		specs = append(specs, SetSpec(set))
	}
	return specs
}

// SetSpec maps one configured install set onto the tree builder's shape.
func SetSpec(set config.InstallSet) vfs.SetSpec {
	layers := make([]vfs.LayerSpec, 0, len(set.Layers))
	for _, l := range set.Layers {
		layers = append(layers, vfs.LayerSpec{
			Name:   l.Name,
			Source: l.Source,
			Base:   l.Base,
			Dist:   l.Dist,
			Stand:  l.Stand,
			Boot:   l.Boot,
		})
	}
	return vfs.SetSpec{Name: set.Name, Layers: layers, Collisions: set.Collisions, Replacements: set.Replacements}
}
