package plan

// Step names. They are the keys of state.json: `resume`, `abort` and the
// rollback data a step leaves for its own undo are all found by these strings,
// and a state.json written by an earlier build has to stay readable. Renaming
// a constant is free; changing its value is not.
const (
	lockStep      = "lock"
	preflightStep = "preflight"
	silenceStep   = "silence"
	cephFlagsStep = "ceph-flags"
	// haStep names the HA evacuation step. restore and doctor read its
	// recorded ownership of the node's HA maintenance.
	haStep           = "ha-evacuate"
	drainStep        = "k8s-drain"
	talosStopStep    = "talos-stop"
	guestStopStep    = "guest-stop"
	waitDownStep     = "wait-down"
	wakeStep         = "wake"
	waitUpStep       = "wait-up"
	restoreStep      = "restore"
	verifyStep       = "verify"
	talosUpgradeStep = "talos-upgrade"
	hostUpgradeStep  = "host-upgrade"
	upgradeCheckStep = "upgrade-check"
)

// hostPowerStep names the step that powers the host off or reboots it:
// host-shutdown or host-reboot, after the PVE node command it sends.
func hostPowerStep(command string) string { return "host-" + command }

// Preflight check names, as --skip-check takes them and the report prints
// them. ceph-flags and k8s-drain are also step names; the two sets are kept
// apart because a check and a step are looked up in different places.
const (
	clusterLockCheck   = "cluster-lock"
	quorumCheck        = "quorum"
	nodesOnlineCheck   = "nodes-online"
	selfHostCheck      = "self-host"
	cephHealthCheck    = "ceph-health"
	cephPGsCheck       = "ceph-pgs"
	cephFlagsCheck     = "ceph-flags"
	k8sReadyCheck      = "k8s-ready"
	k8sDrainCheck      = "k8s-drain"
	etcdCheck          = "etcd"
	haCapacityCheck    = "ha-capacity"
	haAffinityCheck    = "ha-affinity"
	tasksCheck         = "tasks"
	guestsCheck        = "guests"
	sshPathCheck       = "ssh-path"
	ephemeralDataCheck = "ephemeral-data"
	// upgrade only
	aptPlanCheck     = "apt-plan"
	talosTargetCheck = "talos-target"
)
