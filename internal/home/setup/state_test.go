package setup

import (
	"context"
	"strings"
	"testing"
)

func TestStatusOnABareMac(t *testing.T) {
	e := newTestEnv(t)
	st := e.s.Status(context.Background())
	if len(st.Steps) != 7 || st.Job != nil || st.Login != nil {
		t.Fatalf("status: %+v", st)
	}
	want := []string{"container", "service", "network", "image", "projects", "login", "selftest"}
	for i, s := range st.Steps {
		if s.ID != want[i] || s.Title == "" || s.State != Todo {
			t.Errorf("step %d: %+v", i, s)
		}
	}
	if st.Steps[0].Action != "install" || !strings.Contains(st.Steps[0].Reason, "1.6.1") {
		t.Errorf("container: %+v", st.Steps[0])
	}
	for i := 1; i <= 3; i++ { // each names the first step that isn't done
		if st.Steps[i].Reason != "Needs step 1 first" || st.Steps[i].Action != "" {
			t.Errorf("step %d: %+v", i+1, st.Steps[i])
		}
	}
	if st.Steps[4].Action != "add" || st.Steps[4].State != Todo {
		t.Errorf("projects: %+v", st.Steps[4])
	}
	if st.Steps[5].Reason != "Needs step 1 first" || st.Steps[6].Reason != "Needs step 1 first" {
		t.Errorf("login/selftest: %+v %+v", st.Steps[5], st.Steps[6])
	}
}

func TestContainerStep(t *testing.T) {
	for _, c := range []struct {
		name, out string
		goos      string
		state     string
		action    string
	}{
		{"missing", "", "darwin", Todo, "install"},
		{"older", "container CLI version 1.5.9 (build: release, commit: abc)\n", "darwin", Todo, "upgrade"},
		{"same", version161, "darwin", Done, ""},
		{"newer is never downgraded", "container CLI version 1.7.0 (build: release, commit: abc)\n", "darwin", Done, ""},
		{"unreadable", "something else\n", "darwin", Failed, ""},
		{"not a Mac", version161, "linux", Failed, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newTestEnv(t, func(o *Options) { o.GOOS = c.goos })
			e.m.version = c.out
			if st := e.step("container"); st.State != c.state || st.Action != c.action {
				t.Errorf("%+v", st)
			}
		})
	}
}

func TestServiceStep(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	if st := e.step("service"); st.State != Done {
		t.Fatalf("running with a kernel: %+v", st)
	}
	e.m.sysJSON = strings.Replace(e.m.sysJSON, `"status":"running"`, `"status":"stopped"`, 1)
	if st := e.step("service"); st.State != Todo || st.Action != "start" {
		t.Errorf("stopped: %+v", st)
	}
	e.m.sysJSON = ""
	if st := e.step("service"); st.State != Todo || st.Action != "start" {
		t.Errorf("not answering: %+v", st)
	}
	e.ready()
	e.m.sysJSON = strings.ReplaceAll(sysRunning, "%APP%", t.TempDir()+"/") // no kernel there
	if st := e.step("service"); st.State != Todo || !strings.Contains(st.Reason, "kernel") || st.Action != "start" {
		t.Errorf("no kernel: %+v", st)
	}
	e.m.sysJSON = strings.ReplaceAll(sysRunning, "%APP%", "relative/")
	if st := e.step("service"); st.State != Failed {
		t.Errorf("odd layout: %+v", st)
	}
	e.m.sysJSON = "not json"
	if st := e.step("service"); st.State != Todo {
		t.Errorf("not json: %+v", st)
	}
}

func TestNetworkStep(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	if st := e.step("network"); st.State != Done || !strings.Contains(st.Reason, "192.168.128.1") {
		t.Fatalf("host-only: %+v", st)
	}
	e.m.network = ""
	if st := e.step("network"); st.State != Todo || st.Action != "create" {
		t.Errorf("missing: %+v", st)
	}
	e.m.network = "[]"
	if st := e.step("network"); st.State != Todo || st.Action != "create" {
		t.Errorf("empty list: %+v", st)
	}
	// the default network has another name and mode: with agent_network = default it is refused, not changed
	e.cfg.Network = "default"
	e.m.network = defaultNet(t)
	st := e.step("network")
	if st.State != Failed || !strings.Contains(st.Reason, "Remove the network default or set another agent_network") || st.Action != "" {
		t.Errorf("nat: %+v", st)
	}
	e.cfg.Network = ""
	e.m.network = "garbage"
	if st := e.step("network"); st.State != Failed {
		t.Errorf("garbage: %+v", st)
	}
}

func TestNetworkStepProxyAddress(t *testing.T) {
	for _, c := range []struct {
		listen, vm string
		state      string
	}{
		{"", "", Done},
		{"192.168.128.1:0", "", Done},
		{"192.168.128.1:3128", "192.168.128.1", Done},
		{"127.0.0.1:3128", "", NeedsUser},
		{"", "host.example.com", NeedsUser},
	} {
		e := newTestEnv(t)
		e.ready()
		e.cfg.ProxyListen, e.cfg.ProxyVMHost = c.listen, c.vm
		st := e.step("network")
		if st.State != c.state || c.state == NeedsUser && st.Action != "use_auto_address" {
			t.Errorf("%q %q: %+v", c.listen, c.vm, st)
		}
	}
}

func TestImageAndProjectsSteps(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	if st := e.step("image"); st.State != Done || !strings.Contains(st.Reason, "rubi-agent-runner:") {
		t.Fatalf("%+v", st)
	}
	e.m.image = false
	if st := e.step("image"); st.State != Todo || st.Action != "build" {
		t.Errorf("%+v", st)
	}
	e.m.image = true
	if st := e.step("projects"); st.State != Todo || st.Action != "add" {
		t.Errorf("no project: %+v", st)
	}
	p := e.addProject("my-project")
	st := e.step("projects")
	if st.State != Done || len(st.Projects) != 1 || st.Projects[0].Dir != p.Dir || st.Projects[0].Action != "remove" {
		t.Errorf("one project: %+v", st)
	}
	e.ag.projects = nil // configured, but none usable
	if st := e.step("projects"); st.State != Todo || !strings.Contains(st.Reason, "None of the configured") {
		t.Errorf("unusable: %+v", st)
	}
}
