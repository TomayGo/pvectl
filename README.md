# pvectl

Graceful node maintenance for the pve-1/2/3 cluster (Proxmox VE + Ceph + Talos
Kubernetes + passthrough guests).

One command replaces the four-subsystem manual procedure. A cluster-wide lock in
`/etc/pve` makes it structurally impossible to take two nodes down at once,
which is the failure that breaks both corosync quorum and Ceph `min_size=2`.

Design notes: [`/root/pvectl-design.md`](../pvectl-design.md).

## Install

Built on pve-1 and copied to each node:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w -X main.version=$(git describe --tags --always)" -o pvectl ./cmd/pvectl
```

| path | what | permissions |
|---|---|---|
| `/usr/local/bin/pvectl` | the static binary (~58 MB) | 0755 |
| `/etc/pve/pvectl/config.yaml` | topology, no secrets | root:www-data 0640 (pmxcfs) |
| `/etc/pve/pvectl/maintenance.lock` | the cluster lock | created by pvectl |
| `/etc/pve/pvectl/state.json` | resumable run state | created by pvectl |
| `/etc/pvectl/{token,kubeconfig,talosconfig}` | credentials, node-local | 0600 root |
| `/var/log/pvectl/<timestamp>.log` | one transcript per run | 0640 root |

Secrets are node-local on purpose: `/etc/pve` is `root:www-data 0640` and
pveproxy (port 8006) runs as `www-data`, so anything placed there is readable by
the web UI's process.

## Everyday use

```bash
pvectl status                       # maintenance state, leftover flags, cordons, HA placement
pvectl preflight pve-3              # safety checks only, changes nothing
pvectl plan reboot node pve-3       # the plan and its blast radius
pvectl reboot node pve-3            # the real thing, end to end
pvectl rolling reboot cluster       # one node at a time, for kernel updates
pvectl doctor --fix                 # clear leftovers from an interrupted run
```

A shutdown leaves the window open, so it is two commands:

```bash
pvectl shutdown node pve-3          # ... physical work ...
pvectl start node pve-3             # WoL, wait, restore, verify
```

`maint enter` / `maint exit` do the same without touching the power, for work on
a machine that stays on.

If a run stops partway, `pvectl resume` continues it and `pvectl abort` rolls it
back. Both read the same `state.json`, so neither can invent a different plan.

## What it will refuse to do

Preflight blocks, and says why, when:

- another node is already down or another maintenance holds the lock
- Ceph is not `HEALTH_OK`, or any PG is not `active+clean`
- flags are left set from an earlier run that never finished
- a PodDisruptionBudget would make the drain hang
- etcd is not 3/3
- the remaining nodes lack the memory for the HA resources being evacuated
- a backup or replication job is running on the target
- you are running it *on* the node you asked it to stop
- your own SSH session arrives over the Wi-Fi that the target's AP guest serves

Each check has a name that `--skip-check` accepts, listed by `pvectl --help`.
`--dry-run` is available on every command.

## Notes on the implementation

- **All Proxmox work goes through the API**, never ssh or `ceph`/`ha-manager`.
  Requests aimed at a node are sent through a *different* node's pveproxy: a
  `POST /nodes/pve-1/status --command shutdown` delivered to pve-1 loses its
  connection before the reply arrives.
- **HA evacuation is explicit** rather than relying on
  `shutdown_policy=migrate`, so it has a progress report and a timeout. HA
  resources are *not* moved back afterwards -- containers migrate offline, and
  putting them back costs a second outage. `status` reports the imbalance.
- **Wake-on-LAN goes to both bond slaves.** The bonds are balance-alb with
  `wol=g` on both NICs; while the machine is off, which PHY the switch delivers
  to is not predictable.
- **Alertmanager and discord-bridge are reached through the API server's service
  proxy**, not `kubectl port-forward` -- no local port and no tunnel to keep
  alive across a node going down.
- **Every API call is logged** as `API <METHOD> <PATH> -> <status>`. The token
  starts at `--privsep 0`; to narrow it later:

  ```bash
  grep -h ' API ' /var/log/pvectl/*.log | awk '{print $4, $5}' | sort -u
  ```

## Tests

```bash
go test ./...
```

Covers the parts that cannot be exercised safely against the live cluster: the
lock's exclusion and adoption, state round-trips through JSON, WoL packet
construction, silence-matcher regex escaping, and the shape of each plan.
