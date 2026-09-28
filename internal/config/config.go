// Package config loads pvectl's node/cluster definition.
//
// The split matters: config.yaml lives in /etc/pve so every node sees the same
// definition, but /etc/pve is FUSE-mounted root:www-data 0640 and pveproxy
// serves port 8006 as www-data -- anything there is readable by the web UI's
// process. So config.yaml holds only non-secret topology, and credentials live
// in a node-local secrets dir at 0600 root.
package config

import (
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPath is the cluster-shared config. /etc/pve is readable (not writable)
// without quorum, which is enough for us: we only ever read it.
const DefaultPath = "/etc/pve/pvectl/config.yaml"

type Config struct {
	Cluster Cluster         `yaml:"cluster"`
	Nodes   map[string]Node `yaml:"nodes"`
}

type Cluster struct {
	// Endpoints is the full candidate list of API hosts. The node being acted
	// on is removed at runtime -- see pve.Client.
	Endpoints []string `yaml:"endpoints"`
	// SecretsDir is node-local (0600 root): token, kubeconfig, talosconfig.
	SecretsDir string   `yaml:"secrets_dir"`
	CephFlags  []string `yaml:"ceph_flags"`
	// CephIgnoreHealth lists health check codes (globs) that do not bear on
	// whether a node can be taken down or has come back: they are reported,
	// but neither preflight nor verify waits on them. Unset means
	// DefaultCephIgnoreHealth; an explicit empty list ignores nothing.
	CephIgnoreHealth []string `yaml:"ceph_ignore_health"`
	Alertmanager     Service  `yaml:"alertmanager"`
	DiscordBridge    Service  `yaml:"discord_bridge"`
	Rolling          Rolling  `yaml:"rolling"`
}

// DefaultCephIgnoreHealth covers the cephx cipher checks Ceph 20.2.4 raises
// (AUTH_INSECURE_SERVICE_TICKETS and friends, some at ERR). They say keys use
// the older "aes" cipher -- a hardening item, not an availability one -- and
// they cannot be cleared while any client lacks aes256k (the Talos kernel
// before Linux 7.0). Found blocking verify in the 2026-09-28 rolling upgrade.
var DefaultCephIgnoreHealth = []string{"AUTH_INSECURE_*"}

type Service struct {
	Namespace string `yaml:"ns"`
	Service   string `yaml:"svc"`
	Port      int    `yaml:"port"`
}

type Rolling struct {
	SettleTimeout Duration `yaml:"settle_timeout"`
	// Order fixes the rolling reboot sequence. Empty means sorted node names.
	Order []string `yaml:"order"`
}

type Node struct {
	LANIP  string   `yaml:"lan_ip"`
	WOLMAC []string `yaml:"wol_mac"`
	Talos  Talos    `yaml:"talos"`
	// StopGuests are guests that cannot be migrated (passthrough hardware) and
	// must be shut down by hand before the host goes down.
	StopGuests []Guest `yaml:"stop_guests"`
	// StartAfterBoot are guests without onboot that pvectl must start again.
	StartAfterBoot []int `yaml:"start_after_boot"`
}

type Talos struct {
	Name string `yaml:"name"`
	IP   string `yaml:"ip"`
	// InternalIP is the node's address on the cluster network (10.10.x.3).
	// Prometheus labels most talos targets with it, so silences need it too.
	InternalIP string `yaml:"internal_ip,omitempty"`
	// VMID is the Proxmox guest that hosts this Talos node.
	VMID int `yaml:"vmid,omitempty"`
}

type Guest struct {
	ID   int    `yaml:"id"`
	Warn string `yaml:"warn,omitempty"`
	// ProvidesNetwork marks a guest that carries the operator's own path to the
	// cluster (the Wi-Fi AP VM). Stopping it cuts the session that is running
	// pvectl, so preflight has to know which guest that is.
	ProvidesNetwork bool `yaml:"provides_network,omitempty"`
	// Type is "qemu" or "lxc"; discover fills it in, defaults to qemu.
	Type string `yaml:"type,omitempty"`
}

func (g Guest) Kind() string {
	if g.Type == "lxc" {
		return "lxc"
	}
	return "qemu"
}

// Duration lets the YAML carry "15m" instead of a nanosecond count.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		var n int64
		if err2 := value.Decode(&n); err2 == nil {
			*d = Duration(time.Duration(n) * time.Second)
			return nil
		}
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }
func (d Duration) D() time.Duration          { return time.Duration(d) }

func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	dec := yaml.Unmarshal
	if err := dec(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	c.applyDefaults()
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Cluster.SecretsDir == "" {
		c.Cluster.SecretsDir = "/etc/pvectl"
	}
	if len(c.Cluster.CephFlags) == 0 {
		c.Cluster.CephFlags = []string{"noout", "norebalance"}
	}
	if c.Cluster.CephIgnoreHealth == nil {
		c.Cluster.CephIgnoreHealth = DefaultCephIgnoreHealth
	}
	if c.Cluster.Rolling.SettleTimeout == 0 {
		c.Cluster.Rolling.SettleTimeout = Duration(15 * time.Minute)
	}
	if c.Cluster.Alertmanager.Port == 0 {
		c.Cluster.Alertmanager.Port = 9093
	}
	if c.Cluster.DiscordBridge.Port == 0 {
		c.Cluster.DiscordBridge.Port = 9095
	}
}

// Validate reports every problem it finds rather than the first, so a bad
// config can be fixed in one pass.
func (c *Config) Validate() []error {
	var errs []error
	if len(c.Nodes) == 0 {
		errs = append(errs, fmt.Errorf("cluster.nodes is empty"))
	}
	if len(c.Cluster.Endpoints) == 0 {
		errs = append(errs, fmt.Errorf("cluster.endpoints is empty"))
	}
	for _, p := range c.Cluster.CephIgnoreHealth {
		if _, err := path.Match(p, ""); err != nil {
			errs = append(errs, fmt.Errorf("cluster.ceph_ignore_health: %q is not a valid pattern: %v", p, err))
		}
	}
	for _, e := range c.Cluster.Endpoints {
		if net.ParseIP(e) == nil {
			// A hostname here would be resolved through the tailnet and the
			// connection hijacked by Tailscale; insist on literal LAN IPs.
			errs = append(errs, fmt.Errorf("cluster.endpoints: %q is not a literal IP (hostnames resolve via tailnet)", e))
		}
	}
	for name, n := range c.Nodes {
		if net.ParseIP(n.LANIP) == nil {
			errs = append(errs, fmt.Errorf("nodes.%s.lan_ip %q is not a literal IP", name, n.LANIP))
		}
		if len(n.WOLMAC) == 0 {
			errs = append(errs, fmt.Errorf("nodes.%s.wol_mac is empty (wake will be impossible)", name))
		}
		for _, m := range n.WOLMAC {
			if _, err := net.ParseMAC(m); err != nil {
				errs = append(errs, fmt.Errorf("nodes.%s.wol_mac %q: %v", name, m, err))
			}
		}
		if len(n.WOLMAC) == 1 {
			errs = append(errs, fmt.Errorf("nodes.%s.wol_mac has one MAC; with balance-alb both bond slaves must be woken", name))
		}
		if n.Talos.Name == "" || net.ParseIP(n.Talos.IP) == nil {
			errs = append(errs, fmt.Errorf("nodes.%s.talos needs name and a literal ip", name))
		}
		for _, g := range n.StopGuests {
			if g.ID <= 0 {
				errs = append(errs, fmt.Errorf("nodes.%s.stop_guests has an entry without id", name))
			}
		}
		found := false
		for _, e := range c.Cluster.Endpoints {
			if e == n.LANIP {
				found = true
			}
		}
		if !found {
			errs = append(errs, fmt.Errorf("nodes.%s.lan_ip %s is not in cluster.endpoints", name, n.LANIP))
		}
	}
	return errs
}

func (c *Config) Node(name string) (Node, error) {
	n, ok := c.Nodes[name]
	if !ok {
		return Node{}, fmt.Errorf("node %q is not defined in the config; known nodes: %v", name, c.NodeNames())
	}
	return n, nil
}

func (c *Config) NodeNames() []string {
	out := make([]string, 0, len(c.Nodes))
	for k := range c.Nodes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RollingOrder is the sequence for cluster-wide operations.
func (c *Config) RollingOrder() []string {
	if len(c.Cluster.Rolling.Order) > 0 {
		return c.Cluster.Rolling.Order
	}
	return c.NodeNames()
}

// NodeByIP maps an endpoint back to a node name.
func (c *Config) NodeByIP(ip string) string {
	for name, n := range c.Nodes {
		if n.LANIP == ip {
			return name
		}
	}
	return ""
}

// SecretPath resolves a file inside the node-local secrets dir.
func (c *Config) SecretPath(name string) string {
	return filepath.Join(c.Cluster.SecretsDir, name)
}

// Marshal renders the config back to YAML. It carries no header: `config show`
// prints an existing hand-maintained file and must not label it as generated.
func (c *Config) Marshal() ([]byte, error) {
	return yaml.Marshal(c)
}

// GeneratedHeader prefixes the output of `config discover`, which really is
// generated and really will replace whatever was there.
const GeneratedHeader = "# Generated by `pvectl config discover`. Non-secret topology only:\n" +
	"# /etc/pve is root:www-data 0640 and readable by pveproxy, so credentials\n" +
	"# belong in cluster.secrets_dir (node-local, 0600 root) instead.\n"
