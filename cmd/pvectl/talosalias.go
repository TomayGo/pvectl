package main

import (
	"context"

	"github.com/tamago/pvectl/internal/config"
	plog "github.com/tamago/pvectl/internal/log"
	"github.com/tamago/pvectl/internal/talos"
)

// resolveTalosAlias turns --talos-version latest / latest-patch into a release
// tag, in place in the global options, before anything records it. From then
// on only the tag exists: state.json and rolling.json never see the alias, so a
// resume or a continued rolling upgrade installs the release the run started
// with even if a newer one is published in between.
//
// latest-patch is relative to the oldest release running in the cluster, so a
// half-finished earlier upgrade does not drag the target a minor ahead of the
// nodes it has not reached.
func resolveTalosAlias(ctx context.Context, cfg *config.Config, lg *plog.Logger) error {
	alias := g.opts.TalosVersion
	if !talos.IsAlias(alias) {
		return nil
	}
	tc := talos.New(cfg.SecretPath("talosconfig"), lg, false)
	running := ""
	for _, n := range cfg.NodeNames() {
		v, err := tc.Version(ctx, cfg.Nodes[n].Talos.IP)
		if err != nil {
			lg.Warnf("cannot read %s's Talos release: %v", cfg.Nodes[n].Talos.Name, err)
			continue
		}
		if pv := talos.ParseVersion(v); pv != nil && (running == "" || talos.CompareVersions(pv, talos.ParseVersion(running)) < 0) {
			running = v
		}
	}
	tags, err := talos.StableReleases(ctx)
	if err != nil {
		return err
	}
	tag, err := talos.PickRelease(tags, alias, running)
	if err != nil {
		return err
	}
	lg.Plain("--talos-version %s -> %s (cluster runs %s)", alias, tag, orNone(running))
	g.opts.TalosVersion = tag
	return nil
}
