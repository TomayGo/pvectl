package pve

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- cluster ----------

type ClusterNode struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	IP     string `json:"ip"`
	Online int    `json:"online"`
	Local  int    `json:"local"`
	NodeID int    `json:"nodeid"`
	// Only present on the "cluster" entry.
	Nodes   int `json:"nodes"`
	Quorate int `json:"quorate"`
	Version int `json:"version"`
}

type ClusterStatus struct {
	Name     string
	Quorate  bool
	Expected int
	Online   []string
	Offline  []string
	Nodes    map[string]ClusterNode
}

func (c *Client) ClusterStatus(ctx context.Context) (*ClusterStatus, error) {
	var raw []ClusterNode
	if err := c.Get(ctx, "/cluster/status", nil, &raw); err != nil {
		return nil, err
	}
	st := &ClusterStatus{Nodes: map[string]ClusterNode{}}
	for _, e := range raw {
		if e.Type == "cluster" {
			st.Name = e.Name
			st.Quorate = e.Quorate == 1
			st.Expected = e.Nodes
			continue
		}
		st.Nodes[e.Name] = e
		if e.Online == 1 {
			st.Online = append(st.Online, e.Name)
		} else {
			st.Offline = append(st.Offline, e.Name)
		}
	}
	sort.Strings(st.Online)
	sort.Strings(st.Offline)
	return st, nil
}

// ---------- ceph ----------

type CephStatus struct {
	Health struct {
		Status string `json:"status"`
		Checks map[string]struct {
			Severity string `json:"severity"`
			Summary  struct {
				Message string `json:"message"`
			} `json:"summary"`
		} `json:"checks"`
	} `json:"health"`
	PGMap struct {
		NumPGs     int `json:"num_pgs"`
		PGsByState []struct {
			StateName string `json:"state_name"`
			Count     int    `json:"count"`
		} `json:"pgs_by_state"`
		DegradedObjects  int64 `json:"degraded_objects"`
		MisplacedObjects int64 `json:"misplaced_objects"`
		UnfoundObjects   int64 `json:"unfound_objects"`
	} `json:"pgmap"`
	OSDMap struct {
		NumOSDs       int `json:"num_osds"`
		NumUpOSDs     int `json:"num_up_osds"`
		NumInOSDs     int `json:"num_in_osds"`
		NumRemappedPG int `json:"num_remapped_pgs"`
	} `json:"osdmap"`
	QuorumNames []string `json:"quorum_names"`
	MonMap      struct {
		Mons []struct {
			Name string `json:"name"`
		} `json:"mons"`
	} `json:"monmap"`
}

// AllPGsActiveClean reports whether every PG is in exactly active+clean, which
// is the gate for advancing a rolling reboot to the next node.
func (s *CephStatus) AllPGsActiveClean() (bool, string) {
	clean := 0
	var other []string
	for _, st := range s.PGMap.PGsByState {
		if st.StateName == "active+clean" {
			clean += st.Count
			continue
		}
		other = append(other, fmt.Sprintf("%s=%d", st.StateName, st.Count))
	}
	if len(other) == 0 && clean == s.PGMap.NumPGs && s.PGMap.NumPGs > 0 {
		return true, fmt.Sprintf("%d/%d PGs active+clean", clean, s.PGMap.NumPGs)
	}
	return false, fmt.Sprintf("%d/%d PGs active+clean, other: %s", clean, s.PGMap.NumPGs, strings.Join(other, " "))
}

func (s *CephStatus) HealthSummary() string {
	if len(s.Health.Checks) == 0 {
		return s.Health.Status
	}
	var parts []string
	for k, v := range s.Health.Checks {
		parts = append(parts, fmt.Sprintf("%s: %s", k, v.Summary.Message))
	}
	sort.Strings(parts)
	return s.Health.Status + " (" + strings.Join(parts, "; ") + ")"
}

func (c *Client) CephStatus(ctx context.Context) (*CephStatus, error) {
	var s CephStatus
	if err := c.Get(ctx, "/cluster/ceph/status", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

type CephFlag struct {
	Name        string `json:"name"`
	Value       int    `json:"value"`
	Description string `json:"description"`
}

func (c *Client) CephFlags(ctx context.Context) (map[string]bool, error) {
	var raw []CephFlag
	if err := c.Get(ctx, "/cluster/ceph/flags", nil, &raw); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, f := range raw {
		out[f.Name] = f.Value != 0
	}
	return out, nil
}

// SetCephFlag uses the per-flag endpoint, which runs synchronously. The bulk
// PUT /cluster/ceph/flags forks a worker task instead, so we would have to poll
// a UPID for something that should be immediate.
func (c *Client) SetCephFlag(ctx context.Context, flag string, on bool) error {
	v := url.Values{"value": {boolParam(on)}}
	return c.Put(ctx, "/cluster/ceph/flags/"+url.PathEscape(flag), v, nil)
}

func boolParam(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// ---------- HA ----------

type HAResource struct {
	SID    string `json:"sid"`
	Type   string `json:"type"`
	State  string `json:"state"`
	Group  string `json:"group"`
	Digest string `json:"digest"`
}

func (c *Client) HAResources(ctx context.Context) ([]HAResource, error) {
	var out []HAResource
	err := c.Get(ctx, "/cluster/ha/resources", nil, &out)
	return out, err
}

type HAStatusEntry struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Node   string `json:"node"`
	Status string `json:"status"`
	// service entries only
	SID       string `json:"sid"`
	State     string `json:"state"`
	CRMState  string `json:"crm_state"`
	ReqState  string `json:"request_state"`
	Quorate   int    `json:"quorate"`
	Timestamp int64  `json:"timestamp"`
}

type HAStatus struct {
	Master   string
	Quorate  bool
	Services []HAStatusEntry
	LRM      map[string]string
}

// ServicesOn lists the HA services currently placed on a node. groups.cfg is
// empty in this cluster, so placement floats -- it must be read every time
// rather than assumed.
func (h *HAStatus) ServicesOn(node string) []HAStatusEntry {
	var out []HAStatusEntry
	for _, s := range h.Services {
		if s.Node == node {
			out = append(out, s)
		}
	}
	return out
}

func (c *Client) HAStatus(ctx context.Context) (*HAStatus, error) {
	var raw []HAStatusEntry
	if err := c.Get(ctx, "/cluster/ha/status/current", nil, &raw); err != nil {
		return nil, err
	}
	st := &HAStatus{LRM: map[string]string{}}
	for _, e := range raw {
		switch e.Type {
		case "quorum":
			st.Quorate = e.Quorate == 1
		case "master":
			st.Master = e.Node
		case "lrm":
			st.LRM[e.Node] = e.Status
		case "service":
			st.Services = append(st.Services, e)
		}
	}
	sort.Slice(st.Services, func(i, j int) bool { return st.Services[i].SID < st.Services[j].SID })
	return st, nil
}

// HAMigrate asks the CRM to move one service. We drive this explicitly rather
// than relying on datacenter.cfg shutdown_policy=migrate, because the implicit
// path gives no progress and no timeout to hang a decision on.
func (c *Client) HAMigrate(ctx context.Context, sid, target string) error {
	return c.Post(ctx, "/cluster/ha/resources/"+url.PathEscape(sid)+"/migrate",
		url.Values{"node": {target}}, nil)
}

// ---------- resources ----------

type Resource struct {
	ID       string  `json:"id"`
	Type     string  `json:"type"`
	Node     string  `json:"node"`
	VMID     int     `json:"vmid"`
	Name     string  `json:"name"`
	Status   string  `json:"status"`
	HAState  string  `json:"hastate"`
	MaxMem   int64   `json:"maxmem"`
	Mem      int64   `json:"mem"`
	MaxCPU   float64 `json:"maxcpu"`
	Uptime   int64   `json:"uptime"`
	Template int     `json:"template"`
}

func (r Resource) Kind() string {
	if r.Type == "lxc" {
		return "lxc"
	}
	return "qemu"
}

// SIDKind is the prefix HA uses for this guest ("vm:106", "ct:500"), which is
// spelled differently from the API path segment returned by Kind().
func (r Resource) SIDKind() string {
	if r.Type == "lxc" {
		return "ct"
	}
	return "vm"
}

func (c *Client) Resources(ctx context.Context, kind string) ([]Resource, error) {
	v := url.Values{}
	if kind != "" {
		v.Set("type", kind)
	}
	var out []Resource
	err := c.Get(ctx, "/cluster/resources", v, &out)
	return out, err
}

// GuestsOn returns the running guests on a node, keyed by vmid.
func (c *Client) GuestsOn(ctx context.Context, node string) (map[int]Resource, error) {
	all, err := c.Resources(ctx, "vm")
	if err != nil {
		return nil, err
	}
	out := map[int]Resource{}
	for _, r := range all {
		if r.Node == node {
			out[r.VMID] = r
		}
	}
	return out, nil
}

// ---------- nodes ----------

type NodeStatus struct {
	Uptime int64 `json:"uptime"`
	Memory struct {
		Total int64 `json:"total"`
		Used  int64 `json:"used"`
		Free  int64 `json:"free"`
	} `json:"memory"`
	CPU      float64 `json:"cpu"`
	PVEVer   string  `json:"pveversion"`
	Kversion string  `json:"kversion"`
}

func (c *Client) NodeStatus(ctx context.Context, node string) (*NodeStatus, error) {
	var s NodeStatus
	if err := c.Get(ctx, "/nodes/"+url.PathEscape(node)+"/status", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// NodeCommand issues shutdown or reboot. It must be sent through some *other*
// node's pveproxy: aimed at the target itself, the connection dies before the
// response and the caller cannot tell success from failure.
func (c *Client) NodeCommand(ctx context.Context, node, command string) error {
	return c.Post(ctx, "/nodes/"+url.PathEscape(node)+"/status",
		url.Values{"command": {command}}, nil)
}

type NodeListEntry struct {
	Node   string `json:"node"`
	Status string `json:"status"`
	Uptime int64  `json:"uptime"`
}

func (c *Client) Nodes(ctx context.Context) ([]NodeListEntry, error) {
	var out []NodeListEntry
	err := c.Get(ctx, "/nodes", nil, &out)
	return out, err
}

// ---------- guests ----------

type GuestStatus struct {
	Status    string `json:"status"`
	QMPStatus string `json:"qmpstatus"`
	Name      string `json:"name"`
	Lock      string `json:"lock"`
}

func (c *Client) GuestStatus(ctx context.Context, node, kind string, vmid int) (*GuestStatus, error) {
	var s GuestStatus
	p := fmt.Sprintf("/nodes/%s/%s/%d/status/current", url.PathEscape(node), kind, vmid)
	if err := c.Get(ctx, p, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// GuestShutdown sends an ACPI shutdown and returns the worker UPID.
func (c *Client) GuestShutdown(ctx context.Context, node, kind string, vmid int, timeout time.Duration, forceStop bool) (string, error) {
	v := url.Values{}
	if timeout > 0 {
		v.Set("timeout", strconv.Itoa(int(timeout.Seconds())))
	}
	if forceStop {
		v.Set("forceStop", "1")
	}
	var upid string
	p := fmt.Sprintf("/nodes/%s/%s/%d/status/shutdown", url.PathEscape(node), kind, vmid)
	err := c.Post(ctx, p, v, &upid)
	return upid, err
}

func (c *Client) GuestStop(ctx context.Context, node, kind string, vmid int) (string, error) {
	var upid string
	p := fmt.Sprintf("/nodes/%s/%s/%d/status/stop", url.PathEscape(node), kind, vmid)
	err := c.Post(ctx, p, nil, &upid)
	return upid, err
}

func (c *Client) GuestStart(ctx context.Context, node, kind string, vmid int) (string, error) {
	var upid string
	p := fmt.Sprintf("/nodes/%s/%s/%d/status/start", url.PathEscape(node), kind, vmid)
	err := c.Post(ctx, p, nil, &upid)
	return upid, err
}

// GuestConfig returns the raw config map. Used by `config discover` to find
// guests with hostpci*/usb* keys -- those cannot migrate and must be stopped.
func (c *Client) GuestConfig(ctx context.Context, node, kind string, vmid int) (map[string]any, error) {
	var m map[string]any
	p := fmt.Sprintf("/nodes/%s/%s/%d/config", url.PathEscape(node), kind, vmid)
	if err := c.Get(ctx, p, nil, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// ---------- tasks ----------

type Task struct {
	UPID      string `json:"upid"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	ExitState string `json:"exitstatus"`
	Node      string `json:"node"`
	ID        string `json:"id"`
	User      string `json:"user"`
	StartTime int64  `json:"starttime"`
	EndTime   int64  `json:"endtime"`
}

// RunningTasks lists in-flight worker tasks on a node. preflight uses this to
// refuse a shutdown while vzdump or pvesr is mid-run.
func (c *Client) RunningTasks(ctx context.Context, node string) ([]Task, error) {
	v := url.Values{"source": {"active"}, "limit": {"200"}}
	var out []Task
	err := c.Get(ctx, "/nodes/"+url.PathEscape(node)+"/tasks", v, &out)
	return out, err
}

func (c *Client) TaskStatus(ctx context.Context, node, upid string) (*Task, error) {
	var t Task
	p := fmt.Sprintf("/nodes/%s/tasks/%s/status", url.PathEscape(node), url.PathEscape(upid))
	if err := c.Get(ctx, p, nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// WaitTask polls a worker task to completion. An empty upid (which is what
// --dry-run produces) succeeds immediately.
func (c *Client) WaitTask(ctx context.Context, node, upid string, timeout time.Duration) error {
	if upid == "" {
		return nil
	}
	deadline := time.Now().Add(timeout)
	for {
		t, err := c.TaskStatus(ctx, node, upid)
		if err != nil {
			return err
		}
		if t.Status != "running" {
			if t.ExitState != "" && t.ExitState != "OK" {
				return fmt.Errorf("task %s finished with %q", upid, t.ExitState)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("task %s still running after %s", upid, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
