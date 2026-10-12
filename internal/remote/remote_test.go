package remote

import (
	"os/exec"
	"testing"
)

// The host key must come from pmxcfs under the node's name, never from a
// personal known_hosts: that file exists only where someone ssh'd by hand.
func TestArgsPinPVEHostKey(t *testing.T) {
	args := Args(Host{Name: "pve-1", IP: "192.0.2.79"})
	want := map[string]bool{
		"HostKeyAlias=pve-1": false,
		"UserKnownHostsFile=/etc/pve/nodes/pve-1/ssh_known_hosts": false,
		"GlobalKnownHostsFile=none":                               false,
	}
	for _, a := range args {
		if _, ok := want[a]; ok {
			want[a] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing -o %s", k)
		}
	}
	if args[len(args)-1] != "root@192.0.2.79" {
		t.Errorf("must connect by LAN IP, got %s", args[len(args)-1])
	}
}

// Quote must survive a real shell unchanged, whatever the argument holds.
func TestQuoteRoundTrips(t *testing.T) {
	args := []string{"--timeout-up=15m0s", "it's", "a b", "$HOME", "", `"x"`, "--skip-check=ceph-health"}
	out, err := exec.Command("sh", "-c", `printf '%s\n' `+Quote(args...)).Output()
	if err != nil {
		t.Fatal(err)
	}
	var want string
	for _, a := range args {
		want += a + "\n"
	}
	if string(out) != want {
		t.Errorf("got %q, want %q", out, want)
	}
	if got := Quote("pvectl", "--yes"); got != "pvectl --yes" {
		t.Errorf("plain args should stay unquoted, got %q", got)
	}
}
