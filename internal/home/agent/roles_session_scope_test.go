package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
)

// listing makes a role's runner list the given session ids.
func listing(ids ...string) func(string, *FakeRunner) {
	return func(_ string, fr *FakeRunner) {
		fr.Handle = func(method string, _ json.RawMessage) (any, error, bool) {
			if method != "session.list" {
				return nil, nil, false
			}
			rows := []any{}
			for _, id := range ids {
				rows = append(rows, map[string]any{"id": id})
			}
			return map[string]any{"sessions": rows}, nil, true
		}
	}
}

func TestRoleSessionOpsRefuseForeignSessions(t *testing.T) {
	re := newRoleEnv(t, setLimits(3, 6))
	re.setup = listing("mine-1")
	a, _ := re.put("role-a", nil)
	re.startRole(a, "x") // boots the VM
	eventually(t, "slot", func() bool { return re.running() == 1 })
	calls := func(method string) int { return len(re.vm(a).Runner().CallsOf(method)) }

	for _, op := range []string{"send", "messages", "rename", "fork", "interrupt", "close", "mode"} {
		args := map[string]any{"project": "my-project", "role": a, "id": "foreign-1", "text": "hi", "name": "n", "mode": "plan"}
		if _, err := re.op(op, args); err == nil || !strings.Contains(err.Error(), "doesn't belong to role") {
			t.Errorf("%s of a foreign session: %v", op, err)
		}
	}
	for _, method := range []string{"session.send", "session.messages", "session.rename", "session.fork", "session.interrupt", "session.close", "session.mode"} {
		if n := calls(method); n != 0 {
			t.Errorf("%s reached the runner %d times", method, n)
		}
	}
	// The role's own session (listed by its runner) goes through.
	if _, err := re.op("send", map[string]any{"project": "my-project", "role": a, "id": "mine-1", "text": "hi"}); err != nil {
		t.Fatal(err)
	}
	if n := calls("session.send"); n != 1 {
		t.Fatalf("send reached the runner %d times", n)
	}
	if _, err := re.op("messages", map[string]any{"project": "my-project", "role": a, "id": "mine-1"}); err != nil {
		t.Fatal(err)
	}
	// A session that holds a slot is the role's own whatever the listing says.
	if _, err := re.op("rename", map[string]any{"project": "my-project", "role": a, "id": "sess-1", "name": "n"}); err != nil {
		t.Fatal(err)
	}
}

func TestRoleReadsDoNotBootOrKeepTheVMUp(t *testing.T) {
	re := newRoleEnv(t, setLimits(1, 4), func(o *Options) { o.RoleVMIdle = func() time.Duration { return 15 * time.Minute } })
	a, _ := re.put("role-a", nil)
	// A stopped VM is not started by a read.
	for _, op := range []string{"sessions", "messages"} {
		res, err := re.op(op, map[string]any{"project": "my-project", "role": a, "id": "x-1"})
		if err != nil || res["vm_state"] != "stopped" {
			t.Fatalf("%s: %v %v", op, res, err)
		}
	}
	if re.vm(a) != nil && re.vm(a).Starts() != 0 {
		t.Fatal("a read started the VM")
	}
	for _, op := range []string{"interrupt", "mode"} {
		if _, err := re.op(op, map[string]any{"project": "my-project", "role": a, "id": "x-1", "mode": "plan"}); err == nil {
			t.Errorf("%s on a stopped VM went through", op)
		}
	}
	// A read of a running VM doesn't reset its idle clock.
	re.startRole(a, "x")
	re.vm(a).Runner().Emit(statusEv("sess-1", "idle"))
	eventually(t, "slot freed", func() bool { return re.running() == 0 })
	re.advance(14 * time.Minute)
	if _, err := re.op("sessions", map[string]any{"project": "my-project", "role": a}); err != nil {
		t.Fatal(err)
	}
	re.advance(2 * time.Minute)
	re.m.sweepIdle()
	if re.vm(a).State() != StateStopped {
		t.Fatalf("a read kept the VM up: %s", re.vm(a).State())
	}
}

func TestRoleVMGetsTheShield(t *testing.T) {
	re := newRoleEnv(t, setLimits(2, 4))
	a, _ := re.put("role-a", nil)
	proj := filepath.Join(filepath.Dir(re.dir), "my-project")
	if err := os.MkdirAll(filepath.Join(proj, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	re.startRole(a, "x")
	eventually(t, "slot", func() bool { return re.running() == 1 })
	pj := re.vm(a).Runner().Project
	if pj.EmptyDir != filepath.Join(re.dir, RoleEmptyDir) || len(pj.ReadOnly) != 1 || pj.ReadOnly[0] != ".claude" {
		t.Fatalf("role project %+v", pj)
	}
	if ents, err := os.ReadDir(pj.EmptyDir); err != nil || len(ents) != 0 {
		t.Fatalf("the empty directory: %v %v", ents, err)
	}
	if _, err := os.Stat(filepath.Join(proj, ".rubi")); err != nil {
		t.Fatalf("/work/.rubi must exist to be covered: %v", err)
	}
	// No .claude in the project, or one that is a symlink: nothing to bind (the tool would create the mount point).
	if err := os.RemoveAll(filepath.Join(proj, ".claude")); err != nil {
		t.Fatal(err)
	}
	if s := roleShield(proj, filepath.Join(re.dir, RoleEmptyDir)); len(s.ReadOnly) != 0 || s.EmptyDir == "" {
		t.Fatalf("no .claude: %+v", s)
	}
	if err := os.Symlink("/etc", filepath.Join(proj, ".claude")); err != nil {
		t.Fatal(err)
	}
	if s := roleShield(proj, filepath.Join(re.dir, RoleEmptyDir)); len(s.ReadOnly) != 0 {
		t.Fatalf("symlinked .claude: %+v", s)
	}
	// Something in the empty directory: no shield, so the start refuses.
	if err := os.WriteFile(filepath.Join(re.dir, RoleEmptyDir, "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if s := roleShield(proj, filepath.Join(re.dir, RoleEmptyDir)); s.EmptyDir != "" {
		t.Fatalf("dirty empty dir: %+v", s)
	}
}

func TestRoleIsBoundToItsProjectDirectory(t *testing.T) {
	re := newRoleEnv(t, setLimits(2, 4))
	a, _ := re.put("role-a", nil)
	r, err := re.m.roles.Get(a, 0)
	if err != nil || r.DirHash != roles.DirHash(re.m.projectDir("my-project")) {
		t.Fatalf("hash %q %v", r.DirHash, err)
	}
	// The spec can't set it, and a later version keeps it.
	spec := map[string]any{"project": "my-project", "name": "role-a", "description": "e", "tools": []string{"Read"}, "dir_hash": "abc"}
	if _, err := re.op("role.put", map[string]any{"id": a, "base_version": 1, "spec": spec}); err != nil {
		t.Fatal(err)
	}
	if r2, _ := re.m.roles.Get(a, 0); r2.DirHash != r.DirHash {
		t.Fatalf("hash changed: %q", r2.DirHash)
	}
	// The project id now names another directory: the id "my-project" moves to a different one.
	other := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	re.opt.Projects = func() []string { return []string{other} }
	re.m.Close()
	re.m = New(re.opt)
	t.Cleanup(re.m.Close)
	for _, op := range []string{"start", "sessions"} {
		if _, err := re.op(op, map[string]any{"project": "my-project", "role": a, "prompt": "x"}); err == nil || !strings.Contains(err.Error(), "another directory") {
			t.Errorf("%s in another directory: %v", op, err)
		}
	}
	if re.vm(a) != nil && re.vm(a).Starts() != 0 {
		t.Fatal("a VM started for a role bound to another directory")
	}
}

func TestRolePinsAreBoundedAndCoverTheBootWindow(t *testing.T) {
	re := newRoleEnv(t, setLimits(2, 4))
	id, v := re.put("pinned", nil)
	put := func(base int, desc string) {
		spec := map[string]any{"project": "my-project", "name": "pinned", "description": desc, "tools": []string{"Read"}}
		if _, err := re.op("role.put", map[string]any{"id": id, "base_version": base, "spec": spec}); err != nil {
			t.Error(err)
		}
	}
	// A put while the VM boots (before its runner is up) is pinned.
	re.setup = func(key string, _ *FakeRunner) {
		if key == id {
			put(v, "during boot")
		}
	}
	re.startRole(id, "x")
	eventually(t, "slot", func() bool { return re.running() == 1 })
	re.setup = nil
	pins := func() int {
		re.m.mu.Lock()
		defer re.m.mu.Unlock()
		return len(re.m.projs[id].pins)
	}
	if n := pins(); n != 2 {
		t.Fatalf("pins after a put in the boot window: %d, want 2 (the version read and the one put)", n)
	}
	for i := 0; i < 5; i++ {
		put(v+1+i, "later "+strconv.Itoa(i))
	}
	if n := pins(); n != 7 {
		t.Fatalf("pins: %d, want one per version (7)", n)
	}
	re.m.mu.Lock()
	p := re.m.projs[id]
	re.m.pinLocked(p, v) // a version already pinned is not pinned again
	re.m.mu.Unlock()
	if n := pins(); n != 7 {
		t.Fatalf("a duplicate pin was added: %d", n)
	}
	re.m.shutdown(p)
	if n := pins(); n != 0 {
		t.Fatalf("pins after the VM stopped: %d", n)
	}
	// A start that fails leaves nothing pinned and the window closed.
	id2, _ := re.put("failing", nil)
	pf, _ := re.m.projectFor("my-project", id2)
	re.vm(id2).StartErr = errors.New("boom")
	if _, err := re.m.ensure(re.ctx, pf); err == nil {
		t.Fatal("start succeeded")
	}
	re.m.mu.Lock()
	n, st := len(pf.pins), pf.starting
	re.m.mu.Unlock()
	if n != 0 || st {
		t.Fatalf("after a failed start: %d pins, starting=%v", n, st)
	}
}

func TestRoleRevocationReachesARunningVM(t *testing.T) {
	re := newRoleEnv(t, setLimits(2, 4))
	a, v := re.put("role-a", func(r *roles.Role) { r.Hosts = []string{"cdn.example.com", "keep.example.com"} })
	re.startRole(a, "x")
	eventually(t, "slot", func() bool { return re.running() == 1 })
	re.m.mu.Lock()
	px := re.m.projs[a].proxy
	re.m.mu.Unlock()
	if !px.Allowed("cdn.example.com") {
		t.Fatal("host not allowed to begin with")
	}
	spec := &roles.Role{Project: "my-project", Name: "role-a", Description: "d", Tools: []string{"Read"}, Hosts: []string{"keep.example.com"}}
	res, err := re.m.roles.Put(roles.PutArgs{ID: a, BaseVersion: v, Spec: spec, By: "user", Change: &roles.Change{Approval: "ap-test-2"}})
	if err != nil {
		t.Fatal(err)
	}
	re.m.rolePut(res)
	if px.Allowed("cdn.example.com") || !px.Allowed("keep.example.com") {
		t.Fatal("the proxy still has the removed host")
	}
	re.m.mu.Lock()
	hosts := re.m.projs[a].hosts
	re.m.mu.Unlock()
	if !contains(hosts, "keep.example.com") || contains(hosts, "cdn.example.com") {
		t.Fatalf("hosts %v", hosts)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func TestArchivingStopsAnIdleVMAndBlocksSends(t *testing.T) {
	re := newRoleEnv(t, setLimits(2, 4))
	a, v := re.put("role-a", nil)
	b, vb := re.put("role-b", nil)
	re.startRole(a, "x")
	re.startRole(b, "y")
	eventually(t, "slots", func() bool { return re.running() == 2 })
	archive := func(id string, ver int, name string) {
		spec := map[string]any{"project": "my-project", "name": name, "description": "d", "tools": []string{"Read"}, "status": "archived"}
		if _, err := re.op("role.put", map[string]any{"id": id, "base_version": ver, "spec": spec}); err != nil {
			t.Fatal(err)
		}
	}
	// With a session running the VM stays (the turn continues); a send is refused.
	archive(a, v, "role-a")
	if re.vm(a).State() != StateRunning {
		t.Fatal("archiving stopped a VM with a session running")
	}
	if _, err := re.op("send", map[string]any{"project": "my-project", "role": a, "id": "sess-1", "text": "more"}); err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("send to an archived role: %v", err)
	}
	// With nothing running the VM goes at once.
	re.vm(b).Runner().Emit(statusEv("sess-1", "idle"))
	eventually(t, "slot freed", func() bool { return re.running() == 1 })
	archive(b, vb, "role-b")
	if re.vm(b).State() != StateStopped {
		t.Fatalf("an archived role's idle VM: %s", re.vm(b).State())
	}
}
