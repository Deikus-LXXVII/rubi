package agent

import (
	"bufio"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/netpipe"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
)

func TestT3RoleIsTheRoleOfItsProjectWithItsDirectories(t *testing.T) {
	re := newRoleEnv(t)
	id, _ := re.put("reviewer", func(r *roles.Role) { r.Hosts = []string{"git.example.com"} })
	r, err := re.m.T3Role(id)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Release()
	base := filepath.Join(re.dir, RolesDir, id)
	proj, _ := filepath.EvalSymlinks(filepath.Join(re.dir, "..", "my-project"))
	if r.ID != id || r.Name != "reviewer" || r.Project != "my-project" || r.ProjectDir != proj ||
		r.ClaudeDir != filepath.Join(base, "claude") || r.ContentDir != filepath.Join(base, "content") || r.MaxMode != "full-access" {
		t.Fatalf("role: %+v", r)
	}
	if got := re.m.T3Roles(); len(got) != 1 || got[0].ID != id || got[0].Name != "reviewer" || got[0].ClaudeDir != r.ClaudeDir {
		t.Fatalf("roles: %+v", got)
	}
}

func TestT3RoleRefusesWhatARunnerStartWouldRefuse(t *testing.T) {
	re := newRoleEnv(t)
	id, _ := re.put("reviewer", nil)
	for name, f := range map[string]func() string{
		"bad id":      func() string { return "../x" },
		"unknown":     func() string { return "r_zzzzzzzz" },
		"the project": func() string { return "my-project" }, // the legacy role has no directories of its own
		"empty":       func() string { return "" },
	} {
		if r, err := re.m.T3Role(f()); err == nil {
			r.Release()
			t.Errorf("%s: accepted", name)
		}
	}
	re2 := re.m.roles
	r, err := re2.Get(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	// archived roles take no new sessions
	spec := *r
	spec.Status = "archived"
	if _, err := re2.Put(roles.PutArgs{ID: id, BaseVersion: r.Version, Spec: &spec, By: "user"}); err != nil {
		t.Fatal(err)
	}
	if rr, err := re.m.T3Role(id); err == nil || !strings.Contains(err.Error(), "archived") {
		if err == nil {
			rr.Release()
		}
		t.Fatalf("archived: %v", err)
	}
	if got := re.m.T3Roles(); len(got) != 0 {
		t.Errorf("an archived role is listed for an instance: %+v", got)
	}
}

func TestT3RoleModeCeiling(t *testing.T) {
	for _, c := range []struct {
		role   string
		bypass *bool
		want   string
	}{
		{"", nil, "full-access"},
		{"default", nil, "full-access"}, // a role's default mode is not a cap
		{"", ptr(true), "full-access"},
		{"", ptr(false), "auto"},
		{"bypassPermissions", ptr(false), "auto"}, // the switch is the cap
	} {
		re := newRoleEnv(t, func(o *Options) {
			if c.bypass != nil {
				b := *c.bypass
				o.AllowBypass = func() bool { return b }
			}
		})
		id, _ := re.put("r", func(r *roles.Role) { r.DefaultMode = c.role })
		r, err := re.m.T3Role(id)
		if err != nil {
			t.Fatalf("%+v: %v", c, err)
		}
		if r.MaxMode != c.want {
			t.Errorf("role mode %q, bypass %v: ceiling %s, want %s", c.role, c.bypass, r.MaxMode, c.want)
		}
		r.Release()
	}
}

func ptr[T any](v T) *T { return &v }

// The proxy of a T3 session lets the Anthropic API and the role's hosts through, and asks (net.ask) for the rest.
func TestT3ProxyAllowsTheAPIAndTheRolesHostsAndAsksForTheRest(t *testing.T) {
	var dialed []string
	re := newRoleEnv(t, func(o *Options) {
		o.Resolve = func(ctx context.Context, host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}
		o.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialed = append(dialed, addr)
			a, b := net.Pipe()
			go func() { b.Close() }()
			return a, nil
		}
	})
	id, _ := re.put("reviewer", func(r *roles.Role) { r.Hosts = []string{"files.example.org"} })
	pl := startPoller(t, re.m)
	eventually(t, "the plugin polling", re.m.connected)
	role, err := re.m.T3Role(id)
	if err != nil {
		t.Fatal(err)
	}
	defer role.Release()
	ln := netpipe.NewListener("proxy", 4)
	px, token, err := re.m.T3Proxy(role, ln)
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	connect := func(host string) string {
		a, b := net.Pipe()
		if !ln.Offer(b) {
			t.Fatal("the proxy's listener refused")
		}
		defer a.Close()
		a.SetDeadline(time.Now().Add(10 * time.Second))
		auth := basic(token)
		if _, err := a.Write([]byte("CONNECT " + host + ":443 HTTP/1.1\r\nHost: " + host + ":443\r\nProxy-Authorization: " + auth + "\r\n\r\n")); err != nil {
			return "write: " + err.Error()
		}
		line, err := bufio.NewReader(a).ReadString('\n')
		if err != nil {
			return "read: " + err.Error()
		}
		return strings.TrimSpace(line)
	}
	if got := connect(AnthropicHost); !strings.Contains(got, " 200") {
		t.Errorf("the API host: %q", got)
	}
	if got := connect("files.example.org"); !strings.Contains(got, " 200") {
		t.Errorf("the role's host: %q", got)
	}
	// an unlisted host is asked about, as a runner VM's is: the plugin sees a net.ask for the role
	done := make(chan string, 1)
	go func() { done <- connect("unlisted.example.net") }()
	ask := pl.wait(t, "net.ask")
	if ask.Host != "unlisted.example.net" || ask.Role != id || ask.Project != "my-project" {
		t.Fatalf("net.ask: %+v", ask)
	}
	if err := re.m.Decide(ask.AskID, "deny", "", nil); err != nil {
		t.Fatal(err)
	}
	if got := <-done; strings.Contains(got, " 200") {
		t.Errorf("a denied host was connected: %q", got)
	}
	// no password, no tunnel
	a, b := net.Pipe()
	ln.Offer(b)
	a.SetDeadline(time.Now().Add(5 * time.Second))
	a.Write([]byte("CONNECT " + AnthropicHost + ":443 HTTP/1.1\r\nHost: x\r\n\r\n"))
	line, _ := bufio.NewReader(a).ReadString('\n')
	a.Close()
	if strings.Contains(line, " 200") {
		t.Errorf("a client without the password was served: %q", line)
	}
}

func TestT3Caps(t *testing.T) {
	three, seven := 3, 7
	re := newRoleEnv(t, func(o *Options) {
		o.Usage = func() UsageConfig { return UsageConfig{MaxSessionsPerProfile: 2, MaxSessions: &seven} }
	})
	if per, total := re.m.T3Caps(); per != 2 || total != 7 {
		t.Errorf("caps %d %d", per, total)
	}
	re2 := newRoleEnv(t, func(o *Options) { o.Usage = func() UsageConfig { return UsageConfig{MaxSessions: &three} } })
	if per, total := re2.m.T3Caps(); per < 1 || total != 3 {
		t.Errorf("caps %d %d", per, total)
	}
}

func TestSessionPipeAndContainerHelpersUseTheSeams(t *testing.T) {
	f := &authFake{}
	a, _ := newAuthVM(t, f)
	if err := a.RunDetached(context.Background(), []string{"container", "run", "-d", "x"}); err != nil {
		t.Fatal(err)
	}
	if f.find("run -d x") == nil {
		t.Errorf("commands: %v", f.cmds)
	}
	if err := a.RemoveContainer(context.Background(), "rubi-t3-x"); err != nil {
		t.Fatal(err)
	}
	if f.find("stop --time 5 rubi-t3-x") == nil || f.find("delete --force rubi-t3-x") == nil {
		t.Errorf("commands: %v", f.cmds)
	}
	if err := a.RunDetached(context.Background(), nil); err == nil {
		t.Error("an empty command ran")
	}
}

// The forwarder of a session VM starts with the T3 port in its environment, and its streams go to the listeners.
func TestStartSessionPipeGivesTheForwarderItsEnvironment(t *testing.T) {
	fastPipe(t)
	vm := newVMPipes(t, nil)
	f := &authFake{}
	a, _ := newAuthVM(t, f)
	a.spawn = vm.spawn
	spec := &PipeSpec{Proxy: netpipe.NewListener("p", 1), Hook: netpipe.NewListener("h", 1), T3: netpipe.NewListener("t", 1)}
	stop, err := a.StartSessionPipe(context.Background(), "rubi-t3-x", spec, map[string]string{"RUBI_T3_PORT": "4411"}, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if len(vm.argvs) != 1 || !strings.Contains(strings.Join(vm.argvs[0], " "), "exec -i --user 10001 --workdir /work --env-file") || vm.argvs[0][len(vm.argvs[0])-2] != "rubi-t3-x" {
		t.Fatalf("forwarder command: %q", vm.argvs)
	}
	s, _ := vm.latest().Open(netpipe.SvcT3)
	got := make(chan byte, 1)
	go func() {
		c, err := spec.T3.Accept()
		if err == nil {
			got <- c.(*netpipe.Stream).Service()
		}
	}()
	select {
	case svc := <-got:
		if svc != netpipe.SvcT3 {
			t.Fatalf("service %d", svc)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the T3 listener got no stream")
	}
	s.Close()
}

// A role made for one directory is not used for another that now has the same project id.
func TestT3RoleRefusesARoleMadeForAnotherDirectory(t *testing.T) {
	var dirs []string
	re := newRoleEnv(t, func(o *Options) { o.Projects = func() []string { return dirs } })
	dirs = []string{filepath.Join(re.dir, "..", "my-project")}
	id, _ := re.put("reviewer", nil)
	if r, err := re.m.T3Role(id); err != nil {
		t.Fatal(err)
	} else {
		r.Release()
	}
	other := filepath.Join(t.TempDir(), "my-project") // same name, so the same id, but another directory
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	dirs = []string{other}
	if r, err := re.m.T3Role(id); err == nil || !strings.Contains(err.Error(), "another directory") {
		if err == nil {
			r.Release()
		}
		t.Fatalf("a role of another directory was accepted: %v", err)
	}
}

func TestInspectMountsAndListContainersReadTheToolsJSON(t *testing.T) {
	a, _ := newAuthVM(t, &authFake{})
	a.run = func(ctx context.Context, argv []string) ([]byte, error) {
		switch argv[1] {
		case "inspect":
			if argv[2] != "rubi-t3-x" {
				t.Errorf("inspect %v", argv)
			}
			return []byte(`[{"id":"rubi-t3-x","configuration":{"mounts":[
				{"source":"/p/w","destination":"/p/w","options":[],"type":{"virtiofs":{}}},
				{"source":"/h/m.json","destination":"/etc/claude-code/managed-settings.json","options":["ro"],"type":{"virtiofs":{}}}]}}]`), nil
		case "ls":
			return []byte(`[{"configuration":{"id":"buildkit"}},{"configuration":{"id":"rubi-t3-x"}},{"configuration":{"id":""}}]`), nil
		}
		return nil, nil
	}
	ms, err := a.InspectMounts(context.Background(), "rubi-t3-x")
	if err != nil || len(ms) != 2 || ms[0].ReadOnly || !ms[1].ReadOnly || ms[1].Source != "/h/m.json" || ms[0].Type != "virtiofs" || ms[1].Destination != "/etc/claude-code/managed-settings.json" {
		t.Fatalf("%+v %v", ms, err)
	}
	names, err := a.ListContainers(context.Background())
	if err != nil || len(names) != 2 || names[1] != "rubi-t3-x" {
		t.Fatalf("%v %v", names, err)
	}
	a.run = func(context.Context, []string) ([]byte, error) { return []byte("not json"), nil }
	if _, err := a.InspectMounts(context.Background(), "x"); err == nil {
		t.Error("garbage accepted")
	}
	if _, err := a.ListContainers(context.Background()); err == nil {
		t.Error("garbage accepted")
	}
}
