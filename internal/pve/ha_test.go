package pve

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	plog "github.com/TomayGo/pvectl/internal/log"
)

// Shaped like GET /cluster/ha/status/manager_status on PVE 9.2 while pve-1 is
// going into maintenance: one VM mid-migration off it, one VM already moved
// (and remembering where it came from), a stopped container left in place.
const managerStatusJSON = `{
  "lrm_status": {
    "pve-1": {"mode": "maintenance", "state": "maintenance"},
    "pve-2": {"mode": "active", "state": "wait_for_agent_lock"}
  },
  "manager_status": {
    "master_node": "pve-2",
    "node_request": {"pve-1": {"maintenance": 1}, "pve-3": {}},
    "node_status": {"pve-1": "maintenance", "pve-2": "online", "pve-3": "online"},
    "service_status": {
      "vm:100": {"node": "pve-1", "target": "pve-3", "state": "migrate", "maintenance_node": "pve-1"},
      "vm:101": {"node": "pve-2", "state": "started", "maintenance_node": "pve-1"},
      "ct:200": {"node": "pve-1", "state": "stopped"},
      "vm:102": {"node": "pve-3", "target": "pve-1", "state": "migrate"}
    }
  }
}`

func decodeManagerStatus(t *testing.T, raw string) *HAManagerStatus {
	t.Helper()
	var ms HAManagerStatus
	if err := json.Unmarshal([]byte(raw), &ms); err != nil {
		t.Fatal(err)
	}
	return &ms
}

func TestHAManagerStatusMaintenance(t *testing.T) {
	ms := decodeManagerStatus(t, managerStatusJSON)
	if !ms.MaintenanceRequested("pve-1") || ms.MaintenanceRequested("pve-3") || ms.MaintenanceRequested("pve-9") {
		t.Fatal("maintenance request read wrong")
	}
	if ms.NodeState("pve-1") != "maintenance" || ms.NodeState("pve-2") != "online" {
		t.Fatalf("node state read wrong: %v", ms.Manager.NodeStatus)
	}
	if got := ms.MaintenanceNodes(); !reflect.DeepEqual(got, []string{"pve-1"}) {
		t.Fatalf("maintenance nodes: %v", got)
	}
	if ms.Disarmed() {
		t.Fatal("no disarm key means armed")
	}
	if ms.Manager.Master != "pve-2" {
		t.Fatalf("master: %q", ms.Manager.Master)
	}
}

// The evacuation is done when nothing active is on the node or on its way to
// it. A stopped service stays behind under maintenance and must not hold the
// wait open forever.
func TestActiveServicesOn(t *testing.T) {
	ms := decodeManagerStatus(t, managerStatusJSON)
	got := sids(ms.ActiveServicesOn("pve-1"))
	if want := []string{"vm:100", "vm:102"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("active on pve-1: %v, want %v", got, want)
	}
	if got := sids(ms.ActiveServicesOn("pve-2")); !reflect.DeepEqual(got, []string{"vm:101"}) {
		t.Fatalf("active on pve-2: %v", got)
	}
}

func TestReturningTo(t *testing.T) {
	ms := decodeManagerStatus(t, managerStatusJSON)
	// vm:100 is still on pve-1, so it is not "coming back".
	if got := ms.ReturningTo("pve-1"); !reflect.DeepEqual(got, []string{"vm:101"}) {
		t.Fatalf("returning to pve-1: %v", got)
	}
}

func TestDisarmed(t *testing.T) {
	ms := decodeManagerStatus(t, `{"manager_status": {"disarm": {"mode": "freeze", "state": "disarmed"}}}`)
	if !ms.Disarmed() {
		t.Fatal("a disarm entry means disarmed")
	}
	ms = decodeManagerStatus(t, `{"manager_status": {"disarm": null}}`)
	if ms.Disarmed() {
		t.Fatal("null disarm means armed")
	}
}

// An empty manager status (no CRM has run yet) must decode, not fail.
func TestManagerStatusEmpty(t *testing.T) {
	ms := decodeManagerStatus(t, `{"manager_status": {}, "lrm_status": {}}`)
	if ms.MaintenanceRequested("pve-1") || len(ms.MaintenanceNodes()) != 0 || len(ms.ActiveServicesOn("pve-1")) != 0 {
		t.Fatal("empty status must report nothing")
	}
}

func TestFlexBool(t *testing.T) {
	for raw, want := range map[string]bool{`1`: true, `0`: false, `"1"`: true, `"0"`: false, `true`: true, `false`: false, `null`: false, `2`: true} {
		var b flexBool
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if bool(b) != want {
			t.Errorf("%s decoded as %v", raw, b)
		}
	}
	var b flexBool
	if err := json.Unmarshal([]byte(`"maybe"`), &b); err == nil {
		t.Fatal("a non-boolean must be rejected")
	}
}

func TestDecodeRules(t *testing.T) {
	var rules []HARule
	raw := `[{"rule": "prefer-a", "type": "node-affinity", "resources": "vm:100,vm:101", "nodes": "pve-1:2,pve-3:1", "strict": 0, "affinity": "positive"},
	         {"rule": "off", "type": "node-affinity", "resources": "vm:102", "nodes": "pve-2", "strict": 1, "disable": 1}]`
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		t.Fatal(err)
	}
	if !rules[0].Covers("vm:101") || rules[0].Covers("vm:10") {
		t.Fatal("resource list matched wrong")
	}
	if got := rules[0].NodePriorities(); !reflect.DeepEqual(got, map[string]int{"pve-1": 2, "pve-3": 1}) {
		t.Fatalf("priorities: %v", got)
	}
	if !bool(rules[1].Strict) || !bool(rules[1].Disable) {
		t.Fatal("strict/disable not decoded")
	}
}

var cluster3 = []string{"pve-1", "pve-2", "pve-3"}

// The case that broke the old evacuation: a non-strict rule preferring the
// node being evacuated. With it in maintenance the CRM goes to the next
// listed node, not to whichever node has the most free memory.
func TestPlaceHANonStrictTakesNextPriority(t *testing.T) {
	rules := []HARule{{Rule: "prefer-a", Type: "node-affinity", Resources: "vm:100", Nodes: "pve-1:2,pve-3:1"}}
	pl := PlaceHA(rules, "vm:100", cluster3, []string{"pve-2", "pve-3"})
	if pl.Rule != "prefer-a" || pl.Strict {
		t.Fatalf("rule: %+v", pl)
	}
	if !reflect.DeepEqual(pl.Preferred, []string{"pve-3"}) {
		t.Fatalf("preferred %v, want [pve-3]", pl.Preferred)
	}
	// Non-strict still allows an unlisted node, at the lowest priority.
	if !reflect.DeepEqual(pl.Allowed, []string{"pve-2", "pve-3"}) {
		t.Fatalf("allowed %v", pl.Allowed)
	}
	// With every listed node gone, it falls back rather than staying put.
	pl = PlaceHA(rules, "vm:100", cluster3, []string{"pve-2"})
	if !reflect.DeepEqual(pl.Preferred, []string{"pve-2"}) {
		t.Fatalf("fallback preferred %v", pl.Preferred)
	}
}

func TestPlaceHAStrictCanStrand(t *testing.T) {
	rules := []HARule{{Rule: "only-a", Resources: "vm:100", Nodes: "pve-1", Strict: true}}
	pl := PlaceHA(rules, "vm:100", cluster3, []string{"pve-2", "pve-3"})
	if len(pl.Allowed) != 0 || !pl.Strict {
		t.Fatalf("a strict rule naming only the evacuated node allows nothing: %+v", pl)
	}
	rules[0].Nodes = "pve-1,pve-2"
	pl = PlaceHA(rules, "vm:100", cluster3, []string{"pve-2", "pve-3"})
	if !reflect.DeepEqual(pl.Allowed, []string{"pve-2"}) || !reflect.DeepEqual(pl.Preferred, []string{"pve-2"}) {
		t.Fatalf("strict pair: %+v", pl)
	}
}

func TestPlaceHANegative(t *testing.T) {
	rules := []HARule{{Rule: "not-b", Resources: "vm:100", Nodes: "pve-2", Affinity: "negative", Strict: true}}
	pl := PlaceHA(rules, "vm:100", cluster3, []string{"pve-2", "pve-3"})
	if !reflect.DeepEqual(pl.Allowed, []string{"pve-3"}) {
		t.Fatalf("strict negative: %+v", pl)
	}
	rules[0].Strict = false
	pl = PlaceHA(rules, "vm:100", cluster3, []string{"pve-2", "pve-3"})
	if !reflect.DeepEqual(pl.Allowed, []string{"pve-2", "pve-3"}) || !reflect.DeepEqual(pl.Preferred, []string{"pve-3"}) {
		t.Fatalf("non-strict negative: %+v", pl)
	}
}

func TestPlaceHAWithoutRule(t *testing.T) {
	rules := []HARule{
		{Rule: "other", Resources: "vm:999", Nodes: "pve-1", Strict: true},
		{Rule: "disabled", Resources: "vm:100", Nodes: "pve-1", Strict: true, Disable: true},
	}
	pl := PlaceHA(rules, "vm:100", cluster3, []string{"pve-3", "pve-2"})
	if pl.Rule != "" || !reflect.DeepEqual(pl.Allowed, []string{"pve-2", "pve-3"}) || !reflect.DeepEqual(pl.Preferred, pl.Allowed) {
		t.Fatalf("no applicable rule: %+v", pl)
	}
}

func TestNodeMaintenanceArgs(t *testing.T) {
	if got := NodeMaintenanceArgs("pve-1", true); !reflect.DeepEqual(got, []string{"crm-command", "node-maintenance", "enable", "pve-1"}) {
		t.Fatalf("enable: %v", got)
	}
	if got := NodeMaintenanceArgs("pve-1", false); got[2] != "disable" {
		t.Fatalf("disable: %v", got)
	}
}

func TestSetHANodeMaintenance(t *testing.T) {
	saved := runHAManager
	defer func() { runHAManager = saved }()
	var calls [][]string
	runHAManager = func(ctx context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return []byte("node 'pve-9' does not exist\n"), errors.New("exit status 255")
	}

	c := &Client{log: plog.Discard(), DryRun: true}
	if err := c.SetHANodeMaintenance(context.Background(), "pve-1", true); err != nil || len(calls) != 0 {
		t.Fatalf("dry-run must not run ha-manager: err=%v calls=%v", err, calls)
	}

	c.DryRun = false
	err := c.SetHANodeMaintenance(context.Background(), "pve-9", true)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("ha-manager's own message must reach the error: %v", err)
	}
	if len(calls) != 1 || calls[0][3] != "pve-9" {
		t.Fatalf("calls: %v", calls)
	}
}

func sids(ss []HAService) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.SID)
	}
	return out
}
