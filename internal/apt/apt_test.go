package apt

import "testing"

// Lines taken from a real `apt-get -s full-upgrade` on pve-1 (2026-09-28), plus
// a new kernel install and a removal, which are the two shapes that matter.
const sample = `NOTE: This is only a simulation!
Reading package lists...
Inst libc6 [2.41-12+deb13u3] (2.41-12+deb13u4 Debian:13.7/stable [amd64])
Inst ceph-osd [20.2.1-pve1] (20.2.4-pve4 Proxmox Ceph 20 Tentacle Debian Repository:stable [amd64]) [ceph:amd64 ]
Inst proxmox-kernel-7.0.15-1-pve-signed (7.0.15-1 Proxmox VE:9.x/trixie [amd64])
Remv old-thing [1.0-1]
Conf libc6 (2.41-12+deb13u4 Debian:13.7/stable [amd64])
`

func TestParsePlan(t *testing.T) {
	p := ParsePlan(sample)
	if len(p.Upgrade) != 2 || len(p.Install) != 1 || len(p.Remove) != 1 {
		t.Fatalf("got %s", p.Summary())
	}
	if got := p.Find("ceph-osd"); got == nil || got.Old != "20.2.1-pve1" || got.New != "20.2.4-pve4" {
		t.Fatalf("ceph-osd parsed wrong: %+v", got)
	}
	if got := p.Find("proxmox-kernel-7.0.15-1-pve-signed"); got == nil || got.Old != "" || got.New != "7.0.15-1" {
		t.Fatalf("new kernel parsed wrong: %+v", got)
	}
	if p.Remove[0].Name != "old-thing" {
		t.Fatalf("removal parsed wrong: %+v", p.Remove[0])
	}
	if b := p.MajorBumps(); len(b) != 0 {
		t.Fatalf("a Ceph point release must not count as a major bump: %v", b)
	}
}

func TestMajorBumps(t *testing.T) {
	p := ParsePlan("Inst ceph-mon [19.2.3-pve1] (20.2.4-pve4 Proxmox [amd64])\n" +
		"Inst pve-manager [9.2.4] (9.2.5 Proxmox [all])\n" +
		"Inst libfoo [1:1.0] (2:2.0 Debian [amd64])\n")
	b := p.MajorBumps()
	if len(b) != 1 || b[0] != "ceph-mon 19.2.3-pve1 -> 20.2.4-pve4" {
		t.Fatalf("got %v", b)
	}
}

func TestMajorIgnoresEpoch(t *testing.T) {
	if major("2:20.2.4-pve4") != "20" || major("9.2.5") != "9" {
		t.Fatal("epoch not stripped")
	}
}

// The host key must come from pmxcfs under the node's name, never from a
// personal known_hosts: that file exists only where someone ssh'd by hand.
func TestSSHArgsPinPVEHostKey(t *testing.T) {
	args := sshArgs(Host{Name: "pve-1", IP: "192.0.2.79"})
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
