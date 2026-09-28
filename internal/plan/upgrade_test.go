package plan

import "testing"

func TestWithTagKeepsSchematic(t *testing.T) {
	const base = "factory.talos.dev/installer/ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515:v1.13.6"
	want := "factory.talos.dev/installer/ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515:v1.13.7"
	if got := withTag(base, "v1.13.7"); got != want {
		t.Fatalf("got %s", got)
	}
	// A registry with a port must not be mistaken for a tag.
	if got := withTag("repo.example:5000/installer", "v1.13.7"); got != "repo.example:5000/installer:v1.13.7" {
		t.Fatalf("got %s", got)
	}
	if got := withTag("ghcr.io/siderolabs/installer@sha256:abc", "v1.13.7"); got != "ghcr.io/siderolabs/installer:v1.13.7" {
		t.Fatalf("got %s", got)
	}
}

func TestImageTag(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/siderolabs/installer:v1.13.7":    "v1.13.7",
		"ghcr.io/siderolabs/installer@sha256:abc": "",
		"repo.example:5000/installer":             "",
		"ghcr.io/siderolabs/installer:latest":     "",
	}
	for in, want := range cases {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestOlderThan(t *testing.T) {
	if !olderThan("v1.13.6", "v1.13.7") || !olderThan("v1.9.0", "v1.13.0") {
		t.Fatal("numeric comparison broken")
	}
	if olderThan("v1.13.7", "v1.13.7") || olderThan("v1.14.0", "v1.13.9") || olderThan("junk", "v1.0.0") {
		t.Fatal("false positive")
	}
}

func TestValidateUpgradeOptions(t *testing.T) {
	if err := ValidateUpgradeOptions(Options{TalosVersion: "1.13.7"}); err == nil {
		t.Fatal("a version without the v must be rejected")
	}
	if err := ValidateUpgradeOptions(Options{TalosVersion: "v1.13.7", TalosImage: "x"}); err == nil {
		t.Fatal("both flags at once must be rejected")
	}
	if err := ValidateUpgradeOptions(Options{TalosVersion: "v1.14.0-beta.1"}); err != nil {
		t.Fatal(err)
	}
}
