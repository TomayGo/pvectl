package alert

import (
	"regexp"
	"testing"
)

// The instance matcher is a regex, so the dots in an IP have to be escaped --
// otherwise 192.0.2.8 would also match 192x0y2z8 and, more importantly,
// 192.0.2.81 would match 192.0.2.8 plus any character.
func TestNodeMatchersEscapeIPs(t *testing.T) {
	ms := NodeMatchers("pve-3", "talos-cp-3", []string{"192.0.2.81", "198.51.100.3"})
	if len(ms) != 2 {
		t.Fatalf("got %d matchers, want 2", len(ms))
	}
	if ms[0].Name != "node" || ms[0].Value != "pve-3|talos-cp-3" {
		t.Fatalf("node matcher is %+v", ms[0])
	}
	re, err := regexp.Compile("^(?:" + ms[1].Value + ")$")
	if err != nil {
		t.Fatalf("instance matcher does not compile: %v", err)
	}
	for _, ok := range []string{"192.0.2.81", "192.0.2.81:9100", "198.51.100.3:10250"} {
		if !re.MatchString(ok) {
			t.Errorf("%s should match %s", ok, ms[1].Value)
		}
	}
	for _, bad := range []string{"192.0.2.8", "192x0x2x81", "192.0.2.811", "198.51.100.30"} {
		if re.MatchString(bad) {
			t.Errorf("%s should NOT match %s", bad, ms[1].Value)
		}
	}
}

func TestNodeMatchersWithoutIPs(t *testing.T) {
	ms := NodeMatchers("pve-1", "talos-cp-1", nil)
	if len(ms) != 1 {
		t.Fatalf("got %d matchers, want just the node one", len(ms))
	}
}
