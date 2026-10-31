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

// The network step is "done: not needed" whatever the container tool has, even settings of the old host-only design.
func TestNetworkStepIsNotNeeded(t *testing.T) {
	e := newTestEnv(t)
	e.ready()
	check := func(what string) {
		t.Helper()
		st := e.step("network")
		if st.ID != "network" || st.State != Done || !strings.Contains(st.Reason, "Not needed") || st.Action != "" || st.Title != "VM network (none needed)" {
			t.Errorf("%s: %+v", what, st)
		}
	}
	check("a network exists")
	e.m.network = ""
	check("no network")
	e.cfg.Network, e.cfg.ProxyListen, e.cfg.ProxyVMHost = "default", "127.0.0.1:3128", "host.example.com"
	check("old settings")
	for _, c := range e.cmd.calls {
		if len(c) > 1 && c[1] == "network" {
			t.Errorf("the step looked at a network: %v", c)
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
