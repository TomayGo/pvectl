package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/TomayGo/pvectl/internal/pve/pvetest"
	"github.com/TomayGo/pvectl/internal/state"
)

// `shutdown cluster` and `start cluster` run whole, in-process, against the
// fake cluster. Every fake node answers, so `start cluster` finds them all up
// and sends no Wake-on-LAN.

// withGuests gives node-a a passthrough VM and a container to stop, and two
// guests without onboot to start again.
func withGuests(t *testing.T, c *cli) {
	t.Helper()
	c.config = writeConfig(t, strings.Replace(fixtureYAML,
		"    talos: {name: talos-a, ip: 192.0.2.21, vmid: 101}\n",
		"    talos: {name: talos-a, ip: 192.0.2.21, vmid: 101}\n"+
			"    stop_guests: [{id: 110, warn: the Wi-Fi drops}, {id: 111, type: lxc}]\n"+
			"    start_after_boot: [111, 112]\n", 1))
}

func okTask(w *world, node, upid string) {
	w.pve.JSON("GET", taskPath(node, upid), `{"status":"stopped","exitstatus":"OK"}`)
}

func TestShutdownClusterStopsEverythingInOrder(t *testing.T) {
	c := newCLI(t, newWorld(t))
	withGuests(t, c)
	w := c.w
	w.healthy()
	for _, f := range freezeFlags {
		w.pve.Static("PUT", "/cluster/ceph/flags/"+f, nil)
	}
	for node, vmid := range map[string]string{"node-a": "101", "node-b": "102", "node-c": "103"} {
		w.pve.JSON("GET", "/nodes/"+node+"/qemu/"+vmid+"/status/current", `{"status":"running"}`)
		w.pve.Static("POST", "/nodes/"+node+"/qemu/"+vmid+"/status/shutdown", "UPID:"+vmid)
		okTask(w, node, "UPID:"+vmid)
		w.pve.Static("POST", "/nodes/"+node+"/status", nil)
	}
	w.pve.JSON("GET", "/nodes/node-a/qemu/110/status/current", `{"status":"running"}`)
	w.pve.Static("POST", "/nodes/node-a/qemu/110/status/shutdown", "UPID:110")
	okTask(w, "node-a", "UPID:110")
	w.pve.JSON("GET", "/nodes/node-a/lxc/111/status/current", `{"status":"stopped"}`)

	if err := c.run("shutdown", "cluster", "--yes", "--skip-check=etcd"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PUT /cluster/ceph/flags/noout value=1",
		"PUT /cluster/ceph/flags/norecover value=1",
		"PUT /cluster/ceph/flags/norebalance value=1",
		"PUT /cluster/ceph/flags/nobackfill value=1",
		"PUT /cluster/ceph/flags/nodown value=1",
		"PUT /cluster/ceph/flags/pause value=1",
		// every Talos VM is asked before any is waited on
		"POST /nodes/node-a/qemu/101/status/shutdown timeout=180",
		"POST /nodes/node-b/qemu/102/status/shutdown timeout=180",
		"POST /nodes/node-c/qemu/103/status/shutdown timeout=180",
		"POST /nodes/node-a/qemu/110/status/shutdown timeout=180",
		// peers first, the node running pvectl (node-b here) last
		"POST /nodes/node-a/status command=shutdown",
		"POST /nodes/node-c/status command=shutdown",
		"POST /nodes/node-b/status command=shutdown",
	}
	if got := w.pve.Writes(); !reflect.DeepEqual(got, want) {
		t.Errorf("API writes:\n got %q\nwant %q", got, want)
	}
	if got, want := w.proxy.writes(), []string{
		"POST monitoring/alertmanager:9093/api/v2/silences",
		"POST monitoring/discord-bridge:9095/",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("alertmanager / bridge writes %q, want %q", got, want)
	}
	// The lock is left held for `start cluster`.
	held, err := state.Peek(c.stateDir)
	if err != nil || held == nil || held.Target != "cluster" || held.Command != "shutdown-cluster" {
		t.Errorf("lock: %+v, %v", held, err)
	}
	got := c.transcript()
	for _, want := range []string{
		"| pvectl " + version + " -- shutdown cluster (from node-b)",
		"|   [skip] etcd           skipped by --skip-check",
		"| This stops the entire cluster: every VM, every container, all storage.",
		"|   - node-a VM 110: the Wi-Fi drops",
		"=== step ceph-freeze: set noout norecover norebalance nobackfill nodown pause",
		"[ceph-freeze] ceph flag pause set",
		"=== step talos: shut down every Talos control-plane VM together",
		"[talos] guest 101 on node-a stopped",
		"[talos] guest 103 on node-c stopped",
		"=== step guests: shut down passthrough guests on every node",
		"[guests] guest 110 on node-a stopped",
		"[guests] guest 111 on node-a is already stopped",
		"=== step poweroff: power off the hosts, this node last",
		"[poweroff] powering off node-a",
		"[poweroff] powering off node-b",
		"| All nodes are going down. Bring them back with `pvectl start cluster` from any node once one is up.",
	} {
		if !containsLine(got, want) {
			t.Errorf("transcript is missing %q", want)
		}
	}
}

// A failing preflight stops `shutdown cluster` before it takes the lock or
// changes anything.
func TestShutdownClusterStopsAtAFailedPreflight(t *testing.T) {
	c := newCLI(t, newWorld(t))
	c.w.healthy()
	c.w.pve.JSON("GET", "/cluster/ceph/flags", `[{"name":"noout","value":1}]`)
	err := c.run("shutdown", "cluster", "--yes", "--skip-check=etcd")
	if err == nil || err.Error() != "preflight failed: ceph-flags" {
		t.Errorf("error %v", err)
	}
	if len(c.w.pve.Writes()) != 0 || exists(t, c.stateDir, "maintenance.lock") {
		t.Error("a refused shutdown changed the cluster or took the lock")
	}
}

func TestStartClusterThawsAndStartsGuests(t *testing.T) {
	c := newCLI(t, newWorld(t))
	withGuests(t, c)
	w := c.w
	w.healthy()
	// The window `shutdown cluster` left open.
	if _, err := state.Acquire(c.stateDir, "node-a", "cluster", "shutdown-cluster"); err != nil {
		t.Fatal(err)
	}
	for _, f := range freezeFlags {
		w.pve.Static("PUT", "/cluster/ceph/flags/"+f, nil)
	}
	w.proxy.addSilence("s-cluster", "pvectl")
	// 111 is a container in stop_guests; 112 is in no list and starts as a VM.
	w.pve.Static("POST", "/nodes/node-a/lxc/111/status/start", "UPID:111")
	okTask(w, "node-a", "UPID:111")
	w.pve.Handle("POST", "/nodes/node-a/qemu/112/status/start", func(pvetest.Request) (any, error) {
		return nil, &pvetest.Error{Status: 500, Message: "no such VM"}
	})

	if err := c.run("start", "cluster", "--yes"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PUT /cluster/ceph/flags/noout value=0",
		"PUT /cluster/ceph/flags/norecover value=0",
		"PUT /cluster/ceph/flags/norebalance value=0",
		"PUT /cluster/ceph/flags/nobackfill value=0",
		"PUT /cluster/ceph/flags/nodown value=0",
		"PUT /cluster/ceph/flags/pause value=0", // last
		"POST /nodes/node-a/lxc/111/status/start",
		"POST /nodes/node-a/qemu/112/status/start",
	}
	if got := w.pve.Writes(); !reflect.DeepEqual(got, want) {
		t.Errorf("API writes:\n got %q\nwant %q", got, want)
	}
	if got, want := w.proxy.writes(), []string{
		"DELETE monitoring/alertmanager:9093/api/v2/silence/s-cluster",
		"POST monitoring/discord-bridge:9095/",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("alertmanager / bridge writes %q, want %q", got, want)
	}
	if exists(t, c.stateDir, "maintenance.lock") {
		t.Error("the cluster lock is still held")
	}
	got := c.transcript()
	for _, want := range []string{
		"| pvectl " + version + " -- start cluster (from node-b)",
		"=== step wake: send Wake-on-LAN to every node that is down",
		"[wake] node-a is already up",
		"[wake] node-c is already up",
		"=== step wait-up: wait for every node to rejoin",
		"[wait-up] all 3 nodes online and quorate",
		"=== step ceph-thaw: clear the freeze flags, pause last",
		"[ceph-thaw] waiting for OSDs to come back before clearing pause",
		"[ceph-thaw] 3/3 OSDs up and in",
		"[ceph-thaw] ceph flag pause cleared",
		"=== step start-guests: start guests that have no onboot flag",
		"[start-guests] guest 111 started on node-a",
		"WARN: [start-guests] start guest 112 on node-a: POST /nodes/node-a/qemu/112/status/start: HTTP 500: no such VM",
		"=== step verify: wait for full health",
		"[verify] all kubernetes nodes Ready: ready",
		"| Cluster is back in service.",
	} {
		if !containsLine(got, want) {
			t.Errorf("transcript is missing %q:\n%s", want, strings.Join(got, "\n"))
		}
	}
	if !strings.HasPrefix(got[1], "| adopting the cluster maintenance lock held since ") {
		t.Errorf("second line %q", got[1])
	}
}

// `start cluster` must not run over a window that is open for a single node.
func TestStartClusterRefusesANodeWindow(t *testing.T) {
	c := newCLI(t, newWorld(t))
	c.w.healthy()
	if _, err := state.Acquire(c.stateDir, "node-b", "node-a", "shutdown"); err != nil {
		t.Fatal(err)
	}
	err := c.run("start", "cluster", "--yes")
	if err == nil || !strings.Contains(err.Error(), "node-a is under maintenance, refusing to also touch cluster") {
		t.Errorf("error %v", err)
	}
	if len(c.w.pve.Writes()) != 0 {
		t.Errorf("writes %v", c.w.pve.Writes())
	}
}

// Nodes that do not all come back stop `start cluster` before it thaws Ceph.
func TestStartClusterGivesUpWaitingForNodes(t *testing.T) {
	c := newCLI(t, newWorld(t))
	c.w.healthy()
	c.w.pve.JSON("GET", "/cluster/status", `[{"type":"cluster","name":"lab","nodes":3,"quorate":1},`+
		`{"type":"node","name":"node-a","online":1},{"type":"node","name":"node-b","online":1},{"type":"node","name":"node-c","online":0}]`)
	err := c.run("start", "cluster", "--yes", "--timeout-up=0s")
	if err == nil || err.Error() != "not every node came back within 0s" {
		t.Errorf("error %v", err)
	}
	if len(c.w.pve.Writes()) != 0 {
		t.Errorf("writes %v", c.w.pve.Writes())
	}
	// As the code stands, the lock taken at the start stays held.
	if !exists(t, c.stateDir, "maintenance.lock") {
		t.Error("the lock was released")
	}
}
