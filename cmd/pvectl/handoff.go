package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/TomayGo/pvectl/internal/config"
	plog "github.com/TomayGo/pvectl/internal/log"
	"github.com/TomayGo/pvectl/internal/plan"
	"github.com/TomayGo/pvectl/internal/remote"
	"github.com/TomayGo/pvectl/internal/state"
)

// A rolling run cannot reboot the node it runs on: the process would die
// before the restore steps. So when it gets there it continues the run on a
// peer instead -- the same command, started there under systemd-run so it is
// owned by the peer's systemd rather than by this node's ssh session, and
// picking up from rolling.json, which pmxcfs has already replicated. The
// lock is not held between nodes, so there is nothing to pass across.
//
// Each hand-off is followed by at least one node completing (the peer's first
// node is the one that handed off), so a chain of them always terminates.

// handoffSkip are the flags not forwarded to the peer: the hand-off adds its
// own --yes and --handoff-from, the Talos target is recorded in rolling.json
// and inherited from there, and --restart would throw away the very progress
// the peer is meant to continue.
var handoffSkip = map[string]bool{
	"restart": true, "yes": true, "dry-run": true, "no-handoff": true, "handoff-from": true,
	"talos-version": true, "talos-image": true,
}

// forwardedFlags re-renders every flag the operator set (global ones included:
// cobra merges them into the subcommand's set), so the peer runs with the
// same timeouts, skipped checks and verbosity.
func forwardedFlags(fs *pflag.FlagSet) []string {
	var out []string
	fs.Visit(func(f *pflag.Flag) {
		if handoffSkip[f.Name] {
			return
		}
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			for _, v := range sv.GetSlice() {
				out = append(out, "--"+f.Name+"="+v)
			}
			return
		}
		out = append(out, "--"+f.Name+"="+f.Value.String())
	})
	return out
}

// handoffPeers lists who can take over, most recently finished node first:
// it has just been through the full plan and passed verify. Nodes not yet
// done come after, in rolling order; the run hands off again when it reaches
// them.
func handoffPeers(prog *state.Rolling, self string) []string {
	var out []string
	seen := map[string]bool{self: true}
	for i := len(prog.Done) - 1; i >= 0; i-- {
		if n := prog.Done[i]; !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, n := range prog.Order {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// handoffArgv is the command line the peer runs.
func handoffArgv(exe, command, self string, fwd []string) []string {
	argv := []string{exe, "rolling", command, "cluster", "--yes", "--handoff-from=" + self}
	return append(argv, fwd...)
}

func handoffUnit(kind string, now time.Time) string {
	return "pvectl-" + kind + "-" + now.Format("20060102-150405")
}

type handoffCtx struct {
	cfg     *config.Config
	lg      *plog.Logger
	fs      *pflag.FlagSet
	command string // plan.CmdReboot or plan.CmdUpgrade
	prog    *state.Rolling
	self    string
}

func (h *handoffCtx) host(node string) remote.Host {
	return remote.Host{Name: node, IP: h.cfg.Nodes[node].LANIP}
}

// probe checks that the peer answers over ssh, runs the same pvectl build,
// has systemd-run, and (on a real run) already sees the recorded progress --
// without it the peer would start the whole rolling run over.
func (h *handoffCtx) probe(ctx context.Context, peer, exe string) error {
	script := remote.Quote(exe, "--version") + "; command -v systemd-run >/dev/null || echo no-systemd-run; " +
		"test -s " + remote.Quote(filepath.Join(state.Dir, "rolling.json")) + " && echo progress-ok; true"
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", append(remote.Args(h.host(peer)), script)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ssh: %v: %s", err, strings.TrimSpace(string(out)))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	want := "pvectl version " + version
	if got := strings.TrimSpace(lines[0]); got != want {
		return fmt.Errorf("runs %q, this node runs %q (deploy the same build everywhere first)", got, want)
	}
	rest := strings.Join(lines[1:], "\n")
	if strings.Contains(rest, "no-systemd-run") {
		return errors.New("systemd-run not found")
	}
	if !g.opts.DryRun && !strings.Contains(rest, "progress-ok") {
		return errors.New("does not see the recorded progress in " + state.Dir)
	}
	return nil
}

// handOff continues the rolling run on a peer. It returns nil once the peer
// has the run, whether or not the operator stays to follow its output.
func (h *handoffCtx) handOff(ctx context.Context) error {
	kind := h.prog.Command
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return fmt.Errorf("locate own binary: %w", err)
	}
	if !g.opts.DryRun {
		// Warn-only elsewhere; here the peer depends on it.
		if err := h.prog.Save(state.Dir); err != nil {
			return fmt.Errorf("record progress before hand-off: %w", err)
		}
	}

	h.lg.Plain("")
	h.lg.Plain("================ %s ================", h.self)
	h.lg.Plain("%s is the node running pvectl; handing the run over to a peer.", h.self)
	var peer string
	for _, p := range handoffPeers(h.prog, h.self) {
		if err := h.probe(ctx, p, exe); err != nil {
			h.lg.Warnf("cannot hand off to %s: %v", p, err)
			continue
		}
		peer = p
		break
	}
	if peer == "" {
		h.printManual()
		return fmt.Errorf("%s stopped at %s: no peer could take over", rollingWhat(kind), h.self)
	}

	unit := handoffUnit(kind, time.Now())
	argv := handoffArgv(exe, h.command, h.self, forwardedFlags(h.fs))
	start := remote.Quote(append([]string{"systemd-run", "--quiet", "--collect", "--uid=root", "--unit=" + unit,
		fmt.Sprintf("--description=pvectl %s (handed off from %s)", rollingWhat(kind), h.self), "--"}, argv...)...)
	ip := h.cfg.Nodes[peer].LANIP
	if g.opts.DryRun {
		h.lg.Plain("(dry-run) would continue on %s:", peer)
		h.lg.Plain("    ssh %s %s", ip, start)
		return nil
	}

	env := &plan.Env{Opts: g.opts}
	if err := env.Confirm(ctx, fmt.Sprintf("About to hand the %s over to %s, which will %s %s (this node) unattended.",
		rollingWhat(kind), peer, h.command, h.self)); err != nil {
		h.printManual()
		return err
	}
	if out, err := exec.CommandContext(ctx, "ssh", append(remote.Args(h.host(peer)), start)...).CombinedOutput(); err != nil {
		// Not retried on another peer: if ssh broke after systemd-run, the run
		// may well be going, and two of them must not be.
		h.lg.Warnf("starting the run on %s failed: %v: %s", peer, err, strings.TrimSpace(string(out)))
		h.lg.Plain("Check `ssh %s systemctl status %s` before finishing by hand.", ip, unit)
		h.printManual()
		return fmt.Errorf("%s stopped at %s: hand-off to %s failed", rollingWhat(kind), h.self, peer)
	}
	h.lg.Infof("%s continues on %s as systemd unit %s", rollingWhat(kind), peer, unit)
	h.lg.Plain("Its output follows; Ctrl-C (or this node going down) only stops following.")
	h.lg.Plain("Follow it again with:  ssh %s journalctl -o cat -fu %s", ip, unit)
	h.lg.Plain("")
	h.follow(ctx, peer, unit)
	return nil
}

// follow streams the peer's journal for the unit until it ends or this node
// goes down -- the latter being the expected way out, since the peer is about
// to reboot it. Not part of the transcript: the peer keeps its own.
func (h *handoffCtx) follow(ctx context.Context, peer, unit string) {
	u := remote.Quote(unit)
	script := "journalctl -o cat -n all -fu " + u + " & j=$!; sleep 2; " +
		"while systemctl -q is-active " + u + "; do sleep 2; done; sleep 1; kill $j"
	cmd := exec.CommandContext(ctx, "ssh", append(remote.Args(h.host(peer)), script)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	_ = cmd.Run()
	if ctx.Err() == nil {
		fmt.Printf("\n(the run on %s has ended; its result is above)\n", peer)
	}
}

// printManual is the pre-hand-off behaviour, kept for --no-handoff and as the
// fallback when no peer can take over.
func (h *handoffCtx) printManual() {
	h.lg.Plain("")
	h.lg.Plain("Every node before %s is done. Finish from a peer:", h.self)
	for _, other := range otherThan(h.prog.Order, h.self) {
		h.lg.Plain("    ssh %s 'pvectl rolling %s cluster --yes'", h.cfg.Nodes[other].LANIP, h.command)
	}
	h.lg.Plain("")
	h.lg.Plain("(ssh by IP, not by hostname: the tailnet name is hijacked by Tailscale SSH)")
}

func addHandoffFlags(c *cobra.Command) {
	c.Flags().Bool("no-handoff", false, "stop at the node running pvectl instead of continuing on a peer")
	c.Flags().String("handoff-from", "", "set by a hand-off: the node that passed this run on")
	_ = c.Flags().MarkHidden("handoff-from")
}
