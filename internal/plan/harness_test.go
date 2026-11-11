package plan

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/TomayGo/pvectl/internal/config"
	plog "github.com/TomayGo/pvectl/internal/log"
	"github.com/TomayGo/pvectl/internal/pve/pvetest"
	"github.com/TomayGo/pvectl/internal/remote/remotetest"
	"github.com/TomayGo/pvectl/internal/state"
)

// The fixture cluster: three nodes on TEST-NET-1 addresses, pve-1 the target
// of the maintenance and pve-2 the node pvectl runs on.
const (
	ip1 = "192.0.2.11"
	ip2 = "192.0.2.12"
	ip3 = "192.0.2.13"
)

func fixtureConfig() *config.Config {
	c := &config.Config{Nodes: map[string]config.Node{}}
	c.Cluster.Endpoints = []string{ip1, ip2, ip3}
	c.Cluster.CephFlags = []string{"noout", "norebalance"}
	for i, ip := range []string{ip1, ip2, ip3} {
		n := string(rune('1' + i))
		c.Nodes["pve-"+n] = config.Node{
			LANIP: ip,
			Talos: config.Talos{Name: "talos-cp-" + n, IP: "192.0.2.2" + n, VMID: 100 + i + 1},
		}
	}
	return c
}

// testEnv is an Env wired to fakes: no cluster, no ssh, and state in a
// temporary directory.
type testEnv struct {
	*Env
	pve *pvetest.Fake
	cmd *remotetest.Fake
	out *bytes.Buffer
}

// newTestEnv builds the Env a `command` run against pve-1 would have, with
// state.json already created for it (in memory; nothing is saved yet).
func newTestEnv(t *testing.T, command string) *testEnv {
	t.Helper()
	te := &testEnv{pve: pvetest.New(t), cmd: &remotetest.Fake{}, out: &bytes.Buffer{}}
	te.pve.Runner = te.cmd
	te.pve.JSON("GET", "/version", `{"version":"9.2.4"}`)
	cfg := fixtureConfig()
	lg := plog.To(te.out)
	all := te.pve.ClientWith(lg, cfg.Cluster.Endpoints...)
	te.Env = &Env{
		Cfg:      cfg,
		Target:   "pve-1",
		Node:     cfg.Nodes["pve-1"],
		Self:     "pve-2",
		Log:      lg,
		Opts:     DefaultOptions(),
		PVEAll:   all,
		PVE:      all.Excluded(ip1),
		StateDir: t.TempDir(),
	}
	steps, err := StepsFor(command, te.Env)
	if err != nil {
		t.Fatal(err)
	}
	r := &Runner{Env: te.Env, Steps: steps, Name: command}
	te.State = state.NewState(te.StateDir, command, te.Target, te.Self, r.StepNames())
	return te
}

// dryRun switches the Env and its clients to --dry-run, as NewEnv does.
func (te *testEnv) dryRun() {
	te.Opts.DryRun = true
	te.PVE.DryRun = true
	te.PVEAll.DryRun = true
	te.State.SetDryRun(true)
}

// lines returns what the run printed, one entry per line.
func (te *testEnv) lines() []string {
	s := strings.TrimRight(te.out.String(), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func (te *testEnv) wantLines(t *testing.T, want ...string) {
	t.Helper()
	got := te.lines()
	if len(got) != len(want) {
		t.Errorf("printed %d line(s), want %d:\n got:\n%s\nwant:\n%s", len(got), len(want),
			strings.Join(got, "\n"), strings.Join(want, "\n"))
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got %q\nwant %q", i+1, got[i], want[i])
		}
	}
}

func (te *testEnv) wantWrites(t *testing.T, want ...string) {
	t.Helper()
	got := te.pve.Writes()
	if len(got) != len(want) {
		t.Errorf("API writes:\n got %q\nwant %q", got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("API write %d: got %q, want %q", i+1, got[i], want[i])
		}
	}
}

// commands returns the local commands run (ha-manager), one string each.
func (te *testEnv) commands() []string {
	var out []string
	for _, c := range te.cmd.Calls() {
		out = append(out, c.Line())
	}
	return out
}

// reload saves the state and reads it back, which is what a later `resume`,
// `abort` or `start` sees: every number a float64, every list a []any.
func (te *testEnv) reload(t *testing.T) {
	t.Helper()
	if err := te.State.Save(); err != nil {
		t.Fatal(err)
	}
	st, err := state.LoadState(te.StateDir)
	if err != nil || st == nil {
		t.Fatalf("reload state: %v", err)
	}
	te.State = st
}

// haStatus renders GET /cluster/ha/status/manager_status. requested lists the
// nodes with a maintenance request, nodes maps node to CRM state, and each
// service is "sid node state [target]".
func haStatus(requested []string, nodes map[string]string, services ...string) string {
	type svc struct {
		Node   string `json:"node"`
		State  string `json:"state"`
		Target string `json:"target,omitempty"`
	}
	req := map[string]map[string]int{}
	for _, n := range requested {
		req[n] = map[string]int{"maintenance": 1}
	}
	ss := map[string]svc{}
	for _, s := range services {
		f := strings.Fields(s)
		v := svc{Node: f[1], State: f[2]}
		if len(f) > 3 {
			v.Target = f[3]
		}
		ss[f[0]] = v
	}
	b, err := json.Marshal(map[string]any{"manager_status": map[string]any{
		"master_node": "pve-2", "node_request": req, "node_status": nodes, "service_status": ss,
	}})
	if err != nil {
		panic(err)
	}
	return string(b)
}

var allOnline = map[string]string{"pve-1": "online", "pve-2": "online", "pve-3": "online"}

func inMaintenance(node string) map[string]string {
	m := map[string]string{}
	for k, v := range allOnline {
		m[k] = v
	}
	m[node] = "maintenance"
	return m
}

// elapsed reports fake-clock time since start, for tests inside a synctest
// bubble.
func elapsed(start time.Time) time.Duration { return time.Since(start) }

// rawJSON marks a string as the JSON of a reply's data field.
func rawJSON(s string) json.RawMessage { return json.RawMessage(s) }
