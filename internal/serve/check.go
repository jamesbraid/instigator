package serve

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/jamesbraid/instigator/internal/config"
	"github.com/jamesbraid/instigator/internal/mediatree"
	"github.com/jamesbraid/instigator/internal/source"
	"github.com/jamesbraid/instigator/internal/vfs"
)

const (
	// probeParallel caps concurrent remote probes. A recipe names a few
	// dozen discs, usually on one host.
	probeParallel = 8
	// probeTimeout bounds one probe, connection included.
	probeTimeout = 30 * time.Second
)

// CheckShallow checks a configuration without reading remote media. Sets
// with only local sources are assembled as Check assembles them. A set with
// any remote layer has its layout validated, its local sources confirmed to
// exist, and each remote source probed for reachability without reading its
// body; its contents, collisions and product selections are left to Check
// or to serve. Every unreachable source is reported, not only the first.
func CheckShallow(cfg *config.Config) error {
	var local []config.InstallSet
	var remote []string
	unopened := map[string]bool{}
	seen := map[string]bool{}
	for _, set := range cfg.InstallSets {
		if err := vfs.Validate(mediatree.SetSpec(set)); err != nil {
			return err
		}
		hasRemote := false
		for _, l := range set.Layers {
			if source.IsRemote(l.Source) {
				hasRemote = true
				if !seen[l.Source] {
					seen[l.Source] = true
					remote = append(remote, l.Source)
				}
			}
		}
		if !hasRemote {
			local = append(local, set)
			continue
		}
		unopened[set.Name] = true
		for _, l := range set.Layers {
			if source.IsRemote(l.Source) {
				continue
			}
			if _, err := os.Stat(l.Source); err != nil {
				return fmt.Errorf("install set %q: layer %q: source: %w", set.Name, l.Name, err)
			}
		}
	}

	if err := probeSources(mediatree.NewResolver(cfg.CacheDir, cfg.Credentials, nil), remote); err != nil {
		return err
	}

	tree, err := mediatree.Build(local, cfg.CacheDir, cfg.Credentials, nil)
	if err != nil {
		return err
	}
	if _, err := generate(cfg, tree); err != nil {
		tree.Close()
		return err
	}
	if err := checkInstallSelections(cfg, tree, unopened); err != nil {
		tree.Close()
		return err
	}
	return tree.Close()
}

// probeSources probes every reference concurrently and joins the failures
// in configuration order.
func probeSources(r *source.Resolver, refs []string) error {
	errs := make([]error, len(refs))
	sem := make(chan struct{}, probeParallel)
	var wg sync.WaitGroup
	for i, ref := range refs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
			defer cancel()
			_, errs[i] = r.Probe(ctx, ref)
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
