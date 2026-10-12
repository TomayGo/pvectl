// Package pve is the Proxmox VE API client.
//
// Everything pvectl does on the Proxmox side goes through here -- no ssh, no
// shelling out to ceph/ha-manager. That is what lets the binary be static and
// lets pve-2 drive a shutdown of pve-1 without needing anything installed on
// pve-1 beyond a running pveproxy.
package pve

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	plog "github.com/TomayGo/pvectl/internal/log"
)

// RootCAPath is the cluster CA that signs every node's pveproxy certificate.
// The certs carry the LAN IP in their SANs, so pinning this CA gives real
// verification even though we connect by IP.
const RootCAPath = "/etc/pve/pve-root-ca.pem"

type Client struct {
	// endpoints is the ordered candidate list. Excluded() removes the node
	// being acted on: POST /nodes/pve-1/status --command shutdown sent *to*
	// pve-1 tears down the connection before the reply arrives.
	endpoints []string
	tokenID   string
	tokenSec  string
	http      *http.Client
	log       *plog.Logger
	DryRun    bool
	// lastGood remembers the endpoint that worked, so a run does not re-probe
	// a dead node on every call.
	lastGood string
}

type Option func(*Client)

func WithDryRun(v bool) Option { return func(c *Client) { c.DryRun = v } }

// New builds a client. tokenFile holds a single line "USER@REALM!NAME=SECRET",
// the exact string `pvesh create /access/users/.../token` prints.
func New(endpoints []string, tokenFile string, lg *plog.Logger, opts ...Option) (*Client, error) {
	id, sec, err := readToken(tokenFile)
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if pem, err := os.ReadFile(RootCAPath); err == nil {
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(pem) {
			tlsCfg.RootCAs = pool
		} else {
			return nil, fmt.Errorf("%s contains no usable certificate", RootCAPath)
		}
	} else {
		return nil, fmt.Errorf("read cluster CA %s: %w", RootCAPath, err)
	}
	c := &Client{
		endpoints: append([]string(nil), endpoints...),
		tokenID:   id,
		tokenSec:  sec,
		log:       lg,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig:     tlsCfg,
				DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				MaxIdleConnsPerHost: 4,
			},
		},
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

func readToken(path string) (id, secret string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("read API token %s: %w (create with: pvesh create /access/users/root@pam/token/pvectl --privsep 0)", path, err)
	}
	line := strings.TrimSpace(string(b))
	// Tolerate an optional "PVEAPIToken=" prefix so a value pasted from the UI
	// works as-is.
	line = strings.TrimPrefix(line, "PVEAPIToken=")
	i := strings.Index(line, "=")
	if i < 0 || !strings.Contains(line[:i], "!") {
		return "", "", fmt.Errorf("%s must contain one line of the form USER@REALM!TOKENID=SECRET", path)
	}
	return line[:i], line[i+1:], nil
}

// Excluded returns a copy of the client whose endpoint list omits the given
// LAN IPs. Callers pass the node they are about to stop.
func (c *Client) Excluded(ips ...string) *Client {
	drop := map[string]bool{}
	for _, ip := range ips {
		if ip != "" {
			drop[ip] = true
		}
	}
	var keep []string
	for _, e := range c.endpoints {
		if !drop[e] {
			keep = append(keep, e)
		}
	}
	cp := *c
	cp.endpoints = keep
	if drop[cp.lastGood] {
		cp.lastGood = ""
	}
	return &cp
}

// Endpoints exposes the current candidate list (for reporting).
func (c *Client) Endpoints() []string { return append([]string(nil), c.endpoints...) }

type apiError struct {
	Status  int
	Message string
	Path    string
	Method  string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Message)
}

// IsNotFound reports whether err came back as a 404-ish API error.
func IsNotFound(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// retryableStatus covers the codes pveproxy returns when it cannot reach the
// node that owns the resource -- 595/596 are Proxmox's own "connection
// failure"/"no route to host" codes for proxied requests.
func retryableStatus(code int) bool {
	return code == 595 || code == 596 || code == 599 || code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

// retrySafe reports whether the same request may be sent to another endpoint.
//
// A GET can always be repeated. A POST or PUT can only be repeated when we know
// it was never delivered: a dial failure, or pveproxy's own 595/596 saying it
// could not reach the node that owns the resource. A timeout or a dropped
// connection mid-request means the call may already have taken effect, and
// re-sending it turns a success into a confusing "already running" failure.
func retrySafe(method string, err error) bool {
	if method == http.MethodGet {
		return true
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status == 595 || ae.Status == 596
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return oe.Op == "dial"
	}
	return false
}

func (c *Client) do(ctx context.Context, method, path string, params url.Values, out any) error {
	if len(c.endpoints) == 0 {
		return fmt.Errorf("no usable API endpoint remains (all candidates were excluded)")
	}
	order := c.endpoints
	if c.lastGood != "" {
		order = append([]string{c.lastGood}, filterOut(c.endpoints, c.lastGood)...)
	}
	var lastErr error
	for _, ep := range order {
		err := c.doOne(ctx, ep, method, path, params, out)
		if err == nil {
			c.lastGood = ep
			return nil
		}
		var ae *apiError
		if errors.As(err, &ae) && !retryableStatus(ae.Status) {
			// A real API error (403, 404, 400...) is the same from every
			// endpoint; failing over would just repeat it.
			return err
		}
		if !retrySafe(method, err) {
			return fmt.Errorf("%s %s via %s failed after the request may have been applied; not retrying elsewhere: %w",
				method, path, ep, err)
		}
		lastErr = err
		c.log.Debugf("endpoint %s failed (%v), trying next", ep, err)
	}
	return fmt.Errorf("all API endpoints failed (%v): %w", c.endpoints, lastErr)
}

func (c *Client) doOne(ctx context.Context, ep, method, path string, params url.Values, out any) error {
	u := fmt.Sprintf("https://%s:8006/api2/json%s", ep, path)
	var body io.Reader
	if params != nil && (method == http.MethodPost || method == http.MethodPut) {
		body = strings.NewReader(params.Encode())
	} else if params != nil && len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.tokenID+"="+c.tokenSec)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		c.log.API(method, path, 0, time.Since(start), err)
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	c.log.API(method, path, resp.StatusCode, time.Since(start), nil)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 400 {
			msg = msg[:400] + "..."
		}
		if msg == "" {
			msg = resp.Status
		}
		return &apiError{Status: resp.StatusCode, Message: msg, Path: path, Method: method}
	}
	if out == nil {
		return nil
	}
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s %s: decode envelope: %w", method, path, err)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("%s %s: decode data: %w", method, path, err)
	}
	return nil
}

// Get performs a read. Reads are always executed, even under --dry-run.
func (c *Client) Get(ctx context.Context, path string, params url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, params, out)
}

// Post performs a mutating call, which --dry-run turns into a log line.
func (c *Client) Post(ctx context.Context, path string, params url.Values, out any) error {
	if c.DryRun {
		c.log.Infof("(dry-run) would POST %s %s", path, redact(params))
		return nil
	}
	return c.do(ctx, http.MethodPost, path, params, out)
}

func (c *Client) Put(ctx context.Context, path string, params url.Values, out any) error {
	if c.DryRun {
		c.log.Infof("(dry-run) would PUT %s %s", path, redact(params))
		return nil
	}
	return c.do(ctx, http.MethodPut, path, params, out)
}

func redact(p url.Values) string {
	if len(p) == 0 {
		return ""
	}
	return p.Encode()
}

func filterOut(list []string, v string) []string {
	var out []string
	for _, s := range list {
		if s != v {
			out = append(out, s)
		}
	}
	return out
}

// Reachable reports whether an endpoint answers at all. Used by wait-up and by
// preflight's "is another node already down" check.
func (c *Client) Reachable(ctx context.Context, ep string) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var v any
	return c.doOne(ctx, ep, http.MethodGet, "/version", nil, &v) == nil
}
