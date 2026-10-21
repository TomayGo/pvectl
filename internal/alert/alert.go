// Package alert talks to Alertmanager and to the Discord bridge.
//
// Both are ClusterIP-only, so every request goes through the API server's
// service proxy (see k8s.Client.ProxyRequest) rather than a port-forward.
//
// The bridge is not a free-form message endpoint: it is an Alertmanager webhook
// receiver that reads body.alerts[].{status,labels,annotations} and renders a
// Discord embed from them. So a maintenance notice has to be shaped like an
// alert -- firing when the window opens, resolved when it closes.
package alert

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	plog "github.com/TomayGo/pvectl/internal/log"
)

// ServiceProxy is the subset of the k8s client this package needs.
type ServiceProxy interface {
	ProxyRequest(ctx context.Context, method, namespace, service string, port int, path string, body []byte) ([]byte, error)
}

type Endpoint struct {
	Namespace string
	Service   string
	Port      int
}

type Client struct {
	proxy  ServiceProxy
	AM     Endpoint
	Bridge Endpoint
	log    *plog.Logger
	dryRun bool
}

func New(proxy ServiceProxy, am, bridge Endpoint, lg *plog.Logger, dryRun bool) *Client {
	return &Client{proxy: proxy, AM: am, Bridge: bridge, log: lg, dryRun: dryRun}
}

// ---------- silences ----------

type Matcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual bool   `json:"isEqual"`
}

type Silence struct {
	ID        string    `json:"id,omitempty"`
	Matchers  []Matcher `json:"matchers"`
	StartsAt  time.Time `json:"startsAt"`
	EndsAt    time.Time `json:"endsAt"`
	CreatedBy string    `json:"createdBy"`
	Comment   string    `json:"comment"`
	Status    struct {
		State string `json:"state"`
	} `json:"status,omitempty"`
}

// CreatedBy tags every silence pvectl makes, so `doctor` can spot ones left
// behind by an interrupted run.
const CreatedBy = "pvectl"

// NodeMatchers builds the label matchers that cover one Proxmox node and the
// Talos control-plane VM running on it. The cluster's Prometheus carries a
// `node` label holding both pve-N and talos-cp-N, which makes this exact
// rather than a guess.
func NodeMatchers(pveNode, talosName string, ips []string) []Matcher {
	m := []Matcher{{
		Name:    "node",
		Value:   fmt.Sprintf("%s|%s", pveNode, talosName),
		IsRegex: true,
		IsEqual: true,
	}}
	var clean []string
	for _, ip := range ips {
		if ip != "" {
			clean = append(clean, strings.ReplaceAll(ip, ".", `\.`))
		}
	}
	if len(clean) > 0 {
		m = append(m, Matcher{
			Name:    "instance",
			Value:   fmt.Sprintf("(%s)(:[0-9]+)?", strings.Join(clean, "|")),
			IsRegex: true,
			IsEqual: true,
		})
	}
	return m
}

// CreateSilence posts one silence per matcher. Alertmanager ANDs the matchers
// inside a single silence, but a node's alerts are labelled inconsistently --
// some carry `node`, some only `instance` -- so ANDing them would silence
// almost nothing. One silence per matcher is the OR we actually want.
func (c *Client) CreateSilence(ctx context.Context, matchers []Matcher, comment string, dur time.Duration) ([]string, error) {
	var ids []string
	for _, m := range matchers {
		s := Silence{
			Matchers:  []Matcher{m},
			StartsAt:  time.Now().Add(-1 * time.Minute),
			EndsAt:    time.Now().Add(dur),
			CreatedBy: CreatedBy,
			Comment:   comment,
		}
		if c.dryRun {
			c.log.Infof("(dry-run) would silence %s%s%s for %s", m.Name, matchOp(m), m.Value, dur)
			continue
		}
		body, err := json.Marshal(s)
		if err != nil {
			return ids, err
		}
		raw, err := c.proxy.ProxyRequest(ctx, "POST", c.AM.Namespace, c.AM.Service, c.AM.Port, "/api/v2/silences", body)
		if err != nil {
			return ids, fmt.Errorf("create silence: %w", err)
		}
		var resp struct {
			SilenceID string `json:"silenceID"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return ids, fmt.Errorf("create silence: decode response %q: %w", string(raw), err)
		}
		c.log.Infof("silence %s created for %s%s%s", resp.SilenceID, m.Name, matchOp(m), m.Value)
		ids = append(ids, resp.SilenceID)
	}
	return ids, nil
}

func matchOp(m Matcher) string {
	if m.IsRegex {
		return "=~"
	}
	return "="
}

func (c *Client) DeleteSilence(ctx context.Context, id string) error {
	if c.dryRun {
		c.log.Infof("(dry-run) would expire silence %s", id)
		return nil
	}
	if _, err := c.proxy.ProxyRequest(ctx, "DELETE", c.AM.Namespace, c.AM.Service, c.AM.Port, "/api/v2/silence/"+id, nil); err != nil {
		// Alertmanager drops expired silences after its retention (5 days by
		// default), and DELETE on a dropped one is a 404. A window held open
		// longer than that -- a node brought back by hand, say -- would then
		// never close. A 404 alone could also mean a missing Service, so ask
		// Alertmanager whether the silence is still there before letting it go.
		if all, lerr := c.ListSilences(ctx); lerr == nil && !activeSilence(all, id) {
			c.log.Infof("silence %s already expired", id)
			return nil
		}
		return fmt.Errorf("expire silence %s: %w", id, err)
	}
	c.log.Infof("silence %s expired", id)
	return nil
}

func activeSilence(all []Silence, id string) bool {
	for _, s := range all {
		if s.ID == id {
			return s.Status.State != "expired"
		}
	}
	return false
}

// ListSilences returns active silences. doctor filters these by CreatedBy.
func (c *Client) ListSilences(ctx context.Context) ([]Silence, error) {
	raw, err := c.proxy.ProxyRequest(ctx, "GET", c.AM.Namespace, c.AM.Service, c.AM.Port, "/api/v2/silences", nil)
	if err != nil {
		return nil, err
	}
	var out []Silence
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode silences: %w", err)
	}
	return out, nil
}

// OwnSilences filters to silences pvectl created that are still active.
func (c *Client) OwnSilences(ctx context.Context) ([]Silence, error) {
	all, err := c.ListSilences(ctx)
	if err != nil {
		return nil, err
	}
	var out []Silence
	for _, s := range all {
		if s.CreatedBy == CreatedBy && s.Status.State != "expired" {
			out = append(out, s)
		}
	}
	return out, nil
}

// ---------- discord notification ----------

type webhookAlert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    string            `json:"startsAt,omitempty"`
}

type webhookPayload struct {
	Version  string         `json:"version"`
	Receiver string         `json:"receiver"`
	Status   string         `json:"status"`
	Alerts   []webhookAlert `json:"alerts"`
}

// Notify posts a synthetic Alertmanager webhook payload to the bridge.
//
// status must be "firing" or "resolved": the bridge renders the first with a
// red/orange embed and the configured @mention, the second with a green
// "RESOLVED" embed and no mention. Maintenance therefore opens with firing and
// closes with resolved, which is also what makes the pair readable in Discord.
func (c *Client) Notify(ctx context.Context, status, alertname, summary, description string, labels map[string]string) error {
	if c.Bridge.Service == "" {
		return nil
	}
	l := map[string]string{"alertname": alertname, "severity": "info"}
	for k, v := range labels {
		l[k] = v
	}
	p := webhookPayload{
		Version:  "4",
		Receiver: "pvectl",
		Status:   status,
		Alerts: []webhookAlert{{
			Status:      status,
			Labels:      l,
			Annotations: map[string]string{"summary": summary, "description": description},
			StartsAt:    time.Now().Format(time.RFC3339),
		}},
	}
	if c.dryRun {
		c.log.Infof("(dry-run) would notify discord: [%s] %s -- %s", status, alertname, summary)
		return nil
	}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if _, err := c.proxy.ProxyRequest(ctx, "POST", c.Bridge.Namespace, c.Bridge.Service, c.Bridge.Port, "/", body); err != nil {
		// A missed notification must never abort a maintenance step.
		return fmt.Errorf("notify discord: %w", err)
	}
	c.log.Infof("discord notified: [%s] %s", status, alertname)
	return nil
}
