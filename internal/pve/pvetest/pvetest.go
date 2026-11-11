// Package pvetest is an in-memory stand-in for the cluster's pveproxy, for
// tests of the code that drives Proxmox through pve.Client.
//
// A Fake answers the API paths a test registers and fails the test on any
// other request, so a characterization test also pins down which calls a code
// path makes. It serves requests in-process (no socket, no goroutine), which
// keeps it usable inside a testing/synctest bubble where the polling loops run
// on a fake clock; it is also an http.Handler, for the tests that want a real
// httptest.Server in between.
package pvetest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	plog "github.com/TomayGo/pvectl/internal/log"
	"github.com/TomayGo/pvectl/internal/pve"
	"github.com/TomayGo/pvectl/internal/remote"
)

// Request is one API call as the fake saw it.
type Request struct {
	Method string
	// Path is the API path without the /api2/json prefix.
	Path string
	// Form holds the query parameters of a GET and the body of a POST or PUT.
	Form url.Values
	// Endpoint is the host the client addressed (an entry of its endpoint
	// list), which is how a test checks that a node was not asked to stop
	// itself.
	Endpoint string
}

func (r Request) String() string {
	s := r.Method + " " + r.Path
	if len(r.Form) > 0 {
		s += " " + r.Form.Encode()
	}
	return s
}

// Handler answers one request: the value for the reply's "data" field, or an
// error. An *Error picks the HTTP status; any other error is a 500.
type Handler func(Request) (data any, err error)

// Error is an API failure with a chosen status code.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

// Fake is the fake pveproxy. The zero value is not usable; call New.
type Fake struct {
	t testing.TB

	mu       sync.Mutex
	handlers map[string]Handler
	requests []Request
	// Down lists endpoints that refuse every request, as a powered-off node
	// would.
	down map[string]bool

	// Runner, when set, is what the clients built from this fake run
	// ha-manager through.
	Runner remote.Runner
}

func New(t testing.TB) *Fake {
	return &Fake{t: t, handlers: map[string]Handler{}, down: map[string]bool{}}
}

// Handle registers the answer for one method and path, replacing any earlier
// one.
func (f *Fake) Handle(method, path string, h Handler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method+" "+path] = h
}

// Static registers a fixed reply.
func (f *Fake) Static(method, path string, data any) {
	f.Handle(method, path, func(Request) (any, error) { return data, nil })
}

// JSON registers a fixed reply given as the JSON text of the "data" field,
// for replies copied from a real cluster.
func (f *Fake) JSON(method, path, data string) {
	f.Static(method, path, json.RawMessage(data))
}

// Sequence registers replies that are handed out one per request; the last
// one repeats. Each is JSON text of the "data" field.
func (f *Fake) Sequence(method, path string, replies ...string) {
	i := 0
	f.Handle(method, path, func(Request) (any, error) {
		r := replies[i]
		if i < len(replies)-1 {
			i++
		}
		return json.RawMessage(r), nil
	})
}

// SetDown makes an endpoint refuse (or accept again) every request.
func (f *Fake) SetDown(endpoint string, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down[endpoint] = down
}

// Requests returns every request seen so far, in order.
func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

// Calls returns the requests as strings ("PUT /cluster/ceph/flags/noout
// value=1"), optionally only those with the given method(s).
func (f *Fake) Calls(methods ...string) []string {
	var out []string
	for _, r := range f.Requests() {
		if len(methods) > 0 && !containsString(methods, r.Method) {
			continue
		}
		out = append(out, r.String())
	}
	return out
}

// Writes returns the POST and PUT requests: everything that changed the fake
// cluster.
func (f *Fake) Writes() []string { return f.Calls(http.MethodPost, http.MethodPut) }

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// Client returns a pve.Client that talks to the fake. The endpoints are
// arbitrary names; with none given there is a single one, "pve-fake".
func (f *Fake) Client(endpoints ...string) *pve.Client {
	return f.ClientWith(plog.Discard(), endpoints...)
}

// ClientWith is Client with the logger chosen by the test.
func (f *Fake) ClientWith(lg *plog.Logger, endpoints ...string) *pve.Client {
	f.t.Helper()
	if len(endpoints) == 0 {
		endpoints = []string{"pve-fake"}
	}
	token := filepath.Join(f.t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("root@pam!pvectl=not-a-secret\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	opts := []pve.Option{pve.WithTransport(f)}
	if f.Runner != nil {
		opts = append(opts, pve.WithRunner(f.Runner))
	}
	c, err := pve.New(endpoints, token, lg, opts...)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// RoundTrip serves the request in-process.
func (f *Fake) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	f.serve(rec, req, req.URL.Hostname())
	return rec.Result(), nil
}

// ServeHTTP lets the fake sit behind an httptest.Server. The endpoint the
// client meant is then taken from the X-Pvetest-Endpoint header, if the test's
// transport sets it.
func (f *Fake) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	f.serve(w, req, req.Header.Get("X-Pvetest-Endpoint"))
}

func (f *Fake) serve(w http.ResponseWriter, req *http.Request, endpoint string) {
	r := Request{Method: req.Method, Path: strings.TrimPrefix(req.URL.Path, "/api2/json"), Endpoint: endpoint}
	r.Form = req.URL.Query()
	if req.Body != nil {
		if body, _ := io.ReadAll(req.Body); len(body) > 0 {
			if form, err := url.ParseQuery(string(body)); err == nil {
				r.Form = form
			}
		}
	}
	f.mu.Lock()
	f.requests = append(f.requests, r)
	h := f.handlers[r.Method+" "+r.Path]
	down := f.down[endpoint]
	f.mu.Unlock()

	if down {
		// 595 is pveproxy's own "could not connect"; the client treats it as
		// never delivered and moves on to the next endpoint.
		http.Error(w, "connection refused", 595)
		return
	}
	if h == nil {
		f.t.Errorf("pvetest: unexpected request %s", r)
		http.Error(w, "pvetest: no handler for "+r.String(), http.StatusNotImplemented)
		return
	}
	data, err := h(r)
	if err != nil {
		status := http.StatusInternalServerError
		msg := err.Error()
		if pe, ok := err.(*Error); ok {
			status, msg = pe.Status, pe.Message
		}
		http.Error(w, msg, status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}
