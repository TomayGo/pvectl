package main

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/TomayGo/pvectl/internal/plan"
	"github.com/TomayGo/pvectl/internal/pve/pvetest"
	"github.com/TomayGo/pvectl/internal/state"
)

// runDoctor as a whole, against a fake cluster: every line it prints and
// every change --fix makes.

func TestDoctorOnACleanCluster(t *testing.T) {
	w := newWorld(t)
	w.healthy()
	e := w.handEnv("")
	if err := runDoctor(context.Background(), e, true); err != nil {
		t.Fatal(err)
	}
	wantLines(t, w.lines(),
		"pvectl doctor (from node-b) --fix",
		"",
		"[ok]   ceph flags   : none set",
		"[ok]   kubernetes   : no node is cordoned",
		"[ok]   silences     : none",
		"[ok]   ha maint.    : no node in HA maintenance",
		"",
		"Nothing to clean up.",
	)
	if len(w.pve.Writes()) != 0 || len(w.commands()) != 0 || len(w.proxy.writes()) != 0 {
		t.Errorf("a clean cluster was changed: %v %v %v", w.pve.Writes(), w.commands(), w.proxy.writes())
	}
}

// leftovers is a cluster after a maintenance of node-a died: noout set (and
// pause, by someone else), talos-a cordoned, a pvectl silence, node-a in HA
// maintenance, node-c offline, a lock with no run behind it and a finished
// run still recorded.
func leftovers(t *testing.T) (*world, *plan.Env) {
	t.Helper()
	w := newWorld(t, "talos-a")
	e := w.handEnv("")
	w.pve.JSON("GET", "/cluster/ceph/flags", `[{"name":"noout","value":1},{"name":"norebalance","value":0},{"name":"pause","value":1}]`)
	w.pve.Static("PUT", "/cluster/ceph/flags/noout", nil)
	w.pve.JSON("GET", "/cluster/ha/status/manager_status",
		`{"manager_status":{"node_request":{"node-a":{"maintenance":1}},"node_status":{"node-a":"maintenance","node-b":"online"}}}`)
	w.pve.JSON("GET", "/cluster/status", `[{"type":"cluster","name":"lab","nodes":3,"quorate":1},`+
		`{"type":"node","name":"node-a","online":1},{"type":"node","name":"node-b","online":1},{"type":"node","name":"node-c","online":0}]`)
	w.proxy.addSilence("s-ours", "pvectl")
	w.proxy.addSilence("s-theirs", "alice")
	if _, err := state.Acquire(e.StateDir, "node-b", "node-c", "reboot"); err != nil {
		t.Fatal(err)
	}
	done := state.NewState(e.StateDir, plan.CmdReboot, "node-a", "node-b", []string{"lock"})
	done.Finished = true
	saveState(t, done)
	return w, e
}

func TestDoctorFixClearsLeftovers(t *testing.T) {
	w, e := leftovers(t)
	if err := runDoctor(context.Background(), e, true); err != nil {
		t.Fatal(err)
	}
	wantLines(t, w.lines(),
		"pvectl doctor (from node-b) --fix",
		"",
		"[warn] ceph flags   : noout still set",
		"       -> cleared noout",
		"[warn] ceph flags   : pause set, but not by a node maintenance (cluster.ceph_flags is noout norebalance); leaving it alone",
		"       `pvectl start cluster` lifts a cluster freeze; `ceph osd unset <flag>` clears one by hand",
		"[warn] kubernetes   : cordoned: talos-a",
		"  node talos-a marked schedulable",
		"       -> uncordoned talos-a",
		"[warn] silences     : 1 pvectl silence(s) still active",
		"  silence s-ours expired",
		"       -> expired s-ours",
		"[warn] ha maint.    : node-a in HA maintenance",
		"  running ha-manager crm-command node-maintenance disable node-a",
		"       -> maintenance end requested for node-a (HA moves its resources back)",
		"[warn] lock         : held by node-b for node-c with no matching run",
		"       -> released",
		"[warn] state.json   : a finished reboot node-a run is still recorded",
		"       -> removed",
		"[warn] cluster      : offline node(s): node-c -- start with `pvectl start node <name>`",
		"",
		"6 issue(s) addressed. Re-run `pvectl doctor` to confirm.",
	)
	if got, want := w.pve.Writes(), []string{"PUT /cluster/ceph/flags/noout value=0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("API writes %q, want %q", got, want)
	}
	if got, want := w.commands(), []string{"ha-manager crm-command node-maintenance disable node-a"}; !reflect.DeepEqual(got, want) {
		t.Errorf("commands %q, want %q", got, want)
	}
	if got, want := w.proxy.writes(), []string{"DELETE monitoring/alertmanager:9093/api/v2/silence/s-ours"}; !reflect.DeepEqual(got, want) {
		t.Errorf("alertmanager writes %q, want %q", got, want)
	}
	if got := w.cordoned(); len(got) != 0 {
		t.Errorf("still cordoned: %v", got)
	}
	if exists(t, e.StateDir, "maintenance.lock") || exists(t, e.StateDir, "state.json") {
		t.Error("the stale lock and the finished state.json must be gone")
	}
}

func TestDoctorWithoutFixOnlyReports(t *testing.T) {
	w, e := leftovers(t)
	if err := runDoctor(context.Background(), e, false); err != nil {
		t.Fatal(err)
	}
	wantLines(t, w.lines(),
		"pvectl doctor (from node-b)",
		"",
		"[warn] ceph flags   : noout still set",
		"[warn] ceph flags   : pause set, but not by a node maintenance (cluster.ceph_flags is noout norebalance); leaving it alone",
		"       `pvectl start cluster` lifts a cluster freeze; `ceph osd unset <flag>` clears one by hand",
		"[warn] kubernetes   : cordoned: talos-a",
		"[warn] silences     : 1 pvectl silence(s) still active",
		"[warn] ha maint.    : node-a in HA maintenance",
		"[warn] lock         : held by node-b for node-c with no matching run",
		"[warn] state.json   : a finished reboot node-a run is still recorded",
		"[warn] cluster      : offline node(s): node-c -- start with `pvectl start node <name>`",
		"",
		"6 issue(s) found. Re-run with --fix to clear them.",
	)
	if len(w.pve.Writes()) != 0 || len(w.commands()) != 0 || len(w.proxy.writes()) != 0 || len(w.cordoned()) != 1 {
		t.Errorf("a report-only run changed something: %v %v %v %v", w.pve.Writes(), w.commands(), w.proxy.writes(), w.cordoned())
	}
	if !exists(t, e.StateDir, "maintenance.lock") || !exists(t, e.StateDir, "state.json") {
		t.Error("a report-only run removed the lock or state.json")
	}
}

// While a run is open -- a lock with its run, or any unfinished run -- the
// flags, the cordon and the maintenance are that run's, and --fix is refused.
func TestDoctorTouchesNothingWhileARunIsOpen(t *testing.T) {
	w := newWorld(t, "talos-a")
	e := w.handEnv("")
	w.healthy()
	w.pve.JSON("GET", "/cluster/ceph/flags", `[{"name":"noout","value":1}]`)
	if _, err := state.Acquire(e.StateDir, "node-b", "node-a", "shutdown"); err != nil {
		t.Fatal(err)
	}
	open := state.NewState(e.StateDir, plan.CmdShutdown, "node-a", "node-b", []string{"lock", "host-shutdown"})
	open.Set("lock", state.StepDone, nil)
	open.Set("host-shutdown", state.StepDone, nil)
	open.Finished = true // shutdown finished; the window stays open until start
	saveState(t, open)

	if err := runDoctor(context.Background(), e, true); err != nil {
		t.Fatal(err)
	}
	l := w.lines()
	if len(l) < 6 || l[2] != "A maintenance is in progress; doctor will report but not touch anything." ||
		!strings.HasPrefix(l[3], "  lock: shutdown node-a (started by node-b, pid ") ||
		!strings.HasPrefix(l[4], "  state: shutdown node-a: 2 done, 0 failed, 0 pending (started ") ||
		l[5] != "  use `pvectl resume` or `pvectl abort` -- not doctor -- while a run is open." {
		t.Errorf("printed:\n%s", w.out)
	}
	wantLines(t, l[6:],
		"",
		"[warn] ceph flags   : noout still set",
		"[warn] kubernetes   : cordoned: talos-a",
		"[ok]   silences     : none",
		"[ok]   ha maint.    : no node in HA maintenance",
		"",
		"2 issue(s) found. Re-run with --fix to clear them.",
	)
	if len(w.pve.Writes()) != 0 || len(w.cordoned()) != 1 || !exists(t, e.StateDir, "maintenance.lock") || !exists(t, e.StateDir, "state.json") {
		t.Error("doctor --fix changed something while a run was open")
	}

	// An unfinished run with no lock at all is open too.
	w = newWorld(t)
	e = w.handEnv("")
	w.healthy()
	saveState(t, state.NewState(e.StateDir, plan.CmdReboot, "node-a", "node-b", []string{"lock"}))
	if err := runDoctor(context.Background(), e, true); err != nil {
		t.Fatal(err)
	}
	if l := w.lines(); l[2] != "A maintenance is in progress; doctor will report but not touch anything." ||
		!strings.HasPrefix(l[3], "  state: reboot node-a: 0 done, 0 failed, 1 pending") {
		t.Errorf("printed:\n%s", w.out)
	}
	if !exists(t, e.StateDir, "state.json") {
		t.Error("an unfinished run's state.json was removed")
	}
}

// What cannot be read is reported as such and does not stop the rest.
func TestDoctorReportsWhatItCannotRead(t *testing.T) {
	w := newWorld(t)
	e := w.handEnv("")
	for _, p := range []string{"/cluster/ceph/flags", "/cluster/ha/status/manager_status", "/cluster/status"} {
		w.pve.Handle("GET", p, func(r pvetest.Request) (any, error) {
			return nil, &pvetest.Error{Status: 500, Message: "no quorum"}
		})
	}
	if err := runDoctor(context.Background(), e, false); err != nil {
		t.Fatal(err)
	}
	wantLines(t, w.lines(),
		"pvectl doctor (from node-b)",
		"",
		"[fail] ceph flags   : GET /cluster/ceph/flags: HTTP 500: no quorum",
		"[ok]   kubernetes   : no node is cordoned",
		"[ok]   silences     : none",
		"[fail] ha maint.    : GET /cluster/ha/status/manager_status: HTTP 500: no quorum",
		"",
		"Nothing to clean up.",
	)
}
