package pve_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	plog "github.com/TomayGo/pvectl/internal/log"
	"github.com/TomayGo/pvectl/internal/pve"
	"github.com/TomayGo/pvectl/internal/pve/pvetest"
)

// toServer sends every request to one httptest.Server, whatever endpoint the
// client addressed, and tells the server which endpoint that was.
type toServer struct {
	srv  *httptest.Server
	seen *[]string
}

func (r toServer) RoundTrip(req *http.Request) (*http.Response, error) {
	to, _ := url.Parse(r.srv.URL)
	out := req.Clone(req.Context())
	*r.seen = append(*r.seen, req.URL.String()+" "+req.Header.Get("Authorization"))
	out.Header.Set("X-Pvetest-Endpoint", req.URL.Hostname())
	out.URL.Scheme, out.URL.Host, out.Host = to.Scheme, to.Host, to.Host
	return r.srv.Client().Transport.RoundTrip(out)
}

// The client can be pointed at an httptest.Server: real TLS, real sockets,
// the request and reply going over the wire as they do to pveproxy.
func TestClientAgainstHTTPTestServer(t *testing.T) {
	fake := pvetest.New(t)
	fake.JSON("GET", "/cluster/ceph/flags", `[{"name":"noout","value":1},{"name":"pause","value":0}]`)
	fake.Static("PUT", "/cluster/ceph/flags/noout", nil)
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()

	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("PVEAPIToken=root@pam!pvectl=s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen []string
	c, err := pve.New([]string{"192.0.2.1"}, token, plog.Discard(), pve.WithTransport(toServer{srv, &seen}))
	if err != nil {
		t.Fatal(err)
	}
	flags, err := c.CephFlags(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !flags["noout"] || flags["pause"] {
		t.Errorf("flags decoded as %v", flags)
	}
	if err := c.SetCephFlag(context.Background(), "noout", false); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"https://192.0.2.1:8006/api2/json/cluster/ceph/flags PVEAPIToken=root@pam!pvectl=s3cret",
		"https://192.0.2.1:8006/api2/json/cluster/ceph/flags/noout PVEAPIToken=root@pam!pvectl=s3cret",
	}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("requests sent:\n got %q\nwant %q", seen, want)
	}
	if got, want := fake.Writes(), []string{"PUT /cluster/ceph/flags/noout value=0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("writes %q, want %q", got, want)
	}
	if got := fake.Requests()[0].Endpoint; got != "192.0.2.1" {
		t.Errorf("endpoint seen by the server: %q", got)
	}
}

// A node that cannot be reached is skipped, and the one that answered is
// asked first from then on.
func TestClientFailsOverAndRemembers(t *testing.T) {
	fake := pvetest.New(t)
	fake.JSON("GET", "/cluster/ceph/flags", `[]`)
	fake.SetDown("pve-1", true)
	c := fake.Client("pve-1", "pve-2", "pve-3")
	for i := 0; i < 2; i++ {
		if _, err := c.CephFlags(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var asked []string
	for _, r := range fake.Requests() {
		asked = append(asked, r.Endpoint)
	}
	if want := []string{"pve-1", "pve-2", "pve-2"}; !reflect.DeepEqual(asked, want) {
		t.Errorf("endpoints asked %v, want %v", asked, want)
	}
}

// An answer from the API -- even "no" -- is the same from every node, so it
// is returned rather than retried elsewhere.
func TestClientDoesNotRetryAnAPIError(t *testing.T) {
	fake := pvetest.New(t)
	fake.Handle("GET", "/cluster/ceph/flags", func(pvetest.Request) (any, error) {
		return nil, &pvetest.Error{Status: 403, Message: "Permission check failed"}
	})
	_, err := fake.Client("pve-1", "pve-2").CephFlags(context.Background())
	if err == nil || err.Error() != "GET /cluster/ceph/flags: HTTP 403: Permission check failed" {
		t.Errorf("error %v", err)
	}
	if n := len(fake.Requests()); n != 1 {
		t.Errorf("%d request(s), want 1", n)
	}
}

func TestClientReportsEveryEndpointFailing(t *testing.T) {
	fake := pvetest.New(t)
	fake.SetDown("pve-1", true)
	fake.SetDown("pve-2", true)
	_, err := fake.Client("pve-1", "pve-2").CephFlags(context.Background())
	if err == nil || !strings.HasPrefix(err.Error(), "all API endpoints failed ([pve-1 pve-2]): ") {
		t.Errorf("error %v", err)
	}
}

// A command aimed at a node must not go through that node.
func TestExcludedClientNeverAsksTheExcludedNode(t *testing.T) {
	fake := pvetest.New(t)
	fake.Static("POST", "/nodes/pve-1/status", nil)
	c := fake.Client("pve-1", "pve-2", "pve-3").Excluded("pve-1")
	if got := c.Endpoints(); !reflect.DeepEqual(got, []string{"pve-2", "pve-3"}) {
		t.Fatalf("endpoints %v", got)
	}
	if err := c.NodeCommand(context.Background(), "pve-1", "reboot"); err != nil {
		t.Fatal(err)
	}
	reqs := fake.Requests()
	if len(reqs) != 1 || reqs[0].Endpoint != "pve-2" || reqs[0].String() != "POST /nodes/pve-1/status command=reboot" {
		t.Errorf("requests %v", reqs)
	}
	none := fake.Client("pve-1").Excluded("pve-1")
	if err := none.NodeCommand(context.Background(), "pve-1", "reboot"); err == nil ||
		err.Error() != "no usable API endpoint remains (all candidates were excluded)" {
		t.Errorf("error %v", err)
	}
}

// --dry-run reads but never writes.
func TestDryRunClientSendsNoWrites(t *testing.T) {
	fake := pvetest.New(t)
	fake.JSON("GET", "/cluster/ceph/flags", `[]`)
	c := fake.Client()
	c.DryRun = true
	if _, err := c.CephFlags(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.SetCephFlag(context.Background(), "noout", true); err != nil {
		t.Fatal(err)
	}
	upid, err := c.GuestShutdown(context.Background(), "pve-1", "qemu", 103, time.Minute, false)
	if err != nil || upid != "" {
		t.Errorf("dry-run shutdown gave %q, %v", upid, err)
	}
	if got := fake.Calls(); !reflect.DeepEqual(got, []string{"GET /cluster/ceph/flags"}) {
		t.Errorf("requests %v", got)
	}
}

const upid = "UPID:pve-1:0001:0002:0003:qmshutdown:103:root@pam:"

func taskPath() string { return "/nodes/pve-1/tasks/" + upid + "/status" }

// WaitTask asks every 2 s until the task is no longer running.
func TestWaitTaskPollsUntilTheTaskEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := pvetest.New(t)
		fake.Sequence("GET", taskPath(), `{"status":"running"}`, `{"status":"running"}`, `{"status":"stopped","exitstatus":"OK"}`)
		start := time.Now()
		if err := fake.Client().WaitTask(context.Background(), "pve-1", upid, time.Minute); err != nil {
			t.Fatal(err)
		}
		if got := time.Since(start); got != 4*time.Second {
			t.Errorf("waited %s, want 4s (two polls 2s apart)", got)
		}
		if n := len(fake.Requests()); n != 3 {
			t.Errorf("%d status request(s), want 3", n)
		}
	})
}

func TestWaitTaskReportsAFailedTask(t *testing.T) {
	fake := pvetest.New(t)
	fake.JSON("GET", taskPath(), `{"status":"stopped","exitstatus":"VM quit/powerdown failed - got timeout"}`)
	err := fake.Client().WaitTask(context.Background(), "pve-1", upid, time.Minute)
	if want := `task ` + upid + ` finished with "VM quit/powerdown failed - got timeout"`; err == nil || err.Error() != want {
		t.Errorf("error %v, want %s", err, want)
	}
}

// The deadline is checked after a poll, so a task is always asked about at
// least once and the wait overshoots by at most one interval.
func TestWaitTaskTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := pvetest.New(t)
		fake.JSON("GET", taskPath(), `{"status":"running"}`)
		start := time.Now()
		err := fake.Client().WaitTask(context.Background(), "pve-1", upid, 5*time.Second)
		if want := "task " + upid + " still running after 5s"; err == nil || err.Error() != want {
			t.Errorf("error %v, want %s", err, want)
		}
		if got := time.Since(start); got != 6*time.Second {
			t.Errorf("gave up after %s, want 6s", got)
		}
	})
}

func TestWaitTaskStopsWhenCancelledOrFailing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := pvetest.New(t)
		fake.JSON("GET", taskPath(), `{"status":"running"}`)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := fake.Client().WaitTask(ctx, "pve-1", upid, time.Minute); err != context.DeadlineExceeded {
			t.Errorf("error %v, want the context's", err)
		}
	})
	fake := pvetest.New(t)
	fake.Handle("GET", taskPath(), func(pvetest.Request) (any, error) {
		return nil, &pvetest.Error{Status: 500, Message: "boom"}
	})
	if err := fake.Client().WaitTask(context.Background(), "pve-1", upid, time.Minute); err == nil || !strings.Contains(err.Error(), "HTTP 500: boom") {
		t.Errorf("a failing status read must end the wait: %v", err)
	}
	// What --dry-run produces: no task to wait for.
	if err := pvetest.New(t).Client().WaitTask(context.Background(), "pve-1", "", time.Minute); err != nil {
		t.Errorf("empty upid: %v", err)
	}
}
