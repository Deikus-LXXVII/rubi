package t3vm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestProbeRunsTheCommandInASessionVMAndRemovesIt(t *testing.T) {
	w := newWorld(t)
	out, err := w.srv.Probe(context.Background(), testRole, []string{"node", "-e", "1", "arg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Error("no output passed back")
	}
	runs, removed, stops := w.host.snapshot()
	if len(runs) != 1 {
		t.Fatalf("VM runs: %d", len(runs))
	}
	// the VM is a session's: same mounts, in the role's project folder
	run := strings.Join(runs[0], " ")
	for _, want := range []string{"--network none", "--user 10001", "--cap-drop ALL", "--read-only", "target=" + w.project + " ", "managed-settings.json", "agent-signin", "--workdir " + w.project} {
		if !strings.Contains(run, want) {
			t.Errorf("probe VM lacks %q: %s", want, run)
		}
	}
	if len(w.host.outputs) != 1 {
		t.Fatalf("commands run: %v", w.host.outputs)
	}
	exec := w.host.outputs[0]
	n := len(exec)
	if exec[1] != "exec" || exec[n-4] != "node" || exec[n-1] != "arg" || slicesContains(exec, "/opt/claude") {
		t.Errorf("exec line: %v", exec)
	}
	// ... and workdir is the project folder, with the session's env file
	if !slicesContains(exec, "--env-file") || !slicesContains(exec, "--workdir") || !slicesContains(exec, w.project) {
		t.Errorf("exec line: %v", exec)
	}
	if len(removed) != 1 || stops != 1 {
		t.Errorf("removed %v, pipe stops %d", removed, stops)
	}
	// the place under the caps is free again
	if w.srv.Sessions() != 0 {
		t.Errorf("%d sessions after the probe", w.srv.Sessions())
	}
	if w.be.releasedCount() < 2 { // the lookup, and the session's pin
		t.Errorf("role released %d times", w.be.releasedCount())
	}
}

func slicesContains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func TestProbeFollowsTheSessionRules(t *testing.T) {
	w := newWorld(t)
	if _, err := w.srv.Probe(context.Background(), "r_unknown1", []string{"true"}); err == nil {
		t.Error("probe for an unknown role")
	}
	if _, err := w.srv.Probe(context.Background(), testRole, nil); err == nil {
		t.Error("probe with no command")
	}
	w.host.outputErr = errors.New("exec failed")
	if _, err := w.srv.Probe(context.Background(), testRole, []string{"true"}); err == nil || !strings.Contains(err.Error(), "exec failed") {
		t.Errorf("a failing command: %v", err)
	}
	_, removed, _ := w.host.snapshot()
	if len(removed) != 1 {
		t.Errorf("the VM of a failed probe stays: %v", removed)
	}
	// the caps hold for it: with the role's one place taken, a probe is refused like a session
	w.be.perRole = 1
	rel, err := w.srv.admit(testRole)
	if err != nil {
		t.Fatal(err)
	}
	defer rel()
	w.host.outputErr = nil
	start := time.Now()
	if _, err := w.srv.Probe(context.Background(), testRole, []string{"true"}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("probe over the cap: %v", err)
	}
	_ = start
}

func TestVersionAsksTheImage(t *testing.T) {
	w := newWorld(t)
	v, err := w.srv.Version(context.Background(), testRole)
	if err != nil || strings.TrimSpace(v) != "2.1.314 (Claude Code)" {
		t.Fatalf("%q %v", v, err)
	}
	if _, err := w.srv.Version(context.Background(), "r_unknown1"); err == nil {
		t.Error("version for an unknown role")
	}
}
