package config

import (
	"os"
	"testing"
	"time"
)

const sample = `
cluster:
  endpoints: [192.0.2.79, 192.0.2.80]
  rolling: {settle_timeout: 15m}
nodes:
  pve-1:
    lan_ip: 192.0.2.79
    wol_mac: [02:00:00:00:01:fa, 02:00:00:00:01:fb]
    talos: {name: talos-cp-1, ip: 192.0.2.82}
  pve-2:
    lan_ip: 192.0.2.80
    wol_mac: [02:00:00:00:02:92, 02:00:00:00:02:93]
    talos: {name: talos-cp-2, ip: 192.0.2.83}
`

func load(t *testing.T, body string) *Config {
	t.Helper()
	p := t.TempDir() + "/config.yaml"
	if err := writeFile(p, body); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLoadAppliesDefaults(t *testing.T) {
	c := load(t, sample)
	if c.Cluster.SecretsDir != "/etc/pvectl" {
		t.Fatalf("secrets_dir defaulted to %q", c.Cluster.SecretsDir)
	}
	if got := c.Cluster.Rolling.SettleTimeout.D(); got != 15*time.Minute {
		t.Fatalf("settle_timeout is %s, want 15m", got)
	}
	if len(c.Cluster.CephFlags) != 2 {
		t.Fatalf("ceph_flags defaulted to %v", c.Cluster.CephFlags)
	}
	if errs := c.Validate(); len(errs) != 0 {
		t.Fatalf("valid config reported %v", errs)
	}
}

// A hostname in endpoints must be rejected: it resolves through the tailnet and
// the connection gets taken over by Tailscale SSH.
func TestValidateRejectsHostnameEndpoint(t *testing.T) {
	c := load(t, sample)
	c.Cluster.Endpoints = []string{"pve-1", "192.0.2.80"}
	errs := c.Validate()
	if len(errs) == 0 {
		t.Fatal("expected an error for a hostname endpoint")
	}
}

// balance-alb means both bond slaves must be woken, so one MAC is a config bug.
func TestValidateRejectsSingleMAC(t *testing.T) {
	c := load(t, sample)
	n := c.Nodes["pve-1"]
	n.WOLMAC = n.WOLMAC[:1]
	c.Nodes["pve-1"] = n
	found := false
	for _, e := range c.Validate() {
		if contains(e.Error(), "both bond slaves") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the single-MAC warning, got %v", c.Validate())
	}
}

func TestRollingOrderDefaultsToSortedNames(t *testing.T) {
	c := load(t, sample)
	got := c.RollingOrder()
	if len(got) != 2 || got[0] != "pve-1" || got[1] != "pve-2" {
		t.Fatalf("order is %v", got)
	}
	c.Cluster.Rolling.Order = []string{"pve-2", "pve-1"}
	if got := c.RollingOrder(); got[0] != "pve-2" {
		t.Fatalf("explicit order ignored: %v", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
