package alert

import (
	"context"
	"errors"
	"regexp"
	"testing"

	plog "github.com/TomayGo/pvectl/internal/log"
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

type fakeProxy struct {
	deleteErr error
	listBody  string
	listErr   error
}

func (f fakeProxy) ProxyRequest(_ context.Context, method, _, _ string, _ int, _ string, _ []byte) ([]byte, error) {
	if method == "DELETE" {
		return nil, f.deleteErr
	}
	return []byte(f.listBody), f.listErr
}

// A maintenance window left open past Alertmanager's retention finds its
// silences gone; closing it must not fail on that, but must still fail when
// the silence is live or Alertmanager cannot be asked.
func TestDeleteSilenceAlreadyGone(t *testing.T) {
	notFound := errors.New("the server could not find the requested resource")
	cases := []struct {
		name    string
		proxy   fakeProxy
		wantErr bool
	}{
		{"deleted", fakeProxy{}, false},
		{"purged", fakeProxy{deleteErr: notFound, listBody: `[{"id":"other","status":{"state":"active"}}]`}, false},
		{"expired", fakeProxy{deleteErr: notFound, listBody: `[{"id":"s1","status":{"state":"expired"}}]`}, false},
		{"still active", fakeProxy{deleteErr: notFound, listBody: `[{"id":"s1","status":{"state":"active"}}]`}, true},
		{"unreachable", fakeProxy{deleteErr: notFound, listErr: notFound}, true},
	}
	for _, tc := range cases {
		c := New(tc.proxy, Endpoint{}, Endpoint{}, plog.Discard(), false)
		if err := c.DeleteSilence(context.Background(), "s1"); (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
	}
}
