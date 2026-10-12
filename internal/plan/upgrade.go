package plan

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/TomayGo/pvectl/internal/apt"
	"github.com/TomayGo/pvectl/internal/talos"
)

// An upgrade is a reboot with two installs slotted in after the drain:
//
//	talos-upgrade  write the new Talos release to the VM's inactive boot slot
//	host-upgrade   apt-get update && full-upgrade on the PVE host
//
// Neither reboots anything by itself. The host reboot that follows boots the
// new kernel, and the Talos VM -- stopped and started with it -- boots the new
// release, so each node goes down exactly once for both.
//
// Both installs come before talos-stop on purpose: if either fails, the node
// is drained but nothing is down yet, and `abort` puts it straight back.

// talosVersionRE is what --talos-version accepts: a release tag.
var talosVersionRE = regexp.MustCompile(`^v\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`)

// ValidateUpgradeOptions rejects a malformed target before anything runs.
func ValidateUpgradeOptions(o Options) error {
	if o.TalosVersion != "" && o.TalosImage != "" {
		return fmt.Errorf("--talos-version and --talos-image are mutually exclusive")
	}
	if o.TalosVersion != "" && !talosVersionRE.MatchString(o.TalosVersion) && !talos.IsAlias(o.TalosVersion) {
		return fmt.Errorf("--talos-version %q is not a release tag like v1.13.7, %q or %q",
			o.TalosVersion, talos.AliasLatest, talos.AliasLatestPatch)
	}
	return nil
}

// recordUpgradeRequest stores what the operator asked for in state.json, so
// `resume` and `abort` -- which never see the original flags -- upgrade to the
// same release.
func recordUpgradeRequest(e *Env) {
	e.State.SetData("talos-upgrade", "requested_version", e.Opts.TalosVersion)
	e.State.SetData("talos-upgrade", "requested_image", e.Opts.TalosImage)
	e.State.SetData("talos-upgrade", "image", "")
	e.State.SetData("talos-upgrade", "version", "")
}

func dataString(e *Env, step, key string) string {
	s, _ := dataOf(e, step, key).(string)
	return s
}

// talosTarget resolves the installer image for this node, once, and records it.
//
// With --talos-version the image is the node's own machine.install.image with
// the tag replaced. That keeps the Image Factory schematic -- and with it the
// qemu-guest-agent extension -- which a plain ghcr.io/siderolabs/installer
// image would silently drop.
//
// Returns "" when no Talos upgrade was asked for.
func talosTarget(ctx context.Context, e *Env) (image, version string, err error) {
	if img := dataString(e, "talos-upgrade", "image"); img != "" {
		return img, dataString(e, "talos-upgrade", "version"), nil
	}
	reqVer := dataString(e, "talos-upgrade", "requested_version")
	reqImg := dataString(e, "talos-upgrade", "requested_image")
	switch {
	case reqImg != "":
		image = reqImg
		version = imageTag(reqImg)
	case reqVer != "":
		base, err := e.Talos.InstallImage(ctx, e.Node.Talos.IP)
		if err != nil {
			return "", "", fmt.Errorf("%w (pass --talos-image to name the installer explicitly)", err)
		}
		image = withTag(base, reqVer)
		version = reqVer
	default:
		return "", "", nil
	}
	e.State.SetData("talos-upgrade", "image", image)
	e.State.SetData("talos-upgrade", "version", version)
	return image, version, nil
}

// withTag replaces an image reference's tag (or digest).
func withTag(image, tag string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		image = image[:i]
	}
	return image + ":" + tag
}

// imageTag returns the tag when it is a release tag, "" otherwise (a digest,
// or something custom that cannot be compared with what the node reports).
func imageTag(image string) string {
	if strings.Contains(image, "@") {
		return ""
	}
	i := strings.LastIndex(image, ":")
	if i <= strings.LastIndex(image, "/") {
		return ""
	}
	if t := image[i+1:]; talosVersionRE.MatchString(t) {
		return t
	}
	return ""
}

// olderThan compares two release tags numerically, ignoring pre-release
// suffixes. Unparseable input compares as not older.
func olderThan(a, b string) bool {
	pa, pb := talos.ParseVersion(a), talos.ParseVersion(b)
	if pa == nil || pb == nil {
		return false
	}
	return talos.CompareVersions(pa, pb) < 0
}

// skipsMinor reports an upgrade that crosses more than one minor release (or a
// major). Talos supports upgrading one minor at a time; jumping further is
// untested upstream and can skip a config or etcd migration that the
// intermediate release performs.
func skipsMinor(from, to string) bool {
	f, t := talos.ParseVersion(from), talos.ParseVersion(to)
	if f == nil || t == nil {
		return false
	}
	return t[0] != f[0] || t[1]-f[1] > 1
}

// ---------- preflight additions ----------

// upgradeChecks are the parts of an upgrade that can be refused before the
// node is touched. Both are re-checked for real inside the steps; running them
// here moves the refusal ahead of the silence, the ceph flags and the drain.
func upgradeChecks(ctx context.Context, e *Env, r *Report) {
	run := func(name string, fn func()) {
		if e.skipped(name) {
			r.add(name, Skipped, "skipped by --skip-check")
			return
		}
		fn()
	}
	run("apt-plan", func() { checkAptPlan(ctx, e, r) })
	run("talos-target", func() { checkTalosTarget(ctx, e, r) })
}

// checkAptPlan simulates the upgrade from the host's current package index.
// Preflight is read-only, so it does not refresh the index; host-upgrade
// simulates again after `apt-get update` and applies the same rules.
func checkAptPlan(ctx context.Context, e *Env, r *Report) {
	p, err := e.Apt.Simulate(ctx, e.aptHost())
	if err != nil {
		r.add("apt-plan", Fail, "cannot simulate the upgrade on %s: %v", e.Target, err)
		return
	}
	if reason := refuseAptPlan(p.Remove, p.MajorBumps()); reason != "" {
		r.add("apt-plan", Fail, "%s", reason)
		return
	}
	r.add("apt-plan", Pass, "%s (from the current index; refreshed before installing)", p.Summary())
}

func checkTalosTarget(ctx context.Context, e *Env, r *Report) {
	image, version, err := talosTarget(ctx, e)
	if err != nil {
		r.add("talos-target", Fail, "%v", err)
		return
	}
	if image == "" {
		r.add("talos-target", Pass, "no Talos upgrade requested (--talos-version); %s keeps its release", e.Node.Talos.Name)
		return
	}
	cur, err := e.Talos.Version(ctx, e.Node.Talos.IP)
	if err != nil {
		r.add("talos-target", Fail, "cannot read %s's running release: %v", e.Node.Talos.Name, err)
		return
	}
	switch {
	case version != "" && olderThan(version, cur):
		r.add("talos-target", Fail, "%s runs %s; %s would be a downgrade", e.Node.Talos.Name, cur, version)
		return
	case version == cur:
		r.add("talos-target", Pass, "%s already runs %s; nothing to install", e.Node.Talos.Name, cur)
		return
	case version != "" && skipsMinor(cur, version):
		r.add("talos-target", Fail, "%s runs %s; %s skips a minor release -- Talos upgrades one minor at a time, "+
			"go through the intermediate release first", e.Node.Talos.Name, cur, version)
		return
	}
	exists, known, err := talos.ImageExists(ctx, image)
	switch {
	case known && !exists:
		r.add("talos-target", Fail, "%s does not exist in the registry (typo in --talos-version?)", image)
	case !known:
		r.add("talos-target", Warn, "%s %s -> %s; could not confirm the image exists (%v), the pull will tell",
			e.Node.Talos.Name, cur, orUnknown(version), err)
	default:
		r.add("talos-target", Pass, "%s %s -> %s (%s)", e.Node.Talos.Name, cur, orUnknown(version), image)
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "(untagged image)"
	}
	return s
}

// refuseAptPlan is the rule both the check and the step apply.
//
// A removal in a full-upgrade is the classic way a PVE host loses proxmox-ve
// (a repository mix-up makes apt "resolve" a conflict by dropping it), and a
// major bump of Ceph or pve-manager needs a procedure a node-by-node reboot
// does not follow. Both need a human, so both stop the run.
func refuseAptPlan(remove []apt.Package, bumps []string) string {
	if len(remove) > 0 {
		var names []string
		for _, p := range remove {
			names = append(names, p.Name)
		}
		return fmt.Sprintf("full-upgrade would REMOVE %s; resolve by hand before upgrading", strings.Join(names, ", "))
	}
	if len(bumps) > 0 {
		return fmt.Sprintf("major version change needs its own upgrade procedure: %s", strings.Join(bumps, "; "))
	}
	return ""
}

// ---------- talos-upgrade ----------

func stepTalosUpgrade() Step {
	return Step{
		Name: "talos-upgrade",
		Desc: "install the new Talos release into the VM's inactive boot slot (no reboot)",
		Skip: func(ctx context.Context, e *Env) (bool, string, error) {
			image, version, err := talosTarget(ctx, e)
			if err != nil {
				return false, "", err
			}
			if image == "" {
				return true, "no --talos-version given; Talos keeps its release", nil
			}
			if version == "" {
				return false, "", nil
			}
			cur, err := e.Talos.Version(ctx, e.Node.Talos.IP)
			if err != nil {
				return false, "", fmt.Errorf("read %s's release: %w", e.Node.Talos.Name, err)
			}
			if cur == version {
				return true, fmt.Sprintf("%s already runs %s", e.Node.Talos.Name, cur), nil
			}
			e.State.SetData("talos-upgrade", "from", cur)
			return false, "", nil
		},
		Do: func(ctx context.Context, e *Env) error {
			image, _, err := talosTarget(ctx, e)
			if err != nil {
				return err
			}
			if err := e.Talos.Upgrade(ctx, e.Node.Talos.IP, image); err != nil {
				return err
			}
			if !e.Opts.DryRun {
				e.Log.Infof("%s will boot %s the next time it starts", e.Node.Talos.Name, image)
			}
			return nil
		},
		// The install is reversible until the node boots it, and even after:
		// Talos keeps the previous release in the other slot. Rolling back
		// reboots the VM, which is harmless here -- the node is drained.
		Undo: func(ctx context.Context, e *Env) error {
			ip := e.Node.Talos.IP
			if !e.Opts.DryRun {
				if err := e.Talos.WaitUp(ctx, ip, e.Opts.TimeoutUp); err != nil {
					return fmt.Errorf("%s must be running to roll it back: %w", e.Node.Talos.Name, err)
				}
			}
			e.Log.Warnf("rolling %s back to %s; the VM reboots", e.Node.Talos.Name,
				orUnknown(dataString(e, "talos-upgrade", "from")))
			return e.Talos.Rollback(ctx, ip)
		},
	}
}

// ---------- host-upgrade ----------

func stepHostUpgrade() Step {
	return Step{
		Name: "host-upgrade",
		Desc: "apt-get update && full-upgrade on the host",
		// Packages cannot be un-installed in any meaningful sense. The node
		// keeps running what it ran until the reboot, so an abort before
		// host-reboot still leaves it working -- just with new packages on disk.
		Irreversible: true,
		Do: func(ctx context.Context, e *Env) error {
			ip := e.aptHost()
			if err := e.Apt.Update(ctx, ip); err != nil {
				return fmt.Errorf("apt-get update on %s: %w", e.Target, err)
			}
			p, err := e.Apt.Simulate(ctx, ip)
			if err != nil {
				return fmt.Errorf("simulate upgrade on %s: %w", e.Target, err)
			}
			if reason := refuseAptPlan(p.Remove, p.MajorBumps()); reason != "" {
				return fmt.Errorf("%s: %s", e.Target, reason)
			}
			e.Log.Infof("%s: %s", e.Target, p.Summary())
			for _, name := range notablePackages {
				if pkg := p.Find(name); pkg != nil {
					e.Log.Infof("  %s %s -> %s", pkg.Name, orNew(pkg.Old), pkg.New)
				}
			}
			for _, pkg := range p.Install {
				if strings.HasPrefix(pkg.Name, "proxmox-kernel-") {
					e.Log.Infof("  new kernel %s", pkg.Name)
				}
			}
			e.State.SetData("host-upgrade", "summary", p.Summary())
			if p.Empty() {
				e.Log.Infof("nothing to install on %s", e.Target)
				return nil
			}
			if err := e.Apt.FullUpgrade(ctx, ip); err != nil {
				return fmt.Errorf("full-upgrade on %s: %w (the transcript has apt's output)", e.Target, err)
			}
			if !e.Opts.DryRun {
				e.Log.Infof("packages installed on %s", e.Target)
			}
			return nil
		},
	}
}

func (e *Env) aptHost() apt.Host { return apt.Host{Name: e.Target, IP: e.Node.LANIP} }

// notablePackages are the ones worth a line of their own in the transcript.
var notablePackages = []string{"pve-manager", "proxmox-ve", "qemu-server", "pve-qemu-kvm", "ceph-osd", "corosync"}

func orNew(v string) string {
	if v == "" {
		return "(new)"
	}
	return v
}

// ---------- upgrade-check ----------

// stepUpgradeCheck runs last, after verify has put the node back in service.
// Placed any earlier, a Talos release that failed to boot would stop the run
// with the node still cordoned and noout still set -- worse than the problem it
// reports, since Talos has already fallen back to a working release.
func stepUpgradeCheck() Step {
	return Step{
		Name: "upgrade-check",
		Desc: "confirm the node booted the new kernel and Talos release",
		Do: func(ctx context.Context, e *Env) error {
			if e.Opts.DryRun {
				e.Log.Infof("(dry-run) would compare running versions with what was installed")
				return nil
			}
			if k, err := e.Apt.Kernel(ctx, e.aptHost()); err != nil {
				e.Log.Warnf("cannot read %s's running kernel: %v", e.Target, err)
			} else {
				e.Log.Infof("%s runs kernel %s", e.Target, k)
			}
			image, version, err := talosTarget(ctx, e)
			if err != nil || image == "" || version == "" {
				return err
			}
			cur, err := e.Talos.Version(ctx, e.Node.Talos.IP)
			if err != nil {
				return fmt.Errorf("read %s's release: %w", e.Node.Talos.Name, err)
			}
			if cur != version {
				return fmt.Errorf("%s is running %s, not %s: the new release did not boot and Talos fell back to the old one "+
					"(the node is otherwise back in service; check `talosctl -n %s dmesg`)",
					e.Node.Talos.Name, cur, version, e.Node.Talos.IP)
			}
			e.Log.Infof("%s runs %s", e.Node.Talos.Name, cur)
			return nil
		},
	}
}
