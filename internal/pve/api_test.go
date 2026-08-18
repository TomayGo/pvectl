package pve

import "testing"

func TestAllPGsActiveClean(t *testing.T) {
	var s CephStatus
	s.PGMap.NumPGs = 65
	s.PGMap.PGsByState = []struct {
		StateName string `json:"state_name"`
		Count     int    `json:"count"`
	}{{StateName: "active+clean", Count: 65}}
	if ok, detail := s.AllPGsActiveClean(); !ok {
		t.Fatalf("expected clean, got %s", detail)
	}

	s.PGMap.PGsByState[0].Count = 60
	s.PGMap.PGsByState = append(s.PGMap.PGsByState, struct {
		StateName string `json:"state_name"`
		Count     int    `json:"count"`
	}{StateName: "active+undersized+degraded", Count: 5})
	ok, detail := s.AllPGsActiveClean()
	if ok {
		t.Fatal("degraded PGs must not count as clean")
	}
	if detail == "" {
		t.Fatal("expected the non-clean states to be named")
	}
}

func TestSIDKind(t *testing.T) {
	// HA spells containers "ct:500" while the API path segment is "lxc".
	if got := (Resource{Type: "lxc"}).SIDKind(); got != "ct" {
		t.Fatalf("lxc SIDKind is %q, want ct", got)
	}
	if got := (Resource{Type: "lxc"}).Kind(); got != "lxc" {
		t.Fatalf("lxc Kind is %q, want lxc", got)
	}
	if got := (Resource{Type: "qemu"}).SIDKind(); got != "vm" {
		t.Fatalf("qemu SIDKind is %q, want vm", got)
	}
}

func TestRetryableStatus(t *testing.T) {
	// 595/596 are what pveproxy returns when it cannot reach the node that
	// owns the resource; those must fail over to another endpoint.
	for _, c := range []int{595, 596, 502, 503, 504} {
		if !retryableStatus(c) {
			t.Errorf("%d should be retryable", c)
		}
	}
	// A real API error is the same from every endpoint.
	for _, c := range []int{400, 401, 403, 404, 500} {
		if retryableStatus(c) {
			t.Errorf("%d should not trigger failover", c)
		}
	}
}
