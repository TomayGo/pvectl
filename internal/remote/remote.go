// Package remote reaches other PVE nodes over the root ssh trust PVE itself
// sets up between cluster members (it is what migration uses), so using it
// adds no new trust -- only a new transport.
//
// Hosts are always addressed by LAN IP: the hostname resolves to the tailnet,
// where Tailscale SSH intercepts the connection and waits for a browser login.
// The host key is checked the way PVE itself checks it (PVE >= 8.2): against
// the node's own /etc/pve/nodes/<node>/ssh_known_hosts, under its node name.
// Nothing else is reliable -- root's ~/.ssh/known_hosts exists on whichever
// nodes someone happened to ssh from by hand, and on the others it does not.
package remote

import (
	"regexp"
	"strings"
)

// Host is a PVE node: its name selects the host key, its IP is where to connect.
type Host struct {
	Name string
	IP   string
}

// Args pins the host key to the one pmxcfs holds for that node, and ignores
// every other known_hosts file so a stale personal entry can neither satisfy
// nor break the check. The remote command goes after these.
func Args(h Host) []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=4",
		"-o", "HostKeyAlias=" + h.Name,
		"-o", "UserKnownHostsFile=/etc/pve/nodes/" + h.Name + "/ssh_known_hosts",
		"-o", "GlobalKnownHostsFile=none",
		"root@" + h.IP,
	}
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./=:,@%+-]+$`)

// Quote renders args as one POSIX shell command line. ssh hands the remote
// side a single string, so anything not obviously inert is single-quoted.
func Quote(args ...string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if shellSafe.MatchString(a) {
			out[i] = a
		} else {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
	}
	return strings.Join(out, " ")
}
