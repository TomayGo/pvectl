package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tamago/pvectl/internal/config"
	"github.com/tamago/pvectl/internal/plan"
)

func newConfigCmd() *cobra.Command {
	c := &cobra.Command{Use: "config", Short: "Generate, show and check the cluster definition"}
	c.AddCommand(configDiscoverCmd(), configShowCmd(), configValidateCmd())
	return c
}

func configShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the effective configuration after defaults",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(g.configPath)
			if err != nil {
				return err
			}
			b, err := cfg.Marshal()
			if err != nil {
				return err
			}
			fmt.Print(string(b))
			return nil
		},
	}
}

func configValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check the config against the live cluster",
		Long: strings.TrimSpace(`
Checks the file for internal consistency (literal IPs, two WoL MACs per node,
every node in the endpoint list) and then against the cluster: that each node
exists, that each configured guest is really on the node it is listed under,
and that the Talos VM ids match.`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(g.configPath)
			if err != nil {
				return err
			}
			problems := cfg.Validate()
			for _, e := range problems {
				fmt.Printf("  [FAIL] %v\n", e)
			}
			if len(problems) > 0 {
				return fmt.Errorf("%d problem(s) in %s", len(problems), g.configPath)
			}
			fmt.Printf("  [ok]   %s is internally consistent\n", g.configPath)

			e, lg, err := newEnv("")
			if err != nil {
				return err
			}
			defer lg.Close()
			live, err := crossCheck(cmd.Context(), e)
			for _, w := range live {
				fmt.Printf("  [warn] %s\n", w)
			}
			if err != nil {
				return err
			}
			if len(live) == 0 {
				fmt.Println("  [ok]   config matches the live cluster")
			}
			return nil
		},
	}
}

// crossCheck compares the config against what the cluster actually reports.
func crossCheck(ctx context.Context, e *plan.Env) ([]string, error) {
	var out []string
	nodes, err := e.PVE.Nodes(ctx)
	if err != nil {
		return out, err
	}
	known := map[string]bool{}
	for _, n := range nodes {
		known[n.Node] = true
	}
	for _, name := range e.Cfg.NodeNames() {
		if !known[name] {
			out = append(out, fmt.Sprintf("node %s is in the config but not in the cluster", name))
		}
	}
	for name := range known {
		if _, ok := e.Cfg.Nodes[name]; !ok {
			out = append(out, fmt.Sprintf("node %s is in the cluster but not in the config", name))
		}
	}
	res, err := e.PVE.Resources(ctx, "vm")
	if err != nil {
		return out, err
	}
	byID := map[int]string{}
	for _, r := range res {
		byID[r.VMID] = r.Node
	}
	for name, n := range e.Cfg.Nodes {
		if n.Talos.VMID != 0 && byID[n.Talos.VMID] != name {
			out = append(out, fmt.Sprintf("nodes.%s.talos.vmid %d is actually on %q", name, n.Talos.VMID, byID[n.Talos.VMID]))
		}
		for _, gst := range n.StopGuests {
			if byID[gst.ID] != name {
				out = append(out, fmt.Sprintf("nodes.%s.stop_guests %d is actually on %q", name, gst.ID, byID[gst.ID]))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func configDiscoverCmd() *cobra.Command {
	var write bool
	c := &cobra.Command{
		Use:   "discover",
		Short: "Generate a config skeleton from the live cluster",
		Long: strings.TrimSpace(`
Reads the cluster and emits a config: node names, LAN IPs and every running
guest that has a hostpci* or usb* entry, since those are exactly the guests that
cannot be migrated and must be shut down by hand.

What it cannot know is left for you: the WoL MACs, the warning text for each
guest, and which guests need starting again because they have no onboot flag.
Existing values for those are carried over from the current config.`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			e, lg, err := newEnv("")
			if err != nil {
				// Discovery must still work before a config exists.
				return err
			}
			defer lg.Close()
			out, err := discover(cmd.Context(), e)
			if err != nil {
				return err
			}
			b, err := out.Marshal()
			if err != nil {
				return err
			}
			if !write {
				fmt.Print(string(b))
				fmt.Fprintf(os.Stderr, "\n(not written; re-run with --write to replace %s)\n", g.configPath)
				return nil
			}
			// Every command loads this file and refuses to run if it is
			// invalid -- including this one. Writing an invalid config would
			// leave no way to regenerate it but a hand edit.
			if errs := out.Validate(); len(errs) > 0 {
				for _, e := range errs {
					fmt.Fprintf(os.Stderr, "  [FAIL] %v\n", e)
				}
				return fmt.Errorf("refusing to write %s: the generated config is invalid (%d problem(s)); fix the live cluster or edit the file by hand",
					g.configPath, len(errs))
			}
			if err := backupConfig(g.configPath); err != nil {
				return err
			}
			if err := os.WriteFile(g.configPath, b, 0o640); err != nil {
				return fmt.Errorf("write %s: %w", g.configPath, err)
			}
			fmt.Printf("wrote %s\n", g.configPath)
			return nil
		},
	}
	c.Flags().BoolVar(&write, "write", false, "overwrite the config file instead of printing it")
	return c
}

// backupConfig keeps the previous file next to the new one. The generator
// cannot reproduce hand-written comments or the fields it has no way to know,
// so the old copy is the only route back.
func backupConfig(path string) error {
	old, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s before overwriting: %w", path, err)
	}
	bak := path + ".bak"
	if err := os.WriteFile(bak, old, 0o640); err != nil {
		return fmt.Errorf("write backup %s: %w", bak, err)
	}
	fmt.Printf("previous config saved to %s\n", bak)
	return nil
}

func discover(ctx context.Context, e *plan.Env) (*config.Config, error) {
	out := &config.Config{Cluster: e.Cfg.Cluster, Nodes: map[string]config.Node{}}
	nodes, err := e.PVE.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	cs, err := e.PVE.ClusterStatus(ctx)
	if err != nil {
		return nil, err
	}
	res, err := e.PVE.Resources(ctx, "vm")
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		prev := e.Cfg.Nodes[n.Node]
		nc := config.Node{
			LANIP:  cs.Nodes[n.Node].IP,
			WOLMAC: prev.WOLMAC,
			Talos:  prev.Talos,
			// start_after_boot cannot be derived: onboot is a guest setting,
			// but whether *we* want it started is an operator decision.
			StartAfterBoot: prev.StartAfterBoot,
		}
		if nc.LANIP == "" {
			nc.LANIP = prev.LANIP
		}
		warnFor := map[int]string{}
		netFor := map[int]bool{}
		seen := map[int]bool{}
		for _, gst := range prev.StopGuests {
			warnFor[gst.ID] = gst.Warn
			netFor[gst.ID] = gst.ProvidesNetwork
		}
		// Discovery only sees running guests, but a stopped passthrough guest
		// is still one that cannot migrate. Keep what the config already knows
		// rather than silently dropping it (VM 105 is normally powered off).
		for _, gst := range prev.StopGuests {
			nc.StopGuests = append(nc.StopGuests, gst)
			seen[gst.ID] = true
		}
		for _, r := range res {
			if r.Node != n.Node || r.Status != "running" {
				continue
			}
			if r.VMID == prev.Talos.VMID {
				continue
			}
			cfg, err := e.PVE.GuestConfig(ctx, n.Node, r.Kind(), r.VMID)
			if err != nil {
				e.Log.Warnf("cannot read config of guest %d on %s: %v", r.VMID, n.Node, err)
				continue
			}
			if !hasPassthroughKeys(cfg) || seen[r.VMID] {
				continue
			}
			seen[r.VMID] = true
			nc.StopGuests = append(nc.StopGuests, config.Guest{
				ID:              r.VMID,
				Warn:            warnFor[r.VMID],
				Type:            r.Kind(),
				ProvidesNetwork: netFor[r.VMID],
			})
		}
		sort.Slice(nc.StopGuests, func(i, j int) bool { return nc.StopGuests[i].ID < nc.StopGuests[j].ID })
		out.Nodes[n.Node] = nc
	}
	return out, nil
}

func hasPassthroughKeys(cfg map[string]any) bool {
	for k := range cfg {
		if strings.HasPrefix(k, "hostpci") || strings.HasPrefix(k, "usb") {
			return true
		}
	}
	return false
}
