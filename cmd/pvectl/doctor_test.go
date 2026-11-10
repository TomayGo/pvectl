package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	plog "github.com/TomayGo/pvectl/internal/log"
)

// fakeCephFlags stands in for the PVE client: it records every flag doctor
// asks to change and can be told to refuse one.
type fakeCephFlags struct {
	calls []string
	fail  map[string]error
}

func (f *fakeCephFlags) SetCephFlag(ctx context.Context, flag string, on bool) error {
	verb := "unset"
	if on {
		verb = "set"
	}
	f.calls = append(f.calls, verb+" "+flag)
	return f.fail[flag]
}

func runDoctorCephFlags(t *testing.T, pc *fakeCephFlags, flags map[string]bool, fix bool) (int, string) {
	t.Helper()
	var out bytes.Buffer
	found := doctorCephFlags(context.Background(), plog.To(&out), pc, []string{"noout", "norebalance"}, flags, fix)
	return found, out.String()
}

// The case this exists for: a node maintenance left noout behind while an
// operator (or a cluster freeze) has nodown and pause set. --fix must clear
// the first and keep its hands off the others.
func TestDoctorFixClearsOnlyMaintenanceFlags(t *testing.T) {
	pc := &fakeCephFlags{}
	flags := map[string]bool{"noout": true, "norebalance": false, "nodown": true, "pause": true, "noscrub": false}
	found, out := runDoctorCephFlags(t, pc, flags, true)

	if want := []string{"unset noout"}; !reflect.DeepEqual(pc.calls, want) {
		t.Errorf("flags changed: %v, want %v", pc.calls, want)
	}
	if found != 1 {
		t.Errorf("found %d issue(s), want 1: only the maintenance flag is doctor's to fix", found)
	}
	for _, want := range []string{
		"[warn] ceph flags   : noout still set",
		"       -> cleared noout",
		"[warn] ceph flags   : nodown pause set, but not by a node maintenance (cluster.ceph_flags is noout norebalance); leaving them alone",
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "cleared nodown") || strings.Contains(out, "cleared pause") {
		t.Errorf("a flag outside cluster.ceph_flags was reported cleared:\n%s", out)
	}
}

// Without --fix nothing is changed, but both kinds are still reported.
func TestDoctorReportsMixedFlagsWithoutFix(t *testing.T) {
	pc := &fakeCephFlags{}
	found, out := runDoctorCephFlags(t, pc, map[string]bool{"noout": true, "norebalance": true, "nodown": true}, false)
	if len(pc.calls) != 0 {
		t.Errorf("a report-only run changed flags: %v", pc.calls)
	}
	if found != 1 {
		t.Errorf("found %d, want 1", found)
	}
	if !strings.Contains(out, "[warn] ceph flags   : noout norebalance still set\n") ||
		!strings.Contains(out, "[warn] ceph flags   : nodown set, but not by a node maintenance (cluster.ceph_flags is noout norebalance); leaving it alone\n") {
		t.Errorf("unexpected report:\n%s", out)
	}
}

// Flags nobody but the operator set are not something --fix addresses, so they
// must not be counted into "N issue(s) addressed".
func TestDoctorLeavesForeignFlagsAlone(t *testing.T) {
	pc := &fakeCephFlags{}
	found, out := runDoctorCephFlags(t, pc, map[string]bool{"noout": false, "nodown": true}, true)
	if len(pc.calls) != 0 {
		t.Errorf("doctor --fix touched a flag it does not manage: %v", pc.calls)
	}
	if found != 0 {
		t.Errorf("found %d, want 0", found)
	}
	if !strings.Contains(out, "[warn] ceph flags   : nodown set") || strings.Contains(out, "[ok]") {
		t.Errorf("unexpected report:\n%s", out)
	}
}

func TestDoctorCephFlagsNoneSet(t *testing.T) {
	pc := &fakeCephFlags{}
	found, out := runDoctorCephFlags(t, pc, map[string]bool{"noout": false, "pause": false}, true)
	if found != 0 || len(pc.calls) != 0 || out != "[ok]   ceph flags   : none set\n" {
		t.Errorf("found=%d calls=%v out=%q", found, pc.calls, out)
	}
}

// One flag failing to clear must not stop the others.
func TestDoctorFixKeepsGoingPastAFailure(t *testing.T) {
	pc := &fakeCephFlags{fail: map[string]error{"noout": errors.New("HTTP 500")}}
	_, out := runDoctorCephFlags(t, pc, map[string]bool{"noout": true, "norebalance": true}, true)
	if want := []string{"unset noout", "unset norebalance"}; !reflect.DeepEqual(pc.calls, want) {
		t.Errorf("calls %v, want %v", pc.calls, want)
	}
	if !strings.Contains(out, "       -> clearing noout failed: HTTP 500\n") || !strings.Contains(out, "       -> cleared norebalance\n") {
		t.Errorf("unexpected report:\n%s", out)
	}
}
