package agent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
)

// roleEnv is a Manager over one project with a fake VM per role (the legacy role's included).
type roleEnv struct {
	*multi
	mu    sync.Mutex
	vms   map[string]*Fake // role key -> fake
	projs map[string]Project
	clock time.Time
}

func newRoleEnv(t *testing.T, mods ...func(*Options)) *roleEnv {
	t.Helper()
	re := &roleEnv{vms: map[string]*Fake{}, projs: map[string]Project{}, clock: time.Now()}
	all := append([]func(*Options){func(o *Options) {
		o.NewVM = func(p Project) VM {
			re.mu.Lock()
			defer re.mu.Unlock()
			k := p.ID
			if p.Role != "" {
				k = p.Role
			}
			f := &Fake{}
			re.vms[k], re.projs[k] = f, p
			return f
		}
		o.Now = func() time.Time { re.mu.Lock(); defer re.mu.Unlock(); return re.clock }
	}}, mods...)
	re.multi = newMulti(t, []string{"my-project"}, all...)
	return re
}

func (re *roleEnv) advance(d time.Duration) {
	re.mu.Lock()
	re.clock = re.clock.Add(d)
	re.mu.Unlock()
}

func (re *roleEnv) vm(key string) *Fake {
	re.mu.Lock()
	defer re.mu.Unlock()
	return re.vms[key]
}

func (re *roleEnv) put(name string, mod func(*roles.Role)) (id string, version int) {
	re.t.Helper()
	spec := &roles.Role{Project: "my-project", Name: name, Description: "d", Tools: []string{"Read"}}
	if mod != nil {
		mod(spec)
	}
	res, err := re.m.roles.Put(roles.PutArgs{Spec: spec, By: "user"})
	if err != nil {
		re.t.Fatal(err)
	}
	return res.ID, res.Version
}

func (re *roleEnv) startRole(role, prompt string) map[string]any {
	re.t.Helper()
	res, err := re.op("start", map[string]any{"project": "my-project", "role": role, "prompt": prompt})
	if err != nil {
		re.t.Fatal(err)
	}
	return res
}

func TestRoleVMIsItsOwnAndLegacyIsUntouched(t *testing.T) {
	re := newRoleEnv(t, setLimits(3, 4))
	a, _ := re.put("visual-design", nil)
	b, _ := re.put("reviewer", nil)
	if r := re.startRole(a, "x"); r["id"] != "sess-1" {
		t.Fatalf("%v", r)
	}
	re.startRole(b, "y")
	re.start("my-project", "legacy")
	eventually(t, "three slots", func() bool { return re.running() == 3 })
	for _, id := range []string{a, b} {
		p := re.projs[id]
		base := filepath.Join(re.dir, RolesDir, id)
		if p.Role != id || p.ID != "my-project" || p.RoleDir != filepath.Join(base, "content") || p.ClaudeDir != filepath.Join(base, "claude") {
			t.Fatalf("role project %+v", p)
		}
		env := re.vm(id).Env()
		if env["RUBI_ROLE_DIR"] != "/rubi-role" || env["RUBI_CLAUDE_DIR"] != "/rubi-claude" {
			t.Fatalf("env %v", env)
		}
		if re.vm(id).Starts() != 1 || len(re.vm(id).Runner().CallsOf("session.start")) != 1 {
			t.Fatalf("role %s starts", id)
		}
	}
	if re.vm(a) == re.vm(b) || re.vm(a) == re.vm("my-project") {
		t.Fatal("roles share a VM")
	}
	lp := re.projs["my-project"]
	if lp.Role != "" || lp.RoleDir != "" || lp.ClaudeDir != "" {
		t.Fatalf("legacy project %+v", lp)
	}
	if env := re.vm("my-project").Env(); env["RUBI_ROLE_DIR"] != "" || env["RUBI_CLAUDE_DIR"] != "" {
		t.Fatalf("legacy env %v", env)
	}
	// The legacy role still keeps its sessions under /work/.rubi; a role writes nothing there.
	if _, err := os.Stat(filepath.Join(re.dir, "..", "my-project", ".rubi", ".gitignore")); err != nil {
		t.Fatalf("legacy prepare: %v", err)
	}
	// Every role's session list goes to its own VM.
	if _, err := re.op("sessions", map[string]any{"project": "my-project", "role": b}); err != nil {
		t.Fatal(err)
	}
	if n := len(re.vm(b).Runner().CallsOf("session.list")); n != 1 {
		t.Fatalf("list on b: %d", n)
	}
	if n := len(re.vm(a).Runner().CallsOf("session.list")); n != 0 {
		t.Fatalf("list on a: %d", n)
	}
}

func TestRoleOpErrors(t *testing.T) {
	re := newRoleEnv(t)
	a, _ := re.put("visual-design", nil)
	if _, err := re.op("start", map[string]any{"project": "my-project", "role": "r_zzzzzzzz", "prompt": "x"}); err == nil || !strings.Contains(err.Error(), "unknown role") {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := re.op("sessions", map[string]any{"project": "my-project", "role": "nope"}); err == nil {
		t.Fatal("bad id accepted")
	}
	// A role of another project is refused.
	other := filepath.Join(filepath.Dir(re.dir), "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	re.opt.Projects = func() []string { return []string{filepath.Join(filepath.Dir(re.dir), "my-project"), other} }
	re.m.Close()
	re.m = New(re.opt)
	t.Cleanup(re.m.Close)
	if _, err := re.op("start", map[string]any{"project": "other", "role": a, "prompt": "x"}); err == nil || !strings.Contains(err.Error(), "doesn't belong") {
		t.Fatalf("foreign: %v", err)
	}
}

func TestArchivedRoleRefusesStarts(t *testing.T) {
	re := newRoleEnv(t)
	id, v := re.put("old", nil)
	if _, err := re.m.roles.Put(roles.PutArgs{ID: id, BaseVersion: v, Spec: &roles.Role{Project: "my-project", Name: "old", Description: "d", Status: "archived", Tools: []string{"Read"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := re.op("start", map[string]any{"project": "my-project", "role": id, "prompt": "x"}); err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("archived start: %v", err)
	}
	if re.vm(id) != nil && re.vm(id).Starts() != 0 {
		t.Fatal("a VM started for an archived role")
	}
	// A start queued before the role was archived fails when it is delivered.
	id2, v2 := re.put("soon-old", nil)
	p, _ := re.m.projectFor("my-project", id2)
	if _, err := re.m.roles.Put(roles.PutArgs{ID: id2, BaseVersion: v2, Spec: &roles.Role{Project: "my-project", Name: "soon-old", Description: "d", Status: "archived", Tools: []string{"Read"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := re.m.deliver(re.ctx, p, QueueItem{Op: "start", Project: "my-project", Role: id2, Text: "x"}, nil); err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("archived delivery: %v", err)
	}
}

func TestPerRoleProxyHostsAndApprovals(t *testing.T) {
	re := newRoleEnv(t, setLimits(3, 4))
	a, _ := re.put("role-a", func(r *roles.Role) { r.Hosts = []string{"cdn.example.com"} })
	b, _ := re.put("role-b", nil)
	re.startRole(a, "x")
	re.startRole(b, "y")
	re.start("my-project", "z")
	eventually(t, "three slots", func() bool { return re.running() == 3 })
	hosts := func(key string) map[string]bool {
		re.m.mu.Lock()
		defer re.m.mu.Unlock()
		out := map[string]bool{}
		for _, h := range re.m.projs[key].hosts {
			out[h] = true
		}
		return out
	}
	if !hosts(a)["cdn.example.com"] || hosts(b)["cdn.example.com"] || hosts("my-project")["cdn.example.com"] {
		t.Fatalf("a=%v b=%v legacy=%v", hosts(a), hosts(b), hosts("my-project"))
	}
	// An approval belongs to the role that was asked.
	pa, _ := re.m.projectFor("my-project", a)
	if !re.m.net.add(pa.netKey(), "approved.example.net") {
		t.Fatal("add")
	}
	pb, _ := re.m.projectFor("my-project", b)
	pl, _ := re.m.projectFor("my-project", "")
	if len(re.m.net.hosts(pb.netKey())) != 0 || len(re.m.net.hosts(pl.netKey())) != 0 || len(re.m.net.hosts(pa.netKey())) != 1 {
		t.Fatalf("approvals leak: %v %v", re.m.net.hosts(pb.netKey()), re.m.net.hosts(pl.netKey()))
	}
	// They survive a restart of Home, still keyed by role.
	n := newNetState(re.dir)
	if len(n.hosts(roleNetKey(a))) != 1 || len(n.hosts(roleNetKey(b))) != 0 || len(n.hosts(pl.Dir)) != 0 {
		t.Fatalf("reloaded: %v", n.d)
	}
	// The legacy key stays the project's directory.
	re.m.net.add(pl.netKey(), "legacy.example.net")
	b2, _ := os.ReadFile(filepath.Join(re.dir, NetFile))
	if !strings.Contains(string(b2), `"projects":{"`+pl.Dir+`":["legacy.example.net"]}`) || !strings.Contains(string(b2), `"roles":{"`+a+`":["approved.example.net"]}`) {
		t.Fatalf("net file %s", b2)
	}
}

func TestRolePinKeepsVersionsOfARunningVM(t *testing.T) {
	re := newRoleEnv(t)
	id, v := re.put("pinned", nil)
	re.startRole(id, "x")
	eventually(t, "slot", func() bool { return re.running() == 1 })
	content := func(n int) string { return filepath.Join(re.dir, RolesDir, id, "content", "v"+strconv.Itoa(n)) }
	// Seven more versions go through the op, as the plugin's puts do; GC keeps only the newest ones otherwise.
	for i := 0; i < 7; i++ {
		spec := map[string]any{"project": "my-project", "name": "pinned", "description": "d" + strconv.Itoa(i), "tools": []string{"Read"}}
		if _, err := re.op("role.put", map[string]any{"id": id, "base_version": v + i, "spec": spec}); err != nil {
			t.Fatal(err)
		}
	}
	for n := v; n <= v+7; n++ {
		if _, err := os.Stat(content(n)); err != nil {
			t.Errorf("version %d was collected under a running VM: %v", n, err)
		}
	}
	// Once the VM stops the pins go and a later put collects the old ones.
	re.m.mu.Lock()
	p := re.m.projs[id]
	re.m.mu.Unlock()
	re.m.shutdown(p)
	spec := map[string]any{"project": "my-project", "name": "pinned", "description": "z", "tools": []string{"Read"}}
	if _, err := re.op("role.put", map[string]any{"id": id, "base_version": v + 7, "spec": spec}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(content(v)); err == nil {
		t.Error("the first version is still there after the VM stopped")
	}
	// Home never touches the VM-writable config directory.
	if st, err := os.Stat(filepath.Join(re.dir, RolesDir, id, "claude")); err != nil || !st.IsDir() {
		t.Fatalf("claude dir: %v", err)
	}
}

func TestIdleRoleVMStops(t *testing.T) {
	re := newRoleEnv(t, setLimits(3, 4), func(o *Options) { o.RoleVMIdle = func() time.Duration { return 15 * time.Minute } })
	a, _ := re.put("role-a", nil)
	re.startRole(a, "x")
	re.start("my-project", "legacy")
	eventually(t, "slots", func() bool { return re.running() == 2 })
	re.m.sweepIdle()
	if re.vm(a).State() != StateRunning {
		t.Fatal("stopped with a session running")
	}
	re.vm(a).Runner().Emit(statusEv("sess-1", "idle"))
	re.vm("my-project").Runner().Emit(statusEv("sess-1", "idle"))
	eventually(t, "slots freed", func() bool { return re.running() == 0 })
	re.advance(14 * time.Minute)
	re.m.sweepIdle()
	if re.vm(a).State() != StateRunning {
		t.Fatal("stopped before the idle time")
	}
	re.advance(2 * time.Minute)
	re.m.sweepIdle()
	if re.vm(a).State() != StateStopped {
		t.Fatalf("role VM state %s", re.vm(a).State())
	}
	if re.vm("my-project").State() != StateRunning {
		t.Fatal("the legacy VM was stopped for idleness")
	}
	// Its pins are gone and it starts again on the next use.
	re.startRole(a, "again")
	eventually(t, "restarted", func() bool { return re.vm(a).Starts() == 2 })
}

func TestQueuedItemKeepsRoleVMUp(t *testing.T) {
	re := newRoleEnv(t, setLimits(1, 4))
	a, _ := re.put("role-a", nil)
	re.startRole(a, "x")
	r := re.startRole(a, "queued") // per limit 1 (per project): waits
	if r["queued"] != true {
		t.Fatalf("%v", r)
	}
	re.advance(time.Hour)
	re.m.mu.Lock()
	for _, s := range re.m.slots { // the first session's slot is gone (a crashed runner would do this)
		re.m.releaseLocked(s)
	}
	re.m.mu.Unlock()
	re.advance(time.Hour)
	re.m.sweepIdle()
	if re.vm(a).State() != StateRunning {
		t.Fatal("an idle sweep stopped a role VM with a queued item")
	}
}

func TestStatusAndProjectsRowsHaveRoles(t *testing.T) {
	re := newRoleEnv(t, setLimits(3, 4))
	a, _ := re.put("role-a", nil)
	re.startRole(a, "x")
	re.start("my-project", "legacy")
	eventually(t, "slots", func() bool { return re.running() == 2 })
	st, _ := re.op("status", nil)
	got := map[string]string{}
	for _, s := range st["slots"].([]any) {
		row := s.(map[string]any)
		got[row["role"].(string)] = row["role_name"].(string)
	}
	if len(got) != 2 || got[a] != "role-a" || got["my-project"] != "default" {
		t.Fatalf("slots %v", st["slots"])
	}
	ps, _ := re.op("projects", nil)
	rows := ps["projects"].([]any)
	if len(rows) != 2 {
		t.Fatalf("rows %v", rows)
	}
	first, second := rows[0].(map[string]any), rows[1].(map[string]any)
	if first["role"] != "my-project" || first["role_name"] != "default" || second["role"] != a || second["role_name"] != "role-a" ||
		second["id"] != "my-project" || second["sessions_running"] != float64(1) || second["vm_state"] != "running" {
		t.Fatalf("project rows %v", rows)
	}
	// Items for the plugin carry the role.
	pl := startPoller(t, re.m)
	re.vm(a).Runner().Emit(statusEv("sess-1", "idle"))
	if it := pl.wait(t, "event"); it.Role != a || it.Project != "my-project" {
		t.Fatalf("event item %+v", it)
	}
}
