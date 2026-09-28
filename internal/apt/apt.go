// Package apt upgrades a PVE host's packages over ssh.
//
// This is the one place pvectl leaves the PVE API: the API can refresh the
// package index and list what is upgradable, but the only way it offers to
// install anything is the web UI's interactive "Upgrade" terminal. ssh between
// cluster nodes is set up by PVE itself (it is what migration uses), so this
// adds no new trust -- only a new transport.
//
// Hosts are always addressed by LAN IP: the hostname resolves to the tailnet,
// where Tailscale SSH intercepts the connection and waits for a browser login.
package apt

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	plog "github.com/tamago/pvectl/internal/log"
)

type Client struct {
	log    *plog.Logger
	dryRun bool
}

func New(lg *plog.Logger, dryRun bool) *Client {
	return &Client{log: lg, dryRun: dryRun}
}

// noninteractive keeps dpkg from ever stopping to ask. confold keeps the local
// version of any config file the operator changed; confdef takes the package
// maintainer's default for everything else. A prompt nobody can see would hang
// the run with the node already drained.
const noninteractive = "export DEBIAN_FRONTEND=noninteractive APT_LISTCHANGES_FRONTEND=none; "

// Generous bounds: a full-upgrade with a new kernel builds initramfs images
// and takes a few minutes. They exist so a hung dpkg fails the step instead of
// holding the node drained indefinitely.
const (
	updateTimeout  = 5 * time.Minute
	upgradeTimeout = 45 * time.Minute
)

const dpkgOpts = "-o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold"

// run executes a shell snippet on ip as root. Every output line goes to the
// transcript at debug level; stderr is also kept for the error message.
func (c *Client) run(ctx context.Context, ip, script string) (stdout string, err error) {
	c.log.Debugf("ssh root@%s: %s", ip, script)
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"root@"+ip, script)
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("ssh %s: %w", ip, err)
	}
	var (
		wg        sync.WaitGroup
		out, serr strings.Builder
	)
	collect := func(r io.Reader, into *strings.Builder) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			into.WriteString(line)
			into.WriteByte('\n')
			c.log.Debugf("  %s", line)
		}
	}
	wg.Add(2)
	go collect(outPipe, &out)
	go collect(errPipe, &serr)
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		return out.String(), fmt.Errorf("ssh root@%s %q: %w: %s", ip, firstWord(script), err, lastLines(serr.String(), 5))
	}
	return out.String(), nil
}

// Update refreshes the package index. --error-on=any makes a repository that
// failed to fetch an error rather than a warning; upgrading from a half-stale
// index is how a node ends up with a different package set from its peers.
func (c *Client) Update(ctx context.Context, ip string) error {
	if c.dryRun {
		c.log.Infof("(dry-run) would run apt-get update on %s", ip)
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	_, err := c.run(ctx, ip, noninteractive+"apt-get -q --error-on=any update")
	return err
}

// Simulate reports what a full-upgrade would do, from the current index. It
// changes nothing, so it runs in --dry-run as well.
func (c *Client) Simulate(ctx context.Context, ip string) (*Plan, error) {
	out, err := c.run(ctx, ip, noninteractive+"apt-get -s -q full-upgrade")
	if err != nil {
		return nil, err
	}
	return ParsePlan(out), nil
}

// FullUpgrade installs the upgrade. It is full-upgrade, not upgrade, on
// purpose: Proxmox requires it, and plain upgrade holds back anything that
// needs a new dependency -- which is exactly how new kernels and pve-manager
// releases arrive.
//
// dpkg --configure -a first finishes whatever an interrupted earlier attempt
// left half-configured; apt refuses to run at all until that is done, and it is
// a no-op otherwise.
func (c *Client) FullUpgrade(ctx context.Context, ip string) error {
	if c.dryRun {
		c.log.Infof("(dry-run) would run apt-get full-upgrade on %s", ip)
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, upgradeTimeout)
	defer cancel()
	script := noninteractive +
		"dpkg --force-confdef --force-confold --configure -a && " +
		"apt-get -y -q " + dpkgOpts + " full-upgrade"
	_, err := c.run(ctx, ip, script)
	return err
}

// Kernel returns the host's running kernel release.
func (c *Client) Kernel(ctx context.Context, ip string) (string, error) {
	out, err := c.run(ctx, ip, "uname -r")
	return strings.TrimSpace(out), err
}

// Package is one line of the simulation.
type Package struct {
	Name string
	Old  string // empty for a new install
	New  string // empty for a removal
}

type Plan struct {
	Upgrade []Package
	Install []Package
	Remove  []Package
}

func (p *Plan) Empty() bool {
	return len(p.Upgrade) == 0 && len(p.Install) == 0 && len(p.Remove) == 0
}

func (p *Plan) Summary() string {
	return fmt.Sprintf("%d to upgrade, %d new, %d to remove", len(p.Upgrade), len(p.Install), len(p.Remove))
}

// Find returns the upgrade or install entry for a package, if any.
func (p *Plan) Find(name string) *Package {
	for _, list := range [][]Package{p.Upgrade, p.Install} {
		for i := range list {
			if list[i].Name == name {
				return &list[i]
			}
		}
	}
	return nil
}

var (
	// Inst pve-manager [9.2.4] (9.2.5 Proxmox VE:9.x/trixie [all]) []
	// Inst proxmox-kernel-7.0.15-1-pve-signed (7.0.15-1 Proxmox ...)
	instRE = regexp.MustCompile(`^Inst (\S+) (?:\[([^\]]+)\] )?\((\S+)`)
	// Remv foo [1.0]
	remvRE = regexp.MustCompile(`^Remv (\S+)(?: \[([^\]]+)\])?`)
)

// ParsePlan reads `apt-get -s` output.
func ParsePlan(out string) *Plan {
	p := &Plan{}
	for _, line := range strings.Split(out, "\n") {
		if m := instRE.FindStringSubmatch(line); m != nil {
			pkg := Package{Name: m[1], Old: m[2], New: m[3]}
			if pkg.Old == "" {
				p.Install = append(p.Install, pkg)
			} else {
				p.Upgrade = append(p.Upgrade, pkg)
			}
			continue
		}
		if m := remvRE.FindStringSubmatch(line); m != nil {
			p.Remove = append(p.Remove, Package{Name: m[1], Old: m[2]})
		}
	}
	return p
}

// guardedPrefixes are the packages whose major version must not move in a
// routine rolling upgrade. A Ceph major release has a required daemon order
// (mons, then mgrs, then OSDs, cluster-wide) that a node-by-node reboot breaks,
// and a pve-manager major is a distribution upgrade with its own checklist
// (pve8to9 and friends).
var guardedPrefixes = []string{"ceph", "librados", "librbd", "pve-manager", "proxmox-ve"}

// MajorBumps lists guarded packages whose major version changes.
func (p *Plan) MajorBumps() []string {
	var out []string
	for _, pkg := range p.Upgrade {
		if !guarded(pkg.Name) {
			continue
		}
		if major(pkg.Old) != major(pkg.New) {
			out = append(out, fmt.Sprintf("%s %s -> %s", pkg.Name, pkg.Old, pkg.New))
		}
	}
	return out
}

func guarded(name string) bool {
	for _, pfx := range guardedPrefixes {
		if strings.HasPrefix(name, pfx) {
			return true
		}
	}
	return false
}

// major returns the leading numeric component of a Debian version, ignoring
// the epoch: "2:20.2.4-pve4" -> "20".
func major(v string) string {
	if i := strings.IndexByte(v, ':'); i >= 0 {
		v = v[i+1:]
	}
	end := 0
	for end < len(v) && v[end] >= '0' && v[end] <= '9' {
		end++
	}
	return v[:end]
}

func firstWord(s string) string {
	s = strings.TrimPrefix(s, noninteractive)
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
