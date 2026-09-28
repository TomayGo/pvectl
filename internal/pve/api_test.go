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

func statusWith(health string, codes ...string) *CephStatus {
	s := &CephStatus{}
	s.Health.Status = health
	s.Health.Checks = map[string]struct {
		Severity string `json:"severity"`
		Summary  struct {
			Message string `json:"message"`
		} `json:"summary"`
	}{}
	for _, c := range codes {
		s.Health.Checks[c] = s.Health.Checks[c]
	}
	return s
}

// The state after the 2026-09-28 upgrade: only cephx cipher checks, at ERR.
func TestHealthGateIgnoresOnlyListedChecks(t *testing.T) {
	ignore := []string{"AUTH_INSECURE_*"}
	s := statusWith("HEALTH_ERR", "AUTH_INSECURE_SERVICE_TICKETS", "AUTH_INSECURE_CLIENT_KEY_TYPE")
	if ok, ign := s.HealthGate(ignore); !ok || len(ign) != 2 {
		t.Fatalf("cipher-only ERR should pass, got %v %v", ok, ign)
	}
	s = statusWith("HEALTH_WARN", "AUTH_INSECURE_SERVICE_TICKETS", "OSD_DOWN")
	if ok, _ := s.HealthGate(ignore); ok {
		t.Fatal("an unlisted check must still block")
	}
	if ok, _ := statusWith("HEALTH_WARN").HealthGate(ignore); ok {
		t.Fatal("not OK with no checks to judge must block")
	}
	if ok, _ := statusWith("HEALTH_ERR", "AUTH_INSECURE_SERVICE_TICKETS").HealthGate(nil); ok {
		t.Fatal("with nothing ignored, the cipher check blocks as before")
	}
}
