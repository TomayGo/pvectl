package pve

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// This file is HA node maintenance and the rules that decide where HA puts a
// resource. pvectl no longer picks evacuation targets itself: PVE 9's
// node-affinity rules make that the CRM's job (a non-strict rule only lets a
// resource sit on the highest-priority online node, so a hand-picked target is
// either refused or pulled straight back). Putting the node in HA maintenance
// takes it out of the CRM's set of online nodes, and the CRM then moves every
// resource where its rules allow and keeps new ones off until maintenance ends.

// flexBool decodes the booleans the PVE API spells as 1/0, "1"/"0" or
// true/false depending on where the value came from.
type flexBool bool

func (b *flexBool) UnmarshalJSON(raw []byte) error {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	switch strings.ToLower(s) {
	case "", "null", "0", "false", "no", "off":
		*b = false
	case "1", "true", "yes", "on":
		*b = true
	default:
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return fmt.Errorf("not a boolean: %s", raw)
		}
		*b = n != 0
	}
	return nil
}

// ---------- manager status ----------

// HAService is one entry of the CRM's service_status.
type HAService struct {
	SID    string `json:"-"`
	Node   string `json:"node"`
	Target string `json:"target"`
	State  string `json:"state"`
	// MaintenanceNode is where the CRM will return the service once that
	// node leaves maintenance.
	MaintenanceNode string `json:"maintenance_node"`
}

// HAManagerStatus is GET /cluster/ha/status/manager_status: the CRM's own
// state, and the only API view of a node maintenance request.
type HAManagerStatus struct {
	Manager struct {
		Master      string `json:"master_node"`
		NodeRequest map[string]struct {
			Maintenance flexBool `json:"maintenance"`
		} `json:"node_request"`
		NodeStatus    map[string]string    `json:"node_status"`
		ServiceStatus map[string]HAService `json:"service_status"`
		Disarm        json.RawMessage      `json:"disarm"`
	} `json:"manager_status"`
	LRM map[string]struct {
		Mode  string `json:"mode"`
		State string `json:"state"`
	} `json:"lrm_status"`
}

func (c *Client) HAManagerStatus(ctx context.Context) (*HAManagerStatus, error) {
	var ms HAManagerStatus
	if err := c.Get(ctx, "/cluster/ha/status/manager_status", nil, &ms); err != nil {
		return nil, err
	}
	return &ms, nil
}

// MaintenanceRequested reports whether the CRM has accepted a maintenance
// request for the node. It is set as soon as the CRM processes the command and
// cleared as soon as it processes the disable, whether or not the node is up.
func (m *HAManagerStatus) MaintenanceRequested(node string) bool {
	r, ok := m.Manager.NodeRequest[node]
	return ok && bool(r.Maintenance)
}

// NodeState is the CRM's state for the node: online, maintenance, unknown,
// fence or gone.
func (m *HAManagerStatus) NodeState(node string) string {
	return m.Manager.NodeStatus[node]
}

// MaintenanceNodes lists every node that is in maintenance or has asked for it.
func (m *HAManagerStatus) MaintenanceNodes() []string {
	set := map[string]bool{}
	for n := range m.Manager.NodeRequest {
		if m.MaintenanceRequested(n) {
			set[n] = true
		}
	}
	for n, st := range m.Manager.NodeStatus {
		if st == "maintenance" {
			set[n] = true
		}
	}
	return sortedKeys(set)
}

// Disarmed reports whether the HA stack is disarmed. The CRM ignores
// maintenance requests then.
func (m *HAManagerStatus) Disarmed() bool {
	d := bytes.TrimSpace(m.Manager.Disarm)
	return len(d) > 0 && string(d) != "null" && string(d) != "{}"
}

// Services returns service_status as a list sorted by SID.
func (m *HAManagerStatus) Services() []HAService {
	out := make([]HAService, 0, len(m.Manager.ServiceStatus))
	for sid, s := range m.Manager.ServiceStatus {
		s.SID = sid
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SID < out[j].SID })
	return out
}

// ActiveServicesOn lists the services that keep a node from being evacuated:
// those placed on it, or migrating to it, in any state but the ones PVE's
// count_active_services skips. Stopped and errored services stay where they
// are under maintenance -- they hold nothing and the LRM lets go without them.
func (m *HAManagerStatus) ActiveServicesOn(node string) []HAService {
	var out []HAService
	for _, s := range m.Services() {
		if s.Node != node && s.Target != node {
			continue
		}
		switch s.State {
		case "", "stopped", "freeze", "error", "request_start":
			continue
		}
		out = append(out, s)
	}
	return out
}

// ReturningTo lists the services the CRM will move back to node once it
// leaves maintenance.
func (m *HAManagerStatus) ReturningTo(node string) []string {
	var out []string
	for _, s := range m.Services() {
		if s.MaintenanceNode == node && s.Node != node {
			out = append(out, s.SID)
		}
	}
	return out
}

// ---------- node maintenance ----------

// runHAManager runs the local ha-manager CLI. A variable so tests can stand in.
var runHAManager = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "ha-manager", args...).CombinedOutput()
}

// NodeMaintenanceArgs is the ha-manager command line that turns HA
// maintenance on or off for a node.
func NodeMaintenanceArgs(node string, on bool) []string {
	verb := "disable"
	if on {
		verb = "enable"
	}
	return []string{"crm-command", "node-maintenance", verb, node}
}

// SetHANodeMaintenance queues a node maintenance request with the CRM.
//
// This is the one Proxmox operation that does not go through the API: PVE 9
// has no endpoint for it (only the ha-manager CLI queues the command into
// /etc/pve/ha/crm_commands), so it runs the local ha-manager. pvectl always
// runs on a cluster node as root, and the queue is cluster-wide, so which node
// runs it does not matter -- including the target itself.
func (c *Client) SetHANodeMaintenance(ctx context.Context, node string, on bool) error {
	args := NodeMaintenanceArgs(node, on)
	line := "ha-manager " + strings.Join(args, " ")
	if c.DryRun {
		c.log.Infof("(dry-run) would run %s", line)
		return nil
	}
	c.log.Infof("running %s", line)
	out, err := runHAManager(ctx, args...)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("%s: %w: %s", line, err, msg)
		}
		return fmt.Errorf("%s: %w", line, err)
	}
	return nil
}

// ---------- rules ----------

// HARule is one entry of GET /cluster/ha/rules. Only the node-affinity fields
// are decoded.
type HARule struct {
	Rule      string `json:"rule"`
	Type      string `json:"type"`
	Resources string `json:"resources"`
	Nodes     string `json:"nodes"`
	// Affinity is "positive" (run on these nodes) or "negative" (avoid
	// them). Empty means positive.
	Affinity string   `json:"affinity"`
	Strict   flexBool `json:"strict"`
	Disable  flexBool `json:"disable"`
}

// HANodeAffinityRules returns the node-affinity rules, enabled or not.
func (c *Client) HANodeAffinityRules(ctx context.Context) ([]HARule, error) {
	var out []HARule
	err := c.Get(ctx, "/cluster/ha/rules", url.Values{"type": {"node-affinity"}}, &out)
	return out, err
}

// listSep is what PVE's split_list accepts between list items.
var listSep = regexp.MustCompile(`[,;\s]+`)

func splitList(s string) []string {
	var out []string
	for _, f := range listSep.Split(strings.TrimSpace(s), -1) {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// Covers reports whether the rule applies to sid.
func (r HARule) Covers(sid string) bool {
	for _, s := range splitList(r.Resources) {
		if s == sid {
			return true
		}
	}
	return false
}

// NodePriorities parses "pve-1:2,pve-3:1" (priority defaults to 0).
func (r HARule) NodePriorities() map[string]int {
	out := map[string]int{}
	for _, f := range splitList(r.Nodes) {
		name, prio, found := strings.Cut(f, ":")
		p := 0
		if found {
			if n, err := strconv.Atoi(prio); err == nil {
				p = n
			}
		}
		out[name] = p
	}
	return out
}

// effective mirrors how PVE compiles a node-affinity rule: a negative rule
// becomes the complement of its nodes, and a non-strict rule lets the resource
// fall back to every other cluster node at priority -1.
func (r HARule) effective(cluster []string) map[string]int {
	listed := r.NodePriorities()
	eff := map[string]int{}
	if r.Affinity == "negative" {
		for _, n := range cluster {
			if _, avoid := listed[n]; !avoid {
				eff[n] = 0
			}
		}
	} else {
		for n, p := range listed {
			eff[n] = p
		}
	}
	if !r.Strict {
		for _, n := range cluster {
			if _, ok := eff[n]; !ok {
				eff[n] = -1
			}
		}
	}
	return eff
}

// HAPlacement is where the CRM may put a resource, as far as node affinity
// goes. Resource-affinity rules and the CRS scheduler narrow it further; the
// CRM, not pvectl, makes the final choice.
type HAPlacement struct {
	// Rule is the node-affinity rule that applies, or "" for none.
	Rule   string
	Strict bool
	// Allowed are the available nodes the resource may run on; Preferred the
	// subset at the highest priority, which is where the CRM puts it.
	Allowed   []string
	Preferred []string
}

// PlaceHA predicts where sid can go given the nodes available to it (online,
// not in maintenance), using the first enabled node-affinity rule that
// covers it -- PVE allows only one per resource.
func PlaceHA(rules []HARule, sid string, cluster, available []string) HAPlacement {
	var rule *HARule
	for i := range rules {
		r := &rules[i]
		if bool(r.Disable) || (r.Type != "" && r.Type != "node-affinity") || !r.Covers(sid) {
			continue
		}
		rule = r
		break
	}
	avail := map[string]bool{}
	for _, n := range available {
		avail[n] = true
	}
	if rule == nil {
		all := sortedKeys(avail)
		return HAPlacement{Allowed: all, Preferred: all}
	}
	pl := HAPlacement{Rule: rule.Rule, Strict: bool(rule.Strict)}
	eff := rule.effective(cluster)
	best := 0
	for n, p := range eff {
		if !avail[n] {
			continue
		}
		if len(pl.Allowed) == 0 || p > best {
			best = p
		}
		pl.Allowed = append(pl.Allowed, n)
	}
	sort.Strings(pl.Allowed)
	for _, n := range pl.Allowed {
		if eff[n] == best {
			pl.Preferred = append(pl.Preferred, n)
		}
	}
	return pl
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
