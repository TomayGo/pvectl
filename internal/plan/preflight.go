package plan

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/tamago/pvectl/internal/state"
)

type Status string

const (
	Pass    Status = "ok"
	Warn    Status = "warn"
	Fail    Status = "FAIL"
	Skipped Status = "skip"
)

type CheckResult struct {
	Name   string
	Status Status
	Detail string
}

type Report struct {
	Results []CheckResult
}

func (r *Report) add(name string, st Status, format string, a ...any) {
	r.Results = append(r.Results, CheckResult{Name: name, Status: st, Detail: fmt.Sprintf(format, a...)})
}

// Failed reports whether any check is blocking. Warnings never block: they are
// things the operator should see, not things that make the operation unsafe.
func (r *Report) Failed() bool {
	for _, c := range r.Results {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

func (r *Report) Failures() []string {
	var out []string
	for _, c := range r.Results {
		if c.Status == Fail {
			out = append(out, c.Name)
		}
	}
	return out
}

func (r *Report) Print(e *Env) {
	for _, c := range r.Results {
		e.Log.Plain("  [%-4s] %-14s %s", c.Status, c.Name, c.Detail)
	}
}

// CheckNames is the full list of --skip-check values, also used by the CLI to
// reject a typo instead of silently skipping nothing.
var CheckNames = []string{
	"cluster-lock",
	"quorum",
	"nodes-online",
	"self-host",
	"ceph-health",
	"ceph-pgs",
	"ceph-flags",
	"k8s-ready",
	"k8s-drain",
	"etcd",
	"ha-capacity",
	"tasks",
	"guests",
	"ssh-path",
}

func ValidCheckName(n string) bool {
	for _, c := range CheckNames {
		if c == n {
			return true
		}
	}
	return false
}

func (e *Env) skipped(name string) bool {
	for _, s := range e.Opts.SkipChecks {
		if s == name {
			return true
		}
	}
	return false
}

// Preflight runs every read-only safety check. It never mutates anything, so
// it is safe to run at any time and is the whole content of `pvectl preflight`.
//
// forShutdown selects the checks that only matter when a node is about to go
// away; `preflight` with no target runs the cluster-wide subset.
func Preflight(ctx context.Context, e *Env, forShutdown bool) *Report {
	r := &Report{}
	run := func(name string, fn func()) {
		if e.skipped(name) {
			r.add(name, Skipped, "skipped by --skip-check")
			return
		}
		fn()
	}

	run("cluster-lock", func() { checkLock(e, r) })
	run("quorum", func() { checkQuorum(ctx, e, r) })
	run("nodes-online", func() { checkNodesOnline(ctx, e, r) })
	if forShutdown {
		run("self-host", func() { checkSelfHost(e, r) })
	}
	run("ceph-health", func() { checkCephHealth(ctx, e, r) })
	run("ceph-pgs", func() { checkCephPGs(ctx, e, r) })
	run("ceph-flags", func() { checkCephFlags(ctx, e, r) })
	run("k8s-ready", func() { checkK8sReady(ctx, e, r) })
	run("etcd", func() { checkEtcd(ctx, e, r) })
	if e.Target != "" {
		run("k8s-drain", func() { checkDrain(ctx, e, r) })
		run("ha-capacity", func() { checkHACapacity(ctx, e, r) })
		run("tasks", func() { checkTasks(ctx, e, r) })
		run("guests", func() { checkGuests(ctx, e, r) })
		if forShutdown {
			run("ssh-path", func() { checkSSHPath(e, r) })
		}
	}
	return r
}

// checkLock refuses to start while someone else is working on the cluster.
//
// It has to tell "someone else's lock" from "the lock this run just took":
// preflight is step 2 of every plan, and step 1 is what acquires the lock. The
// distinction is the target -- AcquireOrAdopt has already refused any lock held
// for a different node before this check ever runs, so a lock we are holding
// here is necessarily for the node we are working on.
func checkLock(e *Env, r *Report) {
	held, err := state.Peek(e.StateDir)
	if err != nil {
		r.add("cluster-lock", Fail, "cannot read the cluster lock: %v", err)
		return
	}
	if held == nil {
		if e.Lock != nil && e.Opts.DryRun {
			r.add("cluster-lock", Pass, "free (this dry run did not take it)")
			return
		}
		r.add("cluster-lock", Pass, "free")
		return
	}
	if e.Lock != nil && held.Target == e.Target {
		r.add("cluster-lock", Pass, "held by this run (%s %s)", held.Command, held.Target)
		return
	}
	r.add("cluster-lock", Fail, "held: %s", held)
}

func checkQuorum(ctx context.Context, e *Env, r *Report) {
	st, err := e.PVEAll.ClusterStatus(ctx)
	if err != nil {
		r.add("quorum", Fail, "cannot read cluster status: %v", err)
		return
	}
	total := len(st.Nodes)
	switch {
	case !st.Quorate:
		r.add("quorum", Fail, "cluster is NOT quorate (%d nodes known)", total)
	case len(st.Online) < total:
		r.add("quorum", Fail, "quorate but only %d/%d nodes online (offline: %s)",
			len(st.Online), total, strings.Join(st.Offline, ", "))
	default:
		r.add("quorum", Pass, "quorate, %d/%d nodes online", len(st.Online), total)
	}
}

// checkNodesOnline is the most important check in the list: taking a second
// node down breaks both corosync quorum and Ceph min_size=2 at once.
func checkNodesOnline(ctx context.Context, e *Env, r *Report) {
	st, err := e.PVEAll.ClusterStatus(ctx)
	if err != nil {
		r.add("nodes-online", Fail, "cannot read cluster status: %v", err)
		return
	}
	var down []string
	for name, n := range st.Nodes {
		if n.Online != 1 && name != e.Target {
			down = append(down, name)
		}
	}
	sort.Strings(down)
	if len(down) > 0 {
		r.add("nodes-online", Fail, "another node is already down: %s -- stopping a second one loses quorum and Ceph min_size", strings.Join(down, ", "))
		return
	}
	r.add("nodes-online", Pass, "every other node is online")
}

// checkSelfHost refuses to drive a node's shutdown from that same node: the
// process would die at host-poweroff, before the restore steps ever run.
func checkSelfHost(e *Env, r *Report) {
	if e.Self == e.Target {
		r.add("self-host", Fail, "pvectl is running on %s, the node it would stop; run it from %s instead",
			e.Target, strings.Join(e.OtherNodes(), " or "))
		return
	}
	r.add("self-host", Pass, "running on %s, target is %s", e.Self, e.Target)
}

func checkCephHealth(ctx context.Context, e *Env, r *Report) {
	cs, err := e.PVEAll.CephStatus(ctx)
	if err != nil {
		r.add("ceph-health", Fail, "cannot read ceph status: %v", err)
		return
	}
	if cs.Health.Status != "HEALTH_OK" {
		r.add("ceph-health", Fail, "%s", cs.HealthSummary())
		return
	}
	r.add("ceph-health", Pass, "HEALTH_OK, %d/%d OSDs up and in",
		cs.OSDMap.NumUpOSDs, cs.OSDMap.NumOSDs)
}

func checkCephPGs(ctx context.Context, e *Env, r *Report) {
	cs, err := e.PVEAll.CephStatus(ctx)
	if err != nil {
		r.add("ceph-pgs", Fail, "cannot read ceph status: %v", err)
		return
	}
	ok, detail := cs.AllPGsActiveClean()
	if !ok {
		r.add("ceph-pgs", Fail, "%s", detail)
		return
	}
	if cs.PGMap.DegradedObjects > 0 || cs.PGMap.MisplacedObjects > 0 {
		r.add("ceph-pgs", Fail, "%d degraded / %d misplaced objects still moving",
			cs.PGMap.DegradedObjects, cs.PGMap.MisplacedObjects)
		return
	}
	r.add("ceph-pgs", Pass, "%s", detail)
}

// checkCephFlags catches the classic leftover: a previous maintenance that
// never ran its restore step and left noout set.
func checkCephFlags(ctx context.Context, e *Env, r *Report) {
	flags, err := e.PVEAll.CephFlags(ctx)
	if err != nil {
		r.add("ceph-flags", Fail, "cannot read ceph flags: %v", err)
		return
	}
	var set []string
	for name, on := range flags {
		if on {
			set = append(set, name)
		}
	}
	sort.Strings(set)
	if len(set) > 0 {
		r.add("ceph-flags", Fail, "flags left set from an earlier run: %s (clear with: pvectl doctor --fix)", strings.Join(set, ", "))
		return
	}
	r.add("ceph-flags", Pass, "no flags set")
}

func checkK8sReady(ctx context.Context, e *Env, r *Report) {
	ok, bad, err := e.K8s.AllReady(ctx)
	if err != nil {
		r.add("k8s-ready", Fail, "cannot list nodes: %v", err)
		return
	}
	if !ok {
		r.add("k8s-ready", Fail, "not all nodes ready and schedulable: %s", strings.Join(bad, ", "))
		return
	}
	nodes, _ := e.K8s.Nodes(ctx)
	r.add("k8s-ready", Pass, "%d/%d nodes Ready and schedulable", len(nodes), len(nodes))
}

func checkDrain(ctx context.Context, e *Env, r *Report) {
	prev, err := e.K8s.DrainPreview(ctx, e.Node.Talos.Name, e.Opts.TimeoutDrain)
	if err != nil {
		r.add("k8s-drain", Fail, "drain preview failed: %v", err)
		return
	}
	if len(prev.Blocked) > 0 {
		var b []string
		for _, p := range prev.Blocked {
			b = append(b, p.String()+" ("+p.Reason+")")
		}
		r.add("k8s-drain", Fail, "a PodDisruptionBudget would block eviction: %s", strings.Join(b, "; "))
		return
	}
	detail := fmt.Sprintf("%d pod(s) would be evicted from %s", len(prev.Evictable), e.Node.Talos.Name)
	if len(prev.Warnings) > 0 {
		r.add("k8s-drain", Warn, "%s; %s", detail, strings.Join(prev.Warnings, "; "))
		return
	}
	r.add("k8s-drain", Pass, "%s", detail)
}

func checkEtcd(ctx context.Context, e *Env, r *Report) {
	h, err := e.Talos.Etcd(ctx, e.TalosIPs())
	if err != nil {
		r.add("etcd", Fail, "etcd check failed: %v", err)
		return
	}
	expect := len(e.Cfg.Nodes)
	if !h.OK(expect) {
		r.add("etcd", Fail, "%s", h.Summary(expect))
		return
	}
	r.add("etcd", Pass, "%s", h.Summary(expect))
}

// checkHACapacity verifies the remaining nodes can actually hold the HA
// services that would be evacuated. groups.cfg is empty in this cluster, so
// placement floats and has to be read fresh every time.
func checkHACapacity(ctx context.Context, e *Env, r *Report) {
	ha, err := e.PVEAll.HAStatus(ctx)
	if err != nil {
		r.add("ha-capacity", Fail, "cannot read HA status: %v", err)
		return
	}
	svcs := ha.ServicesOn(e.Target)
	if len(svcs) == 0 {
		r.add("ha-capacity", Pass, "no HA resources on %s (master is %s)", e.Target, ha.Master)
		return
	}
	res, err := e.PVEAll.Resources(ctx, "vm")
	if err != nil {
		r.add("ha-capacity", Fail, "cannot read guest resources: %v", err)
		return
	}
	byID := map[string]int64{}
	for _, g := range res {
		byID[fmt.Sprintf("%s:%d", g.SIDKind(), g.VMID)] = g.MaxMem
	}
	var need int64
	var names []string
	for _, s := range svcs {
		need += byID[s.SID]
		names = append(names, s.SID)
	}
	var free int64
	var per []string
	for _, other := range e.OtherNodes() {
		ns, err := e.PVEAll.NodeStatus(ctx, other)
		if err != nil {
			r.add("ha-capacity", Warn, "cannot read %s memory: %v", other, err)
			return
		}
		f := ns.Memory.Total - ns.Memory.Used
		free += f
		per = append(per, fmt.Sprintf("%s %s free", other, humanBytes(f)))
	}
	if need > free {
		r.add("ha-capacity", Fail, "%s need %s but only %s free across %s",
			strings.Join(names, ", "), humanBytes(need), humanBytes(free), strings.Join(e.OtherNodes(), "+"))
		return
	}
	r.add("ha-capacity", Pass, "%s need %s; %s", strings.Join(names, ", "), humanBytes(need), strings.Join(per, ", "))
}

// checkTasks refuses to proceed while a backup or a storage replication job is
// mid-flight on the target.
func checkTasks(ctx context.Context, e *Env, r *Report) {
	tasks, err := e.PVEAll.RunningTasks(ctx, e.Target)
	if err != nil {
		r.add("tasks", Warn, "cannot list running tasks on %s: %v", e.Target, err)
		return
	}
	var blocking []string
	for _, t := range tasks {
		switch t.Type {
		case "vzdump", "pvesr", "qmigrate", "imgcopy", "download":
			blocking = append(blocking, fmt.Sprintf("%s (%s)", t.Type, t.ID))
		}
	}
	if len(blocking) > 0 {
		r.add("tasks", Fail, "still running on %s: %s", e.Target, strings.Join(blocking, ", "))
		return
	}
	r.add("tasks", Pass, "no backup or replication running on %s", e.Target)
}

// checkGuests reconciles config against reality: a passthrough guest that
// appeared since the config was written would be killed by the poweroff
// without ever being shut down cleanly.
func checkGuests(ctx context.Context, e *Env, r *Report) {
	live, err := e.PVEAll.GuestsOn(ctx, e.Target)
	if err != nil {
		r.add("guests", Warn, "cannot list guests on %s: %v", e.Target, err)
		return
	}
	configured := map[int]bool{}
	for _, g := range e.Node.StopGuests {
		configured[g.ID] = true
	}
	var running, missing, warns []string
	for _, g := range e.Node.StopGuests {
		lr, ok := live[g.ID]
		if !ok {
			missing = append(missing, fmt.Sprintf("%d (not on %s)", g.ID, e.Target))
			continue
		}
		if lr.Status == "running" {
			running = append(running, fmt.Sprintf("%d %s", g.ID, lr.Name))
			if g.Warn != "" {
				warns = append(warns, fmt.Sprintf("%d %s: %s", g.ID, lr.Name, g.Warn))
			}
		}
	}
	// Anything running with passthrough hardware that the config does not know
	// about is the dangerous case.
	var unknown []string
	for id, lr := range live {
		if configured[id] || lr.Status != "running" {
			continue
		}
		if isTalosVM(e, id) {
			continue
		}
		cfg, err := e.PVEAll.GuestConfig(ctx, e.Target, lr.Kind(), id)
		if err != nil {
			continue
		}
		if hasPassthrough(cfg) && lr.HAState == "" {
			unknown = append(unknown, fmt.Sprintf("%d %s", id, lr.Name))
		}
	}
	detail := "no guests configured for " + e.Target
	switch {
	case len(running) > 0:
		detail = "will stop " + strings.Join(running, ", ")
	case len(e.Node.StopGuests) > 0:
		detail = fmt.Sprintf("%d configured guest(s), none currently running", len(e.Node.StopGuests))
	}
	if len(missing) > 0 {
		detail += "; not present: " + strings.Join(missing, ", ")
	}
	switch {
	case len(unknown) > 0:
		r.add("guests", Fail, "%s; passthrough guests missing from the config: %s (run `pvectl config discover`)",
			detail, strings.Join(unknown, ", "))
	case len(warns) > 0:
		r.add("guests", Warn, "%s -- %s", detail, strings.Join(warns, " / "))
	default:
		r.add("guests", Pass, "%s", detail)
	}
}

func isTalosVM(e *Env, id int) bool {
	for _, n := range e.Cfg.Nodes {
		if n.Talos.VMID == id {
			return true
		}
	}
	return false
}

func hasPassthrough(cfg map[string]any) bool {
	for k := range cfg {
		if strings.HasPrefix(k, "hostpci") || strings.HasPrefix(k, "usb") {
			return true
		}
	}
	return false
}

// checkSSHPath catches the two ways a shutdown can cut the connection that is
// driving it: the session originating on the node itself, or arriving over the
// Wi-Fi that the node's AP guest provides.
func checkSSHPath(e *Env, r *Report) {
	var netGuests []string
	for _, g := range e.Node.StopGuests {
		if g.ProvidesNetwork {
			netGuests = append(netGuests, fmt.Sprintf("%d", g.ID))
		}
	}
	from := sshClientIP()
	if from == "" {
		if len(netGuests) == 0 {
			r.add("ssh-path", Pass, "not an SSH session, and %s hosts no network-providing guest", e.Target)
			return
		}
		r.add("ssh-path", Warn, "%s hosts guest %s which carries the LAN; this session is not over SSH, but anything else on that network will drop",
			e.Target, strings.Join(netGuests, ", "))
		return
	}
	if from == e.Node.LANIP {
		r.add("ssh-path", Fail, "this session comes from %s itself; stopping it kills the run mid-plan", e.Target)
		return
	}
	// A session arriving from another cluster node is on the wired LAN and
	// survives the AP guest going away.
	if peer := e.Cfg.NodeByIP(from); peer != "" {
		r.add("ssh-path", Pass, "session comes from %s over the wired LAN", peer)
		return
	}
	if len(netGuests) == 0 {
		r.add("ssh-path", Pass, "no network-providing guest on %s", e.Target)
		return
	}
	r.add("ssh-path", Fail, "%s hosts guest %s which carries the LAN, and this session comes from %s -- it will be cut mid-run; connect from another node, or pass --skip-check ssh-path if you are on wired ethernet",
		e.Target, strings.Join(netGuests, ", "), from)
}

// sshClientIP returns the client address of the current SSH session, if any.
func sshClientIP() string {
	for _, v := range []string{os.Getenv("SSH_CONNECTION"), os.Getenv("SSH_CLIENT")} {
		if f := strings.Fields(v); len(f) > 0 {
			return f[0]
		}
	}
	return ""
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
