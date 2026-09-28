package main

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/tamago/pvectl/internal/config"
	"github.com/tamago/pvectl/internal/plan"
	"github.com/tamago/pvectl/internal/talos"
)

// Completion reads node names from the live config rather than a baked-in
// list, so a node added with `config discover` completes without a rebuild.
// It must never fail loudly: a missing or unreadable config just means no
// suggestions.

func configNodeNames() []string {
	cfg, err := config.Load(g.configPath)
	if err != nil {
		return nil
	}
	return cfg.NodeNames()
}

// completeNodeOp handles both argument shapes nodeOpCmd accepts:
// `reboot node <name>` and `plan reboot node <name>`.
func completeNodeOp(use string) cobra.CompletionFunc {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		switch {
		case use != "node" && len(args) == 0:
			return []string{"node"}, cobra.ShellCompDirectiveNoFileComp
		case use == "node" && len(args) == 0, use != "node" && len(args) == 1:
			return configNodeNames(), cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

func completeNodeOptional(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return configNodeNames(), cobra.ShellCompDirectiveNoFileComp
}

func completeCluster(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return []string{"cluster"}, cobra.ShellCompDirectiveNoFileComp
}

func noCompletion(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// completeSkipCheck completes the comma-separated --skip-check list one item
// at a time, leaving out the names already given.
func completeSkipCheck(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	done := map[string]bool{}
	prefix := ""
	if i := strings.LastIndexByte(toComplete, ','); i >= 0 {
		prefix = toComplete[:i+1]
		for _, s := range strings.Split(toComplete[:i], ",") {
			done[s] = true
		}
	}
	var out []string
	for _, n := range plan.CheckNames {
		if !done[n] {
			out = append(out, prefix+n)
		}
	}
	return out, cobra.ShellCompDirectiveNoFileComp | cobra.ShellCompDirectiveNoSpace
}

// installCompletions wires the dynamic completions onto the finished tree.
// Doing it in one walk means a new subcommand gets sensible behaviour (no file
// names offered for node arguments) without anyone remembering to add it here.
func installCompletions(root *cobra.Command) {
	_ = root.RegisterFlagCompletionFunc("skip-check", completeSkipCheck)
	_ = root.MarkPersistentFlagFilename("config", "yaml", "yml")
	_ = root.MarkPersistentFlagDirname("log-dir")
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.ValidArgsFunction == nil && c.Runnable() && c.Name() != "help" && c.Name() != "completion" &&
			(c.Parent() == nil || c.Parent().Name() != "completion") {
			c.ValidArgsFunction = noCompletion
		}
		if c.Flags().Lookup("talos-version") != nil {
			_ = c.RegisterFlagCompletionFunc("talos-version", cobra.FixedCompletions(
				[]string{talos.AliasLatest, talos.AliasLatestPatch}, cobra.ShellCompDirectiveNoFileComp))
			_ = c.RegisterFlagCompletionFunc("talos-image", noCompletion)
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}
