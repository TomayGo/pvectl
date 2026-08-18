// Package k8s wraps the Kubernetes side of a node maintenance: cordon, drain,
// uncordon and readiness waits against the Talos control-plane VM that lives on
// the Proxmox node being stopped.
//
// It uses client-go plus kubectl's own drain helper so the eviction semantics
// match `kubectl drain` exactly, rather than being re-implemented and drifting.
package k8s

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
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
	cs     kubernetes.Interface
	log    *plog.Logger
	dryRun bool
}

func New(kubeconfig string, lg *plog.Logger, dryRun bool) (*Client, error) {
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("load kubeconfig %s: %w", kubeconfig, err)
	}
	// The API server sits behind the same LAN the node is on; a hung request
	// must not stall a maintenance window.
	cfg.Timeout = 30 * time.Second
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build kubernetes client: %w", err)
	}
	return &Client{cs: cs, log: lg, dryRun: dryRun}, nil
}

type NodeInfo struct {
	Name          string
	Ready         bool
	Unschedulable bool
	Reason        string
	Version       string
}

func (c *Client) Nodes(ctx context.Context) ([]NodeInfo, error) {
	list, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
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
	n, err := c.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get node %s: %w", name, err)
	}
	return n, nil
}

func (c *Client) helper(ctx context.Context, timeout time.Duration) *drain.Helper {
	return &drain.Helper{
		Ctx:                 ctx,
		Client:              c.cs,
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
	}
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
	if err, patchErr := h.PatchOrReplaceWithContext(ctx, c.cs, false); err != nil || patchErr != nil {
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
	h := c.helper(ctx, timeout)
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
	Blocked  []PodRef
	Warnings []string
}

// DrainPreview is preflight's read-only look at what a drain would do.
func (c *Client) DrainPreview(ctx context.Context, name string, timeout time.Duration) (*DrainPreviewResult, error) {
	h := c.helper(ctx, timeout)
	list, errs := h.GetPodsForDeletion(name)
	res := &DrainPreviewResult{}
	for _, e := range errs {
		res.Warnings = append(res.Warnings, e.Error())
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
	blocked, err := c.pdbBlockers(ctx, list.Pods())
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("PDB check incomplete: %v", err))
	}
	res.Blocked = blocked
	sort.Slice(res.Evictable, func(i, j int) bool { return res.Evictable[i].String() < res.Evictable[j].String() })
	return res, nil
}

// pdbBlockers finds pods covered by a PDB that currently permits no
// disruptions. Those are exactly the ones that would make drain spin.
func (c *Client) pdbBlockers(ctx context.Context, pods []corev1.Pod) ([]PodRef, error) {
	namespaces := map[string]bool{}
	for _, p := range pods {
		namespaces[p.Namespace] = true
	}
	byNS := map[string][]policyv1.PodDisruptionBudget{}
	for ns := range namespaces {
		l, err := c.cs.PolicyV1().PodDisruptionBudgets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			if apierrors.IsForbidden(err) || apierrors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		byNS[ns] = l.Items
	}
	var out []PodRef
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
			if pdb.Status.DisruptionsAllowed <= 0 {
				out = append(out, PodRef{
					Namespace: p.Namespace,
					Name:      p.Name,
					Reason: fmt.Sprintf("pdb %s allows %d disruptions (%d/%d healthy)",
						pdb.Name, pdb.Status.DisruptionsAllowed,
						pdb.Status.CurrentHealthy, pdb.Status.DesiredHealthy),
				})
			}
		}
	}
	return out, nil
}

// WaitReady blocks until the node reports Ready, or the timeout expires. A
// node that has not reappeared in the API at all counts as not-ready rather
// than an error, because after a reboot the object is there but stale.
func (c *Client) WaitReady(ctx context.Context, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last string
	for {
		n, err := c.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
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
	abs := fmt.Sprintf("/api/v1/namespaces/%s/services/%s:%d/proxy%s", namespace, service, port, path)
	req := c.cs.CoreV1().RESTClient().Verb(method).AbsPath(abs)
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
