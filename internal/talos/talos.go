// Package talos drives the Talos control-plane VMs through the machinery API.
//
// Each call talks straight to the target node as its own endpoint (the
// equivalent of `talosctl -n IP -e IP`). Proxying through a peer would mean a
// shutdown request travels via a node we may be about to stop next.
package talos

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"

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
