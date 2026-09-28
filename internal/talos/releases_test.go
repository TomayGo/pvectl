package talos

import "testing"

// Order as GitHub returned it on 2026-09-28: patches to older minors are
// interleaved with newer releases, so "first in the list" is not "newest".
var tags = []string{"v1.14.1", "v1.12.12", "v1.14.0", "v1.13.10", "v1.13.9", "v1.12.11", "v1.13.8"}

func TestPickRelease(t *testing.T) {
	cases := []struct{ alias, running, want string }{
		{AliasLatest, "v1.13.6", "v1.14.1"},
		{AliasLatestPatch, "v1.13.6", "v1.13.10"},
		{AliasLatestPatch, "v1.12.3", "v1.12.12"},
		{AliasLatestPatch, "v1.14.1", "v1.14.1"},
	}
	for _, c := range cases {
		got, err := PickRelease(tags, c.alias, c.running)
		if err != nil || got != c.want {
			t.Errorf("%s from %s = %s %v, want %s", c.alias, c.running, got, err, c.want)
		}
	}
	if _, err := PickRelease(tags, AliasLatestPatch, "v1.11.0"); err == nil {
		t.Error("a minor with no stable release must be an error")
	}
	if _, err := PickRelease(tags, AliasLatestPatch, ""); err == nil {
		t.Error("latest-patch without a running release must be an error")
	}
}
