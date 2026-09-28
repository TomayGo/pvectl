// Package talos drives the Talos control-plane VMs through the machinery API.
//
// Each call talks straight to the target node as its own endpoint (the
// equivalent of `talosctl -n IP -e IP`). Proxying through a peer would mean a
// shutdown request travels via a node we may be about to stop next.
package talos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/cosi-project/runtime/pkg/safe"
	"github.com/siderolabs/talos/pkg/machinery/api/common"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"
	configres "github.com/siderolabs/talos/pkg/machinery/resources/config"

	plog "github.com/tamago/pvectl/internal/log"
)

type Client struct {
	configPath string
	log        *plog.Logger
	dryRun     bool
}

func New(talosconfig string, lg *plog.Logger, dryRun bool) *Client {
	return &Client{configPath: talosconfig, log: lg, dryRun: dryRun}
}

// connect dials one node directly. The caller closes the returned client.
func (c *Client) connect(ctx context.Context, ip string) (*talosclient.Client, context.Context, error) {
	cl, err := talosclient.New(ctx,
		talosclient.WithConfigFromFile(c.configPath),
		talosclient.WithEndpoints(ip),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("talos client for %s (config %s): %w", ip, c.configPath, err)
	}
	return cl, talosclient.WithNode(ctx, ip), nil
}

// Shutdown powers off a Talos node. The gRPC call frequently fails to return
// cleanly -- the machine is on its way down and drops the connection -- so a
// transport error here is not treated as a failure. Confirmation comes from
// WaitDown instead.
func (c *Client) Shutdown(ctx context.Context, ip string) error {
	if c.dryRun {
		c.log.Infof("(dry-run) would send talos shutdown to %s", ip)
		return nil
	}
	cl, nctx, err := c.connect(ctx, ip)
	if err != nil {
		return err
	}
	defer cl.Close()
	sctx, cancel := context.WithTimeout(nctx, 30*time.Second)
	defer cancel()
	if err := cl.Shutdown(sctx); err != nil {
		if isDisconnect(err) {
			c.log.Debugf("talos %s dropped the connection while shutting down (expected)", ip)
			return nil
		}
		return fmt.Errorf("talos shutdown %s: %w", ip, err)
	}
	return nil
}

func isDisconnect(err error) bool {
	s := err.Error()
	for _, frag := range []string{"Unavailable", "connection refused", "EOF", "transport is closing", "context deadline exceeded"} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}

// Alive reports whether the node answers the Version API.
//
// The error return is the whole point: a node that is down and a talosconfig we
// cannot even build a client from both produce alive=false, and callers that
// treat the second as "already shut down" would skip the graceful stop and let
// the host poweroff kill a live control-plane node.
func (c *Client) Alive(ctx context.Context, ip string) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cl, nctx, err := c.connect(cctx, ip)
	if err != nil {
		return false, err
	}
	defer cl.Close()
	_, err = cl.Version(nctx)
	return err == nil, nil
}

// WaitDown blocks until the node stops answering, which is the only reliable
// confirmation that the shutdown took effect.
func (c *Client) WaitDown(ctx context.Context, ip string, timeout time.Duration) error {
	if c.dryRun {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		alive, err := c.Alive(ctx, ip)
		if err != nil {
			return fmt.Errorf("cannot tell whether %s is down: %w", ip, err)
		}
		if !alive {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("talos node %s still responding after %s", ip, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (c *Client) WaitUp(ctx context.Context, ip string, timeout time.Duration) error {
	if c.dryRun {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		alive, err := c.Alive(ctx, ip)
		if err != nil {
			return fmt.Errorf("cannot reach %s: %w", ip, err)
		}
		if alive {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("talos node %s not answering after %s", ip, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

type EtcdHealth struct {
	// Members is the union of member hostnames as seen by every node that
	// answered.
	Members []string
	// Responding lists the node IPs that answered EtcdStatus without errors.
	Responding []string
	Unhealthy  []string
	Leader     uint64
}

func (h EtcdHealth) OK(expect int) bool {
	return len(h.Members) == expect && len(h.Responding) == expect && len(h.Unhealthy) == 0
}

func (h EtcdHealth) Summary(expect int) string {
	s := fmt.Sprintf("%d/%d members, %d/%d nodes responding", len(h.Members), expect, len(h.Responding), expect)
	if len(h.Unhealthy) > 0 {
		s += "; unhealthy: " + strings.Join(h.Unhealthy, ", ")
	}
	return s
}

// Etcd queries every listed node for etcd membership and raft status. Asking
// all of them (rather than one) is the point: a node that cannot answer is
// itself the finding.
func (c *Client) Etcd(ctx context.Context, ips []string) (*EtcdHealth, error) {
	h := &EtcdHealth{}
	seen := map[string]bool{}
	for _, ip := range ips {
		func() {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			cl, nctx, err := c.connect(cctx, ip)
			if err != nil {
				h.Unhealthy = append(h.Unhealthy, fmt.Sprintf("%s: %v", ip, err))
				return
			}
			defer cl.Close()
			ml, err := cl.EtcdMemberList(nctx, &machineapi.EtcdMemberListRequest{})
			if err != nil {
				h.Unhealthy = append(h.Unhealthy, fmt.Sprintf("%s: member list: %v", ip, err))
				return
			}
			for _, msg := range ml.GetMessages() {
				for _, m := range msg.GetMembers() {
					if !seen[m.GetHostname()] {
						seen[m.GetHostname()] = true
						h.Members = append(h.Members, m.GetHostname())
					}
				}
			}
			st, err := cl.EtcdStatus(nctx)
			if err != nil {
				h.Unhealthy = append(h.Unhealthy, fmt.Sprintf("%s: status: %v", ip, err))
				return
			}
			for _, msg := range st.GetMessages() {
				ms := msg.GetMemberStatus()
				if len(ms.GetErrors()) > 0 {
					h.Unhealthy = append(h.Unhealthy, fmt.Sprintf("%s: %s", ip, strings.Join(ms.GetErrors(), "; ")))
					return
				}
				if ms.GetLeader() == 0 {
					h.Unhealthy = append(h.Unhealthy, fmt.Sprintf("%s: no raft leader", ip))
					return
				}
				h.Leader = ms.GetLeader()
			}
			h.Responding = append(h.Responding, ip)
		}()
	}
	sort.Strings(h.Members)
	sort.Strings(h.Responding)
	return h, nil
}

// Version returns the Talos release tag the node is running, e.g. "v1.13.6".
func (c *Client) Version(ctx context.Context, ip string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cl, nctx, err := c.connect(cctx, ip)
	if err != nil {
		return "", err
	}
	defer cl.Close()
	resp, err := cl.Version(nctx)
	if err != nil {
		return "", fmt.Errorf("talos version %s: %w", ip, err)
	}
	for _, m := range resp.GetMessages() {
		if tag := m.GetVersion().GetTag(); tag != "" {
			return tag, nil
		}
	}
	return "", fmt.Errorf("talos %s reported no version", ip)
}

// InstallImage reads machine.install.image from the node's active config. It
// carries the Image Factory schematic, so an upgrade built from it keeps the
// node's system extensions (qemu-guest-agent here).
func (c *Client) InstallImage(ctx context.Context, ip string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cl, nctx, err := c.connect(cctx, ip)
	if err != nil {
		return "", err
	}
	defer cl.Close()
	mc, err := safe.StateGetByID[*configres.MachineConfig](nctx, cl.COSI, configres.ActiveID)
	if err != nil {
		return "", fmt.Errorf("read machine config from %s: %w", ip, err)
	}
	cfg := mc.Config()
	if cfg == nil || cfg.Machine() == nil || cfg.Machine().Install() == nil {
		return "", fmt.Errorf("machine config on %s has no install section", ip)
	}
	img := cfg.Machine().Install().Image()
	if img == "" {
		return "", fmt.Errorf("machine config on %s has no install image", ip)
	}
	return img, nil
}

// Upgrade writes a new Talos release to the node's inactive boot slot and
// makes it the default, without rebooting. That is what the LifecycleService
// API (Talos >= 1.13) does, and it is the reason the upgrade folds into a host
// reboot for free: the Talos VM's next boot -- whenever and however it happens
// -- is the upgrade.
//
// The image is pulled first, as talosctl does, so a bad reference fails here
// with a registry error instead of inside the installer.
func (c *Client) Upgrade(ctx context.Context, ip, image string) error {
	if c.dryRun {
		c.log.Infof("(dry-run) would install %s on %s (no reboot)", image, ip)
		return nil
	}
	cl, nctx, err := c.connect(ctx, ip)
	if err != nil {
		return err
	}
	defer cl.Close()
	containerd := &common.ContainerdInstance{
		Driver:    common.ContainerDriver_CRI,
		Namespace: common.ContainerdNamespace_NS_SYSTEM,
	}

	c.log.Infof("pulling %s on %s", image, ip)
	pull, err := cl.ImageClient.Pull(nctx, &machineapi.ImageServicePullRequest{Containerd: containerd, ImageRef: image})
	if err != nil {
		return fmt.Errorf("pull %s on %s: %w", image, ip, err)
	}
	for {
		msg, err := pull.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("pull %s on %s: %w", image, ip, err)
		}
		if name := msg.GetName(); name != "" {
			c.log.Debugf("pulled %s", name)
		}
	}

	c.log.Infof("installing %s on %s", image, ip)
	up, err := cl.LifecycleClient.Upgrade(nctx, &machineapi.LifecycleServiceUpgradeRequest{
		Containerd: containerd,
		Source:     &machineapi.InstallArtifactsSource{ImageName: image},
	})
	if err != nil {
		return fmt.Errorf("upgrade %s: %w", ip, err)
	}
	exit := int32(-1)
	for {
		msg, err := up.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("upgrade %s: %w", ip, err)
		}
		switch r := msg.GetProgress().GetResponse().(type) {
		case *machineapi.LifecycleServiceInstallProgress_Message:
			c.log.Debugf("installer: %s", strings.TrimSpace(r.Message))
		case *machineapi.LifecycleServiceInstallProgress_ExitCode:
			exit = r.ExitCode
		}
	}
	switch exit {
	case 0:
		return nil
	case -1:
		return fmt.Errorf("upgrade %s: the installer stream ended without an exit code", ip)
	default:
		return fmt.Errorf("upgrade %s: installer exited with code %d (see the transcript for its output)", ip, exit)
	}
}

// Rollback points the boot loader back at the previous release and reboots the
// node into it.
func (c *Client) Rollback(ctx context.Context, ip string) error {
	if c.dryRun {
		c.log.Infof("(dry-run) would roll %s back to its previous release", ip)
		return nil
	}
	cl, nctx, err := c.connect(ctx, ip)
	if err != nil {
		return err
	}
	defer cl.Close()
	rctx, cancel := context.WithTimeout(nctx, 30*time.Second)
	defer cancel()
	if err := cl.Rollback(rctx); err != nil && !isDisconnect(err) {
		return fmt.Errorf("talos rollback %s: %w", ip, err)
	}
	return nil
}
