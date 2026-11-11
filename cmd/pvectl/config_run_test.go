package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// complete asks the command line for completions the way the shell does.
func (c *cli) complete(args ...string) string {
	c.t.Helper()
	root := c.root()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs(append([]string{"__complete", "--config", c.config}, args...))
	if err := root.ExecuteContext(context.Background()); err != nil {
		c.t.Fatal(err)
	}
	return out.String()
}

// Node names are completed from the config that --config names, at either
// depth a node operation is reachable at.
func TestCompletionReadsNodeNamesFromTheConfig(t *testing.T) {
	c := newCLI(t, newWorld(t))
	for _, args := range [][]string{
		{"reboot", "node", ""},
		{"plan", "reboot", "node", ""},
		{"preflight", ""},
	} {
		got := c.complete(args...)
		if !strings.HasPrefix(got, "node-a\nnode-b\nnode-c\n:4\n") {
			t.Errorf("%v completed to %q", args, got)
		}
	}
	if got := c.complete("plan", "reboot", ""); !strings.HasPrefix(got, "node\n:4\n") {
		t.Errorf("plan reboot completed to %q", got)
	}
	if got := c.complete("rolling", "reboot", ""); !strings.HasPrefix(got, "cluster\n:4\n") {
		t.Errorf("rolling reboot completed to %q", got)
	}
	// An unreadable config means no suggestions, not an error.
	c.config = "/nonexistent/config.yaml"
	if got := c.complete("reboot", "node", ""); !strings.HasPrefix(got, ":4\n") {
		t.Errorf("without a config: %q", got)
	}
}

// config show and config validate read the file --config names.
func TestConfigCommandsUseTheGivenFile(t *testing.T) {
	c := newCLI(t, newWorld(t))
	if err := c.run("config", "show"); err != nil {
		t.Errorf("config show: %v", err)
	}
	// A file that is consistent goes on to the live checks, which need an Env.
	c.refuse = errStop
	if err := c.run("config", "validate"); !errors.Is(err, errStop) {
		t.Errorf("config validate: %v", err)
	}
	if len(c.built) != 1 || c.built[0].target != "" {
		t.Errorf("env built with %+v", c.built)
	}

	bad := writeConfig(t, strings.Replace(fixtureYAML, "lan_ip: 192.0.2.11", "lan_ip: not-an-ip", 1))
	c.config = bad
	c.built = nil
	err := c.run("config", "validate")
	if err == nil || err.Error() != "2 problem(s) in "+bad {
		t.Errorf("config validate on a bad file: %v", err)
	}
	if len(c.built) != 0 {
		t.Error("a file with problems must not reach the live checks")
	}

	c.config = "/nonexistent/config.yaml"
	for _, sub := range []string{"show", "validate"} {
		if err := c.run("config", sub); err == nil || !strings.HasPrefix(err.Error(), "read config /nonexistent/config.yaml: ") {
			t.Errorf("config %s without a file: %v", sub, err)
		}
	}
}
