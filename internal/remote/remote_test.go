package remote

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// The host key must come from pmxcfs under the node's name, never from a
// personal known_hosts: that file exists only where someone ssh'd by hand.
func TestArgsPinPVEHostKey(t *testing.T) {
	args := Args(Host{Name: "pve-1", IP: "192.0.2.79"})
	want := map[string]bool{
		"HostKeyAlias=pve-1": false,
		"UserKnownHostsFile=/etc/pve/nodes/pve-1/ssh_known_hosts": false,
		"GlobalKnownHostsFile=none":                               false,
	}
	for _, a := range args {
		if _, ok := want[a]; ok {
			want[a] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing -o %s", k)
		}
	}
	if args[len(args)-1] != "root@192.0.2.79" {
		t.Errorf("must connect by LAN IP, got %s", args[len(args)-1])
	}
}

// Quote must survive a real shell unchanged, whatever the argument holds.
func TestQuoteRoundTrips(t *testing.T) {
	args := []string{"--timeout-up=15m0s", "it's", "a b", "$HOME", "", `"x"`, "--skip-check=ceph-health"}
	out, err := exec.Command("sh", "-c", `printf '%s\n' `+Quote(args...)).Output()
	if err != nil {
		t.Fatal(err)
	}
	var want string
	for _, a := range args {
		want += a + "\n"
	}
	if string(out) != want {
		t.Errorf("got %q, want %q", out, want)
	}
	if got := Quote("pvectl", "--yes"); got != "pvectl --yes" {
		t.Errorf("plain args should stay unquoted, got %q", got)
	}
}

// Exec is the one place a process is really started. `sh` stands in for ssh.
func TestExecRunAndStream(t *testing.T) {
	ctx := context.Background()
	out, err := Exec{}.Run(ctx, "sh", "-c", "echo out; echo err >&2; exit 3")
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 3 {
		t.Errorf("error %v, want exit status 3", err)
	}
	if got := string(out); got != "out\nerr\n" && got != "err\nout\n" {
		t.Errorf("combined output %q", got)
	}

	var stdout, stderr bytes.Buffer
	if err := (Exec{}).Stream(ctx, &stdout, &stderr, "sh", "-c", "echo out; echo err >&2"); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "out\n" || stderr.String() != "err\n" {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	if err := (Exec{}).Stream(ctx, &stdout, &stderr, "sh", "-c", "exit 7"); !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Errorf("error %v, want exit status 7", err)
	}

	// A command that cannot be started at all is told apart from one that
	// ran and failed.
	err = Exec{}.Stream(ctx, &stdout, &stderr, "/nonexistent/pvectl-test-binary")
	var se *StartError
	if !errors.As(err, &se) {
		t.Errorf("error %v (%T), want a *StartError", err, err)
	}
}

func TestSSHArgsPutTheScriptLast(t *testing.T) {
	h := Host{Name: "pve-1", IP: "192.0.2.79"}
	got := SSHArgs(h, "uname -r")
	if n := len(got); got[n-1] != "uname -r" || got[n-2] != "root@192.0.2.79" || n != len(Args(h))+1 {
		t.Errorf("args %v", got)
	}
}

// LineWriter must deliver what bufio.ScanLines delivered: whole lines without
// their ending, a trailing \r dropped, and a last line with no newline.
func TestLineWriterSplitsLikeScanLines(t *testing.T) {
	var got []string
	w := &LineWriter{Line: func(s string) { got = append(got, s) }}
	for _, chunk := range []string{"one\ntw", "o\r\n\nthr", "ee"} {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	w.Flush()
	w.Flush()
	if want := []string{"one", "two", "", "three"}; !reflect.DeepEqual(got, want) {
		t.Errorf("lines %q, want %q", got, want)
	}

	input := "a\r\nb\n\nc\r"
	var viaScanner []string
	sc := bufio.NewScanner(strings.NewReader(input))
	for sc.Scan() {
		viaScanner = append(viaScanner, sc.Text())
	}
	got = nil
	_, _ = w.Write([]byte(input))
	w.Flush()
	if !reflect.DeepEqual(got, viaScanner) {
		t.Errorf("LineWriter %q, bufio.Scanner %q", got, viaScanner)
	}
}
