// Package k8s wraps the Kubernetes side of a node maintenance: cordon, drain,
// uncordon and readiness waits against the Talos control-plane VM that lives on
// the Proxmox node being stopped.
//
// It uses client-go plus kubectl's own drain helper so the eviction semantics
// match `kubectl drain` exactly, rather than being re-implemented and drifting.
package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubectl/pkg/drain"

	plog "github.com/tamago/pvectl/internal/log"
)

type Client struct {
	kubeconfig string
	once       sync.Once
	client     kubernetes.Interface
	clientErr  error
	log        *plog.Logger
	dryRun     bool
}

// New never fails: the client is built on first use instead.
//
// Half of pvectl's commands -- status, doctor, config validate -- have plenty
// to say about quorum, Ceph and the maintenance lock without Kubernetes. A
// missing kubeconfig should cost you the Kubernetes lines, not the whole tool.
func New(kubeconfig string, lg *plog.Logger, dryRun bool) *Client {
	return &Client{kubeconfig: kubeconfig, log: lg, dryRun: dryRun}
}

func (c *Client) cs() (kubernetes.Interface, error) {
	c.once.Do(func() {
		cfg, err := clientcmd.BuildConfigFromFlags("", c.kubeconfig)
		if err != nil {
			c.clientErr = fmt.Errorf("load kubeconfig %s: %w", c.kubeconfig, err)
			return
		}
		// The API server sits behind the same LAN the node is on; a hung
		// request must not stall a maintenance window.
		cfg.Timeout = 30 * time.Second
		cl, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			c.clientErr = fmt.Errorf("build kubernetes client: %w", err)
			return
		}
		c.client = cl
	})
	return c.client, c.clientErr
}

type NodeInfo struct {
	Name          string
	Ready         bool
	Unschedulable bool
	Reason        string
	Version       string
}

func (c *Client) Nodes(ctx context.Context) ([]NodeInfo, error) {
	cs, err := c.cs()
	if err != nil {
		return nil, err
	}
	list, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	out := make([]NodeInfo, 0, len(list.Items))
	for i := range list.Items {
		n := &list.Items[i]
		info := NodeInfo{
			Name:          n.Name,
			Unschedulable: n.Spec.Unschedulable,
			Version:       n.Status.NodeInfo.KubeletVersion,
		}
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady {
				info.Ready = cond.Status == corev1.ConditionTrue
				info.Reason = cond.Reason
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (c *Client) node(ctx context.Context, name string) (*corev1.Node, error) {
	cs, err := c.cs()
	if err != nil {
		return nil, err
	}
	n, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get node %s: %w", name, err)
	}
	return n, nil
}

func (c *Client) helper(ctx context.Context, timeout time.Duration) (*drain.Helper, error) {
	cs, err := c.cs()
	if err != nil {
		return nil, err
	}
	return &drain.Helper{
		Ctx:                 ctx,
		Client:              cs,
		IgnoreAllDaemonSets: true,
		DeleteEmptyDirData:  true,
		// Negative means "use the pod's own terminationGracePeriodSeconds";
		// 0 would kill pods instantly.
		GracePeriodSeconds: -1,
		Timeout:            timeout,
		Out:                logWriter{c.log, false},
		ErrOut:             logWriter{c.log, true},
		OnPodDeletionOrEvictionFinished: func(pod *corev1.Pod, usingEviction bool, err error) {
			verb := "deleted"
			if usingEviction {
				verb = "evicted"
			}
			if err != nil {
				c.log.Warnf("%s/%s not %s: %v", pod.Namespace, pod.Name, verb, err)
				return
			}
			c.log.Debugf("%s/%s %s", pod.Namespace, pod.Name, verb)
		},
	}, nil
}

// SetSchedulable cordons (desired=false) or uncordons (desired=true).
func (c *Client) SetSchedulable(ctx context.Context, name string, schedulable bool) error {
	n, err := c.node(ctx, name)
	if err != nil {
		return err
	}
	h := drain.NewCordonHelper(n)
	if !h.UpdateIfRequired(!schedulable) {
		c.log.Debugf("node %s already %s", name, schedulableWord(schedulable))
		return nil
	}
	if c.dryRun {
		c.log.Infof("(dry-run) would mark node %s %s", name, schedulableWord(schedulable))
		return nil
	}
	cs, err := c.cs()
	if err != nil {
		return err
	}
	if err, patchErr := h.PatchOrReplaceWithContext(ctx, cs, false); err != nil || patchErr != nil {
		if patchErr != nil {
			return fmt.Errorf("cordon %s: %w", name, patchErr)
		}
		return fmt.Errorf("cordon %s: %w", name, err)
	}
	c.log.Infof("node %s marked %s", name, schedulableWord(schedulable))
	return nil
}

func schedulableWord(schedulable bool) string {
	if schedulable {
		return "schedulable"
	}
	return "unschedulable"
}

// Drain evicts everything evictable off the node. Cordon must have happened
// first; Drain does not cordon so that `maint enter` can report the two
// separately and undo them separately.
func (c *Client) Drain(ctx context.Context, name string, timeout time.Duration) error {
	if c.dryRun {
		pods, err := c.DrainPreview(ctx, name, timeout)
		if err != nil {
			return err
		}
		c.log.Infof("(dry-run) would evict %d pod(s) from %s", len(pods.Evictable), name)
		return nil
	}
	h, err := c.helper(ctx, timeout)
	if err != nil {
		return err
	}
	if err := drain.RunNodeDrain(h, name); err != nil {
		return fmt.Errorf("drain %s: %w", name, err)
	}
	return nil
}

type PodRef struct {
	Namespace string
	Name      string
	Reason    string
}

func (p PodRef) String() string { return p.Namespace + "/" + p.Name }

type DrainPreviewResult struct {
	Evictable []PodRef
	Skipped   []PodRef
	// Blocked lists pods whose PodDisruptionBudget currently allows zero
	// disruptions. kubectl's own --dry-run does not catch these because PDBs
	// are only enforced at eviction time, and a drain that trips one hangs
	// until the timeout.
	Blocked []PodRef
	// Handovers are PDB-protected pods whose owner moves them out of the way
	// once the node is cordoned -- a CloudNativePG primary with a healthy
	// replica elsewhere. The PDB reads 0 now and will not after the cordon.
	Handovers []PodRef
	// Refusals are the filter errors GetPodsForDeletion returns -- an
	// unmanaged pod, a DaemonSet pod we are not ignoring. RunNodeDrain fails
	// on exactly these, so they are blockers, not advisories.
	Refusals []string
	Warnings []string
}

// DrainPreview is preflight's read-only look at what a drain would do.
func (c *Client) DrainPreview(ctx context.Context, name string, timeout time.Duration) (*DrainPreviewResult, error) {
	h, err := c.helper(ctx, timeout)
	if err != nil {
		return nil, err
	}
	list, errs := h.GetPodsForDeletion(name)
	res := &DrainPreviewResult{}
	for _, e := range errs {
		res.Refusals = append(res.Refusals, e.Error())
	}
	if list == nil {
		if len(errs) > 0 {
			return res, fmt.Errorf("cannot enumerate pods on %s: %v", name, errs[0])
		}
		return res, nil
	}
	for _, p := range list.Pods() {
		res.Evictable = append(res.Evictable, PodRef{Namespace: p.Namespace, Name: p.Name})
	}
	blocked, handovers, err := c.pdbBlockers(ctx, name, list.Pods())
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("PDB check incomplete: %v", err))
	}
	res.Blocked = blocked
	res.Handovers = handovers
	sort.Slice(res.Evictable, func(i, j int) bool { return res.Evictable[i].String() < res.Evictable[j].String() })
	return res, nil
}

// pdbBlockers finds pods covered by a PDB that currently permits no
// disruptions. Those are exactly the ones that would make drain spin -- unless
// the cordon itself changes the answer, which is what handovers are.
func (c *Client) pdbBlockers(ctx context.Context, node string, pods []corev1.Pod) (blocked, handovers []PodRef, err error) {
	namespaces := map[string]bool{}
	for _, p := range pods {
		namespaces[p.Namespace] = true
	}
	cs, err := c.cs()
	if err != nil {
		return nil, nil, err
	}
	byNS := map[string][]policyv1.PodDisruptionBudget{}
	for ns := range namespaces {
		l, err := cs.PolicyV1().PodDisruptionBudgets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			if apierrors.IsForbidden(err) || apierrors.IsNotFound(err) {
				continue
			}
			return nil, nil, err
		}
		byNS[ns] = l.Items
	}
	for _, p := range pods {
		for i := range byNS[p.Namespace] {
			pdb := &byNS[p.Namespace][i]
			sel, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
			if err != nil {
				continue
			}
			if sel.Empty() || !sel.Matches(labels.Set(p.Labels)) {
				continue
			}
			if pdb.Status.DisruptionsAllowed > 0 {
				continue
			}
			if to, ok := c.cnpgSwitchoverTarget(ctx, node, &p); ok {
				handovers = append(handovers, PodRef{
					Namespace: p.Namespace,
					Name:      p.Name,
					Reason: fmt.Sprintf("CloudNativePG primary; the cordon makes the operator switch over to %s, "+
						"after which pdb %s no longer covers it", to, pdb.Name),
				})
				continue
			}
			blocked = append(blocked, PodRef{
				Namespace: p.Namespace,
				Name:      p.Name,
				Reason: fmt.Sprintf("pdb %s allows %d disruptions (%d/%d healthy)",
					pdb.Name, pdb.Status.DisruptionsAllowed,
					pdb.Status.CurrentHealthy, pdb.Status.DesiredHealthy),
			})
		}
	}
	return blocked, handovers, nil
}

// cnpgSwitchoverTarget reports whether pod is a CloudNativePG primary that the
// operator will move off node by itself.
//
// CNPG guards its primary with a PDB selecting instanceRole=primary, which by
// construction always allows 0 disruptions. It does not need to: when the
// primary's node turns unschedulable, the operator switches over to a replica
// on a schedulable node (internal/controller/replicas.go, "Primary is running
// on an unschedulable node, will try switching over"), the old primary is
// relabelled replica, the PDB stops selecting it, and the drain -- which keeps
// retrying refused evictions until its timeout -- goes through.
//
// That only holds with somewhere to switch to, so this demands a Ready replica
// of the same cluster on another node that is itself schedulable. Anything
// less stays a blocker.
func (c *Client) cnpgSwitchoverTarget(ctx context.Context, node string, p *corev1.Pod) (string, bool) {
	cluster := p.Labels["cnpg.io/cluster"]
	if cluster == "" || p.Labels["cnpg.io/instanceRole"] != "primary" {
		return "", false
	}
	cs, err := c.cs()
	if err != nil {
		return "", false
	}
	peers, err := cs.CoreV1().Pods(p.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "cnpg.io/cluster=" + cluster + ",cnpg.io/instanceRole=replica",
	})
	if err != nil {
		return "", false
	}
	for _, r := range peers.Items {
		if r.Spec.NodeName == "" || r.Spec.NodeName == node || !podReady(&r) {
			continue
		}
		n, err := cs.CoreV1().Nodes().Get(ctx, r.Spec.NodeName, metav1.GetOptions{})
		if err != nil || n.Spec.Unschedulable {
			continue
		}
		return fmt.Sprintf("%s on %s", r.Name, r.Spec.NodeName), true
	}
	return "", false
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// EphemeralPrometheus lists prometheus-operator Prometheus resources whose
// TSDB has no volume (spec.storage unset), which means an emptyDir.
//
// Replicas of such a Prometheus each hold their own copy of the data and do
// not replicate it. Moving one pod loses that pod's history while the other
// still has it; a rolling operation moves every replica in turn, and then the
// history is gone -- which is how 2026-09-28's rolling upgrade lost it.
// Alertmanager is not listed: its replicas gossip silences to each other, so
// moving them one at a time loses nothing.
//
// A cluster without the CRD simply has nothing to report.
func (c *Client) EphemeralPrometheus(ctx context.Context) ([]string, error) {
	cs, err := c.cs()
	if err != nil {
		return nil, err
	}
	raw, err := cs.CoreV1().RESTClient().Get().AbsPath("/apis/monitoring.coreos.com/v1/prometheuses").DoRaw(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return ephemeralFromList(raw)
}

func ephemeralFromList(raw []byte) ([]string, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Storage json.RawMessage `json:"storage"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decode prometheuses: %w", err)
	}
	var out []string
	for _, p := range list.Items {
		st := strings.TrimSpace(string(p.Spec.Storage))
		if st == "" || st == "null" || st == "{}" {
			out = append(out, p.Metadata.Namespace+"/"+p.Metadata.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// WaitReady blocks until the node reports Ready, or the timeout expires. A
// node that has not reappeared in the API at all counts as not-ready rather
// than an error, because after a reboot the object is there but stale.
func (c *Client) WaitReady(ctx context.Context, name string, timeout time.Duration) error {
	cs, err := c.cs()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var last string
	for {
		n, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			ready := false
			for _, cond := range n.Status.Conditions {
				if cond.Type == corev1.NodeReady {
					ready = cond.Status == corev1.ConditionTrue
					last = string(cond.Status) + "/" + cond.Reason
				}
			}
			if ready {
				return nil
			}
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("node %s not Ready after %s (last: %s)", name, timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// AllReady reports whether every node is Ready, and names those that are not.
func (c *Client) AllReady(ctx context.Context) (bool, []string, error) {
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return false, nil, err
	}
	var bad []string
	for _, n := range nodes {
		if !n.Ready {
			bad = append(bad, n.Name+"(NotReady)")
		} else if n.Unschedulable {
			bad = append(bad, n.Name+"(cordoned)")
		}
	}
	return len(bad) == 0, bad, nil
}

// Cordoned lists nodes left unschedulable -- doctor's leftover detector.
func (c *Client) Cordoned(ctx context.Context) ([]string, error) {
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range nodes {
		if n.Unschedulable {
			out = append(out, n.Name)
		}
	}
	return out, nil
}

// logWriter funnels the drain helper's own output into our transcript.
type logWriter struct {
	l     *plog.Logger
	isErr bool
}

func (w logWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line == "" {
			continue
		}
		if w.isErr {
			w.l.Warnf("%s", line)
		} else {
			w.l.Debugf("%s", line)
		}
	}
	return len(p), nil
}

var _ io.Writer = logWriter{}

// ProxyRequest performs an HTTP call to a ClusterIP service through the API
// server's service proxy subresource.
//
// Alertmanager and discord-bridge have no LoadBalancer VIP, so something has to
// bridge into the cluster network. `kubectl port-forward` would work but means
// binding a local port and keeping a tunnel goroutine alive across a node
// shutdown; the proxy subresource is a plain request over the connection we
// already have.
func (c *Client) ProxyRequest(ctx context.Context, method, namespace, service string, port int, path string, body []byte) ([]byte, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	cs, err := c.cs()
	if err != nil {
		return nil, err
	}
	abs := fmt.Sprintf("/api/v1/namespaces/%s/services/%s:%d/proxy%s", namespace, service, port, path)
	req := cs.CoreV1().RESTClient().Verb(method).AbsPath(abs)
	if body != nil {
		req = req.Body(body).SetHeader("Content-Type", "application/json")
	}
	out, err := req.DoRaw(ctx)
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "..."
		}
		return out, fmt.Errorf("%s %s via service proxy: %w (%s)", method, abs, err, snippet)
	}
	return out, nil
}
