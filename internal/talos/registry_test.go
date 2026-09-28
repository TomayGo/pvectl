package talos

import "testing"

func TestSplitRef(t *testing.T) {
	cases := []struct{ in, host, repo, tag string }{
		{"factory.talos.dev/installer/abc:v1.13.7", "factory.talos.dev", "installer/abc", "v1.13.7"},
		{"ghcr.io/siderolabs/installer:v1.13.7", "ghcr.io", "siderolabs/installer", "v1.13.7"},
		{"repo.example:5000/installer:v1", "repo.example:5000", "installer", "v1"},
		{"ghcr.io/siderolabs/installer@sha256:abc", "ghcr.io", "siderolabs/installer", "sha256:abc"},
	}
	for _, c := range cases {
		h, r, tg, err := splitRef(c.in)
		if err != nil || h != c.host || r != c.repo || tg != c.tag {
			t.Errorf("splitRef(%s) = %s %s %s %v", c.in, h, r, tg, err)
		}
	}
	if _, _, _, err := splitRef("siderolabs/installer:v1"); err == nil {
		t.Error("a reference without a registry host must be rejected")
	}
}
