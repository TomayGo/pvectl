package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/TomayGo/pvectl/internal/alert"
	"github.com/TomayGo/pvectl/internal/config"
	"github.com/TomayGo/pvectl/internal/k8s"
	plog "github.com/TomayGo/pvectl/internal/log"
	"github.com/TomayGo/pvectl/internal/plan"
	"github.com/TomayGo/pvectl/internal/pve/pvetest"
	"github.com/TomayGo/pvectl/internal/remote/remotetest"
	"github.com/TomayGo/pvectl/internal/state"
)

// The fixture cluster. The node names are deliberately not pve-N: a rolling
// run compares them with the real hostname, and these tests may run on a
// cluster node.
const fixtureYAML = `
cluster:
  endpoints: [192.0.2.11, 192.0.2.12, 192.0.2.13]
  alertmanager: {ns: monitoring, svc: alertmanager}
  discord_bridge: {ns: monitoring, svc: discord-bridge}
  rolling: {settle_timeout: 20m, order: [node-c, node-b, node-a]}
nodes:
  node-a:
    lan_ip: 192.0.2.11
    wol_mac: [02:00:00:00:01:01, 02:00:00:00:01:02]
    talos: {name: talos-a, ip: 192.0.2.21, vmid: 101}
  node-b:
    lan_ip: 192.0.2.12
    wol_mac: [02:00:00:00:02:01, 02:00:00:00:02:02]
    talos: {name: talos-b, ip: 192.0.2.22, vmid: 102}
  node-c:
    lan_ip: 192.0.2.13
    wol_mac: [02:00:00:00:03:01, 02:00:00:00:03:02]
    talos: {name: talos-c, ip: 192.0.2.23, vmid: 103}
`

func writeFixtureConfig(t *testing.T) string { return writeConfig(t, fixtureYAML) }

func writeConfig(t *testing.T, yaml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o640); err != nil {
		t.Fatal(err)
	}
	return p
}

func fixtureConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load(writeFixtureConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// fakeProxy is Alertmanager and the Discord bridge as seen through the
// Kubernetes service proxy.
type fakeProxy struct {
	mu       sync.Mutex
	silences []alert.Silence
	calls    []string
}

func (p *fakeProxy) ProxyRequest(ctx context.Context, method, namespace, service string, port int, path string, body []byte) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, fmt.Sprintf("%s %s/%s:%d%s", method, namespace, service, port, path))
	switch {
	case method == "GET" && path == "/api/v2/silences":
		return json.Marshal(p.silences)
	case method == "DELETE" && strings.HasPrefix(path, "/api/v2/silence/"):
		id := strings.TrimPrefix(path, "/api/v2/silence/")
		for i := range p.silences {
			if p.silences[i].ID == id {
				p.silences[i].Status.State = "expired"
			}
		}
		return nil, nil
	}
	return []byte(`{}`), nil
}

func (p *fakeProxy) addSilence(id, createdBy string) {
	s := alert.Silence{ID: id, CreatedBy: createdBy, Matchers: []alert.Matcher{{Name: "node", Value: "node-a|talos-a", IsRegex: true}}}
	s.Status.State = "active"
	p.silences = append(p.silences, s)
}

func (p *fakeProxy) writes() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, c := range p.calls {
		if !strings.HasPrefix(c, "GET ") {
			out = append(out, c)
		}
	}
	return out
}

// world is a fake cluster: PVE, Kubernetes, Alertmanager, and the commands
// that would be run locally or over ssh.
type world struct {
	t     *testing.T
	pve   *pvetest.Fake
	cmd   *remotetest.Fake
	kube  *fake.Clientset
	proxy *fakeProxy
	// out is what a hand-built Env prints. An Env built through newEnv writes
	// its transcript to logDir instead.
	out *bytes.Buffer
}

func k8sNode(name string, cordoned bool) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: cordoned},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady"},
		}},
	}
}

// newWorld builds the fake cluster; cordoned names the Kubernetes nodes that
// start out unschedulable.
func newWorld(t *testing.T, cordoned ...string) *world {
	t.Helper()
	w := &world{t: t, pve: pvetest.New(t), cmd: &remotetest.Fake{}, proxy: &fakeProxy{}, out: &bytes.Buffer{}}
	w.pve.Runner = w.cmd
	w.pve.JSON("GET", "/version", `{"version":"9.2.4"}`)
	var objs []runtime.Object
	for _, n := range []string{"talos-a", "talos-b", "talos-c"} {
		c := false
		for _, x := range cordoned {
			c = c || x == n
		}
		objs = append(objs, k8sNode(n, c))
	}
	w.kube = fake.NewSimpleClientset(objs...)
	return w
}

// env wires an Env to the fake cluster the way plan.NewEnv wires one to the
// real one.
func (w *world) env(cfg *config.Config, target string, lg *plog.Logger, opts plan.Options, stateDir string) *plan.Env {
	w.t.Helper()
	all := w.pve.ClientWith(lg, cfg.Cluster.Endpoints...)
	all.DryRun = opts.DryRun
	e := &plan.Env{Cfg: cfg, Target: target, Self: "node-b", Log: lg, Opts: opts, PVEAll: all, PVE: all, StateDir: stateDir}
	if target != "" {
		e.Node = cfg.Nodes[target]
		e.PVE = all.Excluded(e.Node.LANIP)
	}
	e.K8s = k8s.NewForClientset(w.kube, lg, opts.DryRun)
	e.Alert = alert.New(w.proxy,
		alert.Endpoint{Namespace: cfg.Cluster.Alertmanager.Namespace, Service: cfg.Cluster.Alertmanager.Service, Port: cfg.Cluster.Alertmanager.Port},
		alert.Endpoint{Namespace: cfg.Cluster.DiscordBridge.Namespace, Service: cfg.Cluster.DiscordBridge.Service, Port: cfg.Cluster.DiscordBridge.Port},
		lg, opts.DryRun)
	return e
}

// handEnv is an Env for calling a function directly: it prints into w.out and
// keeps its state in a fresh temporary directory.
func (w *world) handEnv(target string) *plan.Env {
	w.t.Helper()
	return w.env(fixtureConfig(w.t), target, plog.To(w.out), plan.DefaultOptions(), w.t.TempDir())
}

func (w *world) lines() []string {
	s := strings.TrimRight(w.out.String(), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func wantLines(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%d line(s), want %d:\n got:\n%s\nwant:\n%s", len(got), len(want), strings.Join(got, "\n"), strings.Join(want, "\n"))
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got %q\nwant %q", i+1, got[i], want[i])
		}
	}
}

func (w *world) cordoned() []string {
	list, err := w.kube.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		w.t.Fatal(err)
	}
	var out []string
	for _, n := range list.Items {
		if n.Spec.Unschedulable {
			out = append(out, n.Name)
		}
	}
	sort.Strings(out)
	return out
}

func (w *world) commands() []string {
	var out []string
	for _, c := range w.cmd.Calls() {
		out = append(out, c.Line())
	}
	return out
}

// healthy registers the replies of a cluster with nothing wrong.
func (w *world) healthy() {
	w.pve.JSON("GET", "/cluster/status", `[{"type":"cluster","name":"lab","nodes":3,"quorate":1},`+
		`{"type":"node","name":"node-a","online":1},{"type":"node","name":"node-b","online":1},{"type":"node","name":"node-c","online":1}]`)
	w.pve.JSON("GET", "/cluster/ceph/status", `{"health":{"status":"HEALTH_OK"},`+
		`"pgmap":{"num_pgs":97,"pgs_by_state":[{"state_name":"active+clean","count":97}]},`+
		`"osdmap":{"num_osds":3,"num_up_osds":3,"num_in_osds":3}}`)
	w.pve.JSON("GET", "/cluster/ceph/flags", `[{"name":"noout","value":0},{"name":"norebalance","value":0},{"name":"pause","value":0}]`)
	w.pve.JSON("GET", "/cluster/ha/status/manager_status",
		`{"manager_status":{"master_node":"node-b","node_status":{"node-a":"online","node-b":"online","node-c":"online"}}}`)
	w.pve.JSON("GET", "/cluster/ha/resources", `[]`)
}

// ---------- commands run through newRoot ----------

// cli is the process-wide state a command line touches, pointed at temporary
// directories and at a fake cluster for one test.
type cli struct {
	t        *testing.T
	w        *world
	config   string
	logDir   string
	stateDir string
	// built records the target and options of every Env a command asked for.
	built []builtEnv
	// refuse, when set, makes building an Env fail with it: for tests that
	// only care how the command line became options.
	refuse error
}

type builtEnv struct {
	target string
	opts   plan.Options
}

// newCLI replaces the globals for the duration of the test.
func newCLI(t *testing.T, w *world) *cli {
	t.Helper()
	c := &cli{t: t, w: w, config: writeFixtureConfig(t), logDir: t.TempDir(), stateDir: t.TempDir()}
	savedG, savedBuild, savedSSH := g, buildEnv, sshRunner
	t.Cleanup(func() { g, buildEnv, sshRunner = savedG, savedBuild, savedSSH })
	g = globals{opts: plan.DefaultOptions(), stateDir: c.stateDir}
	sshRunner = w.cmd
	buildEnv = func(cfg *config.Config, target string, lg *plog.Logger, opts plan.Options) (*plan.Env, error) {
		c.built = append(c.built, builtEnv{target, opts})
		if c.refuse != nil {
			return nil, c.refuse
		}
		return w.env(cfg, target, lg, opts, c.stateDir), nil
	}
	return c
}

// run executes `pvectl <args>` in-process against the fake cluster.
func (c *cli) run(args ...string) error {
	c.t.Helper()
	root := newRoot()
	root.SetArgs(append([]string{"--config", c.config, "--log-dir", c.logDir}, args...))
	return root.ExecuteContext(context.Background())
}

var transcriptPrefix = regexp.MustCompile(`^\S+ (DEBUG|INFO |WARN |ERROR) `)

// transcript returns what the commands logged, without timestamps and without
// debug lines: "| text" for a plain line, "[step] text" inside a step.
func (c *cli) transcript() []string {
	c.t.Helper()
	files, err := filepath.Glob(filepath.Join(c.logDir, "*.log"))
	if err != nil {
		c.t.Fatal(err)
	}
	sort.Strings(files)
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			c.t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			m := transcriptPrefix.FindStringSubmatch(line)
			if m == nil || m[1] == "DEBUG" || strings.Contains(line, " INFO  API ") {
				continue
			}
			text := strings.TrimPrefix(line, m[0])
			if m[1] != "INFO " {
				text = strings.TrimSpace(m[1]) + ": " + text
			}
			out = append(out, text)
		}
	}
	return out
}

func exists(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

func saveState(t *testing.T, st *state.State) {
	t.Helper()
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
}

func regexpMust(pattern string) *regexp.Regexp { return regexp.MustCompile(pattern) }

// failing answers every command with the given error.
func failing(msg string) func(remotetest.Call) remotetest.Result {
	return func(remotetest.Call) remotetest.Result { return remotetest.Result{Err: errors.New(msg)} }
}
