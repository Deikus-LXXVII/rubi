package home

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/home/t3instances"
	"github.com/Deikus-LXXVII/rubi/internal/t3"
)

// agentLife is a Home with T3 up, one role (project my-project) and room for several sessions.
func agentLife(t *testing.T) (*life, string) {
	t.Helper()
	l := newLife(t)
	l.t3.TaskSupervisor = func(context.Context) error { return nil }
	role := putRole(t, l.h, "reviewer")
	zero := 0
	if err := l.h.Update(func(c *Config) { c.AgentMaxSessionsPerProfile, c.AgentMaxSessions = 20, &zero }); err != nil {
		t.Fatal(err)
	}
	l.up(t)
	return l, role
}

func startThread(t *testing.T, l *life, role string, extra map[string]any) string {
	t.Helper()
	args := map[string]any{"project": "my-project", "role": role, "prompt": "do the work"}
	for k, v := range extra {
		args[k] = v
	}
	res, err := handle(t, l.h, "tok", "t3-agent.start", args)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res["id"].(string)
	if id == "" {
		t.Fatalf("start gave %v", res)
	}
	return id
}

func nextItems(t *testing.T, h *Helper, want int) []agent.Item {
	t.Helper()
	var got []agent.Item
	deadline := time.Now().Add(10 * time.Second)
	for len(got) < want && time.Now().Before(deadline) {
		items, err := h.Agent.Next(context.Background(), 300*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, items...)
	}
	return got
}

func drain(t *testing.T, h *Helper) {
	t.Helper()
	nextItems(t, h, 0)
	_, _ = h.Agent.Next(context.Background(), time.Millisecond)
}

// resultOf is the decoded event of kind result among the items.
func resultOf(t *testing.T, items []agent.Item) map[string]any {
	t.Helper()
	for _, it := range items {
		if it.Type != "event" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal(it.Event, &ev) == nil && ev["kind"] == "result" {
			ev["_project"], ev["_role"], ev["_session"] = it.Project, it.Role, it.SessionID
			return ev
		}
	}
	t.Fatalf("no result event among %d items", len(items))
	return nil
}

func kinds(items []agent.Item) []string {
	var out []string
	for _, it := range items {
		var ev map[string]any
		_ = json.Unmarshal(it.Event, &ev)
		out = append(out, fmt.Sprint(it.Type, ":", ev["kind"], ":", ev["state"]))
	}
	return out
}

func realPath(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func launchArgs(l *life, n int) map[string]any { return l.fake.Calls("t3_thread_launch")[n].Args }

func sessionList(t *testing.T, l *life, role string) []any {
	t.Helper()
	res, err := handle(t, l.h, "tok", "t3-agent.sessions", map[string]any{"project": "my-project", "role": role})
	if err != nil {
		t.Fatal(err)
	}
	return res["sessions"].([]any)
}

// ---- not set up ----

func TestT3AgentOpsNameTheSetupStepWhenT3IsNotThere(t *testing.T) {
	l := newLife(t)
	role := putRole(t, l.h, "reviewer")
	// A thread Home started earlier (so the ownership checks pass and the missing T3 is what stops the op).
	now := time.Now()
	if err := l.t3.threads.add(t3Thread{ID: "thr-1", Project: "my-project", Role: role, Mode: "default", Created: now, LastActivity: now}); err != nil {
		t.Fatal(err)
	}
	ops := map[string]map[string]any{
		"start":     {"project": "my-project", "role": role, "prompt": "p"},
		"send":      {"project": "my-project", "role": role, "id": "thr-1", "text": "t"},
		"messages":  {"project": "my-project", "role": role, "id": "thr-1"},
		"rename":    {"project": "my-project", "role": role, "id": "thr-1", "name": "n"},
		"fork":      {"project": "my-project", "role": role, "id": "thr-1"},
		"interrupt": {"project": "my-project", "role": role, "id": "thr-1"},
		"mode":      {"project": "my-project", "role": role, "id": "thr-1", "mode": "default"},
	}
	check := func(want string) {
		t.Helper()
		for op, args := range ops {
			_, err := handle(t, l.h, "tok", "t3-agent."+op, args)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: %v (want it to name %q)", op, err, want)
			}
		}
	}
	check("install step")
	l.install(t)
	check("start step")
	l.srv.alive = true
	l.t3.mu.Lock()
	l.t3.srv = l.srv
	l.t3.mu.Unlock()
	check("pair step")
	// What needs no T3 still answers.
	for _, op := range []string{"projects", "status", "sessions", "role.list", "ssh.list"} {
		args := map[string]any{}
		if op == "sessions" {
			args = map[string]any{"project": "my-project", "role": role}
		}
		if _, err := handle(t, l.h, "tok", "t3-agent."+op, args); err != nil {
			t.Errorf("%s without T3: %v", op, err)
		}
	}
	if len(l.fake.Calls("")) != 0 {
		t.Errorf("T3 was called while it wasn't set up: %v", l.fake.Calls(""))
	}
}

// ---- projects, status, delegation ----

func TestT3AgentProjectsListsTheRoles(t *testing.T) {
	l, role := agentLife(t)
	res, err := handle(t, l.h, "tok", "t3-agent.projects", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := res["projects"].([]any)
	if len(rows) != 1 {
		t.Fatalf("rows: %v", rows)
	}
	r := rows[0].(map[string]any)
	if r["id"] != "my-project" || r["dir"] != realPath(t, l.proj) || r["role"] != role || r["role_name"] != "reviewer" || r["vm_state"] != "stopped" ||
		r["slot_busy"] != false || r["queue"].(float64) != 0 || r["sessions_running"].(float64) != 0 {
		t.Errorf("row: %v", r)
	}
	// With a running session the row says so; with the role's limit reached the slots are busy.
	if err := l.h.Update(func(c *Config) { c.AgentMaxSessionsPerProfile = 1 }); err != nil {
		t.Fatal(err)
	}
	startThread(t, l, role, nil)
	res, _ = handle(t, l.h, "tok", "t3-agent.projects", nil)
	r = res["projects"].([]any)[0].(map[string]any)
	if r["vm_state"] != "running" || r["sessions_running"].(float64) != 1 || r["slot_busy"] != true {
		t.Errorf("row with a session: %v", r)
	}
}

func TestT3AgentStatusCountsT3Sessions(t *testing.T) {
	l, role := agentLife(t)
	st, err := handle(t, l.h, "tok", "t3-agent.status", nil)
	if err != nil {
		t.Fatal(err)
	}
	if st["sessions_running"].(float64) != 0 || st["slot"].(map[string]any)["busy"] != false {
		t.Errorf("idle status: %v", st)
	}
	t3s := st["t3"].(map[string]any)
	if t3s["installed"] != true || t3s["running"] != true || t3s["paired"] != true {
		t.Errorf("t3: %v", t3s)
	}
	id := startThread(t, l, role, map[string]any{"bot": "bot-a"})
	st, _ = handle(t, l.h, "tok", "t3-agent.status", map[string]any{"probe": true}) // a probe would need a runner VM: not started
	slots := st["slots"].([]any)
	if st["sessions_running"].(float64) != 1 || len(slots) != 1 {
		t.Fatalf("status: %v", st)
	}
	s := slots[0].(map[string]any)
	if s["session"] != id || s["role"] != role || s["project"] != "my-project" || s["busy"] != true || s["bot"] != "bot-a" || s["role_name"] != "reviewer" {
		t.Errorf("slot: %v", s)
	}
	if st["slot"].(map[string]any)["session"] != id {
		t.Errorf("first slot for older plugins: %v", st["slot"])
	}
	for _, k := range []string{"limits", "usage", "queue", "login", "usage_today"} {
		if _, ok := st[k]; !ok {
			t.Errorf("status lacks %s", k)
		}
	}
}

func TestT3AgentLimitsUsageModeAndRolesAreTheExistingOnes(t *testing.T) {
	l, role := agentLife(t)
	st, err := handle(t, l.h, "tok", "t3-agent.limits", map[string]any{"per_profile": 3})
	if err != nil || st["limits"].(map[string]any)["per_profile"].(float64) != 3 || l.h.Fresh().AgentMaxSessionsPerProfile != 3 {
		t.Fatalf("limits: %v %v", st, err)
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.limits", map[string]any{}); err == nil {
		t.Error("limits with nothing said")
	}
	st, err = handle(t, l.h, "tok", "t3-agent.usage_mode", map[string]any{"bot": "bot-a", "mode": "light"})
	if err != nil || l.h.Fresh().AgentBotModes["bot-a"] != "light" {
		t.Fatalf("usage_mode: %v %v", st, err)
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.usage_mode", map[string]any{"mode": "bogus", "default": true}); err == nil {
		t.Error("an unknown usage mode")
	}
	list, err := handle(t, l.h, "tok", "t3-agent.role.list", map[string]any{})
	if err != nil || !strings.Contains(fmt.Sprint(list), role) {
		t.Errorf("role.list: %v %v", list, err)
	}
	// The same store as claude-agent: a role made under one prefix is there under the other.
	other, err := handle(t, l.h, "tok", "claude-agent.role.list", map[string]any{})
	if err != nil || fmt.Sprint(other) != fmt.Sprint(list) {
		t.Errorf("claude-agent.role.list differs: %v", other)
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.role.get", map[string]any{"id": role}); err != nil {
		t.Errorf("role.get: %v", err)
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.ssh.list", map[string]any{}); err != nil {
		t.Errorf("ssh.list: %v", err)
	}
	// An unknown op under the prefix is refused.
	if _, err := handle(t, l.h, "tok", "t3-agent.nonsense", nil); err == nil {
		t.Error("an unknown op was served")
	}
}

// After role.put the T3 instances follow the roles.
func TestT3AgentRolePutSyncsTheInstances(t *testing.T) {
	l, _ := agentLife(t)
	second := putRole(t, l.h, "builder")
	b, err := os.ReadFile(filepath.Join(l.h.Fresh().T3BaseDir, "userdata", "settings.json"))
	if err != nil || !strings.Contains(string(b), "claude-"+second) {
		t.Fatalf("the new role's instance is missing: %v %s", err, b)
	}
	if _, err := os.Stat(filepath.Join(l.h.Dir, "t3", "bin", "claude-"+second)); err != nil {
		t.Errorf("shim: %v", err)
	}
}

// Starting a session in a project T3 didn't have turns the role's instance on in that project (the project is made after
// the last sync) and leaves it off globally.
func TestT3AgentStartTurnsTheRoleOnInItsNewProject(t *testing.T) {
	l, role := agentLife(t)
	startThread(t, l, role, nil)
	ps := l.fake.Projects()
	if len(ps) != 1 {
		t.Fatalf("projects: %+v", ps)
	}
	b, err := os.ReadFile(filepath.Join(l.h.Fresh().T3BaseDir, "userdata", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		ProviderInstances        map[string]struct{ Enabled *bool }
		ProjectSettingsOverrides map[string]struct {
			ProviderInstances map[string]struct{ Enabled *bool }
		}
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	id := "claude-" + role
	if e := s.ProviderInstances[id].Enabled; e == nil || *e {
		t.Errorf("the instance globally: %v", e)
	}
	if e := s.ProjectSettingsOverrides[ps[0].ID].ProviderInstances[id].Enabled; e == nil || !*e {
		t.Errorf("the instance in its project: %s", b)
	}
}

// When the role's instance can't be synced at a session start, the session doesn't start: no thread is launched and the
// caller is told why.
func TestT3AgentStartRefusedWhenTheInstanceSyncFails(t *testing.T) {
	l, role := agentLife(t)
	path := filepath.Join(l.h.Fresh().T3BaseDir, "userdata", "settings.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	m["projectSettingsOverrides"] = map[string]any{"elsewhere": map[string]any{"providerInstances": "not an object"}}
	b, _ = json.Marshal(m)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = handle(t, l.h, "tok", "t3-agent.start", map[string]any{"project": "my-project", "role": role, "prompt": "do the work"})
	if err == nil || !strings.Contains(err.Error(), "didn't start") || !strings.Contains(err.Error(), "T3 project") {
		t.Fatalf("start with a failing sync: %v", err)
	}
	if n := len(l.fake.Calls("t3_thread_launch")); n != 0 {
		t.Fatalf("a thread was launched: %d", n)
	}
	if rows := sessionList(t, l, role); len(rows) != 0 {
		t.Errorf("sessions: %v", rows)
	}
}

// A sync that couldn't read T3's projects wrote no project switch: the session doesn't start either.
func TestT3AgentStartRefusedWhenTheSyncKnewNoProjects(t *testing.T) {
	l, role := agentLife(t)
	l.t3.syncInstances = func() (t3instances.SyncResult, error) { return t3instances.SyncResult{}, nil }
	_, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"project": "my-project", "role": role, "prompt": "do the work"})
	if err == nil || !strings.Contains(err.Error(), "didn't start") {
		t.Fatalf("start with no projects known: %v", err)
	}
	if n := len(l.fake.Calls("t3_thread_launch")); n != 0 {
		t.Fatalf("a thread was launched: %d", n)
	}
}

// A tool ask arrives through the same queue as before and decide answers it.
func TestT3AgentNextAndDecide(t *testing.T) {
	l, role := agentLife(t)
	if _, err := handle(t, l.h, "tok", "t3-agent.next", map[string]any{"wait_seconds": 1}); err != nil { // Rubi is present
		t.Fatal(err)
	}
	answer := make(chan agent.AskResult, 1)
	go func() {
		answer <- l.h.Agent.AskTool(context.Background(), "my-project", role, agent.AskParams{SessionID: "thr-9", Tool: "Bash", Via: "hook",
			Input: json.RawMessage(`{"command":"ls"}`), Cwd: l.proj})
	}()
	var ask map[string]any
	for ask == nil {
		res, err := handle(t, l.h, "tok", "t3-agent.next", map[string]any{"wait_seconds": 2})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range res["items"].([]any) {
			if m := it.(map[string]any); m["type"] == "perm.ask" {
				ask = m
			}
		}
	}
	if ask["tool"] != "Bash" || ask["role"] != role || ask["project"] != "my-project" || ask["session_id"] != "thr-9" {
		t.Errorf("ask: %v", ask)
	}
	if res, err := handle(t, l.h, "tok", "t3-agent.decide", map[string]any{"ask_id": ask["ask_id"], "behavior": "allow"}); err != nil || res["ok"] != true {
		t.Fatalf("decide: %v %v", res, err)
	}
	select {
	case r := <-answer:
		if r.Behavior != "allow" {
			t.Errorf("answer %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the ask was not answered")
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.decide", map[string]any{"ask_id": ask["ask_id"], "behavior": "allow"}); err == nil {
		t.Error("an ask was answered twice")
	}
}

// ---- start ----

func TestT3AgentStartLaunchesInTheRolesInstance(t *testing.T) {
	l, role := agentLife(t)
	id := startThread(t, l, role, map[string]any{"prompt": "fix the bug\nsecond line", "from": "Alice", "bot": "alice"})
	calls := l.fake.Calls("t3_thread_launch")
	if len(calls) != 1 {
		t.Fatalf("launches: %d", len(calls))
	}
	a := calls[0].Args
	sel := a["modelSelection"].(map[string]any)
	if sel["instanceId"] != "claude-"+role || sel["model"] != l.t3.Model {
		t.Errorf("model selection %v", sel)
	}
	if a["runtimeMode"] != "approval-required" || a["interactionMode"] != nil {
		t.Errorf("modes: %v %v", a["runtimeMode"], a["interactionMode"])
	}
	if a["message"] != "[message from Alice]\nfix the bug\nsecond line" || a["title"] != "fix the bug" {
		t.Errorf("message %q title %q", a["message"], a["title"])
	}
	if rid, _ := a["clientRequestId"].(string); !strings.HasPrefix(rid, "rubi-") {
		t.Errorf("request id %q", rid)
	}
	// The project is the role's pinned folder, created in T3 on first use.
	ps := l.fake.Projects()
	if len(ps) != 1 || ps[0].Root != realPath(t, l.proj) || ps[0].Title != "my-project" || a["projectId"] != ps[0].ID {
		t.Fatalf("projects: %+v (launch in %v)", ps, a["projectId"])
	}
	cp := l.fake.Calls("t3_project_create")
	if len(cp) != 1 || cp[0].Args["workspaceRoot"] != realPath(t, l.proj) {
		t.Errorf("project create: %v", cp)
	}
	// The session is the T3 thread and shows in the sessions list with the fields the plugin reads.
	rows := sessionList(t, l, role)
	if len(rows) != 1 {
		t.Fatalf("sessions: %v", rows)
	}
	r := rows[0].(map[string]any)
	if r["dir"] != realPath(t, l.proj) {
		t.Errorf("session dir %v", r["dir"])
	}
	if r["id"] != id || r["name"] != "fix the bug" || r["mode"] != "default" || r["busy"] != true {
		t.Errorf("session row: %v", r)
	}
	for _, k := range []string{"created", "last_activity"} {
		if _, err := time.Parse(time.RFC3339, fmt.Sprint(r[k])); err != nil {
			t.Errorf("%s = %v", k, r[k])
		}
	}
	// A second session reuses the project; a name given is the title; the model can be named.
	startThread(t, l, role, map[string]any{"name": "My name", "model": "claude-opus-4-7"})
	if len(l.fake.Calls("t3_project_create")) != 1 {
		t.Error("the project was created twice")
	}
	if a := launchArgs(l, 1); a["title"] != "My name" || a["modelSelection"].(map[string]any)["model"] != "claude-opus-4-7" || a["message"] != "do the work" {
		t.Errorf("second launch: %v", a)
	}
	// The sessions list is per role.
	if other := sessionList(t, l, "someone-else"); len(other) != 0 {
		t.Errorf("another role's list: %v", other)
	}
	// A project that already exists in T3 for the folder is used, not created again.
	l2, role2 := agentLife(t)
	l2.fake.AddProject("p-existing", "mine", l2.proj)
	startThread(t, l2, role2, nil)
	if len(l2.fake.Calls("t3_project_create")) != 0 || launchArgs(l2, 0)["projectId"] != "p-existing" {
		t.Errorf("existing project: %v", launchArgs(l2, 0)["projectId"])
	}
}

func TestT3AgentStartMapsAndClampsTheMode(t *testing.T) {
	l, role := agentLife(t)
	for _, c := range []struct{ mode, runtime, interaction, session string }{
		{"", "approval-required", "", "default"},
		{"default", "approval-required", "", "default"},
		{"acceptEdits", "auto-accept-edits", "", "acceptEdits"},
		{"auto", "auto", "", "auto"},
		{"dontAsk", "auto", "", "auto"},
		{"bypassPermissions", "full-access", "", "bypassPermissions"},
		{"plan", "approval-required", "plan", "plan"},
	} {
		id := startThread(t, l, role, map[string]any{"mode": c.mode})
		rt, inter := l.fake.Mode(id)
		if rt != c.runtime || inter != c.interaction {
			t.Errorf("mode %q: T3 got %q/%q, want %q/%q", c.mode, rt, inter, c.runtime, c.interaction)
		}
		var got string
		for _, r := range sessionList(t, l, role) {
			if m := r.(map[string]any); m["id"] == id {
				got = m["mode"].(string)
			}
		}
		if got != c.session {
			t.Errorf("mode %q: the session says %q, want %q", c.mode, got, c.session)
		}
	}
	n := len(l.fake.Calls("t3_thread_launch"))
	if _, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"project": "my-project", "role": role, "prompt": "p", "mode": "yolo"}); err == nil ||
		!strings.Contains(err.Error(), "unknown mode") {
		t.Errorf("unknown mode: %v", err)
	}
	if len(l.fake.Calls("t3_thread_launch")) != n {
		t.Error("an unknown mode launched a thread")
	}
}

// The role's ceiling and the bridge's both cap the mode.
func TestT3AgentModeCeilings(t *testing.T) {
	l, role := agentLife(t)
	sel, eff, claude, err := l.t3.clampMode("bypassPermissions", "auto")
	if err != nil || sel.Runtime != "full-access" || eff != t3.Auto || claude != "auto" {
		t.Errorf("role ceiling auto: %+v %q %q %v", sel, eff, claude, err)
	}
	_, eff, claude, _ = l.t3.clampMode("acceptEdits", "auto")
	if eff != t3.AutoAcceptEdits || claude != "acceptEdits" {
		t.Errorf("a lower mode stays: %q %q", eff, claude)
	}
	l.t3.Ceiling = t3.AutoAcceptEdits
	_, eff, claude, _ = l.t3.clampMode("bypassPermissions", "full-access")
	if eff != t3.AutoAcceptEdits || claude != "acceptEdits" {
		t.Errorf("bridge ceiling: %q %q", eff, claude)
	}
	_, eff, claude, _ = l.t3.clampMode("plan", "full-access")
	if eff != t3.ApprovalRequired || claude != "plan" {
		t.Errorf("plan: %q %q", eff, claude)
	}

	// End to end: a bridge whose client has a lower ceiling sends the lower mode, and the session says so.
	l2 := newLife(t)
	role2 := putRole(t, l2.h, "reviewer")
	l2.t3.Ceiling = t3.Auto
	l2.up(t)
	id := startThread(t, l2, role2, map[string]any{"mode": "bypassPermissions"})
	if rt, _ := l2.fake.Mode(id); rt != "auto" {
		t.Errorf("T3 was asked for %q under an auto ceiling", rt)
	}
	if h := l2.fake.Calls("t3_thread_launch")[0].Header.Get("t3-mode-limit"); h != "auto/default" {
		t.Errorf("mode limit %q", h)
	}
	if rows := sessionList(t, l2, role2); rows[0].(map[string]any)["mode"] != "auto" {
		t.Errorf("session mode: %v", rows[0])
	}
	_ = role
}

func TestT3AgentBypassFollowsTheSwitch(t *testing.T) {
	l := newLife(t)
	role := putRole(t, l.h, "reviewer")
	l.h.Agent.Close()
	l.h.Agent = agent.New(agent.Options{Dir: l.h.Dir, Projects: func() []string { return l.h.Fresh().AgentProjects },
		GitHosts: func() []string { return nil }, NewVM: func(agent.Project) agent.VM { return &agent.Fake{} }, Wait: 5 * time.Second,
		AllowBypass: func() bool { return false }})
	t.Cleanup(l.h.Agent.Close)
	l.up(t)
	for _, m := range []string{"bypassPermissions", "dontAsk"} {
		if _, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"project": "my-project", "role": role, "prompt": "p", "mode": m}); err == nil ||
			!strings.Contains(err.Error(), "switched off") {
			t.Errorf("%s with bypass off: %v", m, err)
		}
	}
	if len(l.fake.Calls("t3_thread_launch")) != 0 {
		t.Error("a refused mode launched a thread")
	}
	id := startThread(t, l, role, map[string]any{"mode": "auto"})
	if rt, _ := l.fake.Mode(id); rt != "auto" {
		t.Errorf("auto: %q", rt)
	}
	r, err := l.h.Agent.T3Role(role)
	if err != nil {
		t.Fatal(err)
	}
	r.Release()
	if r.MaxMode != "auto" {
		t.Errorf("role ceiling %q with bypass off", r.MaxMode)
	}
}

func TestT3AgentStartRefusals(t *testing.T) {
	l, role := agentLife(t)
	cases := map[string]struct {
		args map[string]any
		want string
	}{
		"empty prompt":  {map[string]any{"project": "my-project", "role": role, "prompt": "  "}, "prompt is empty"},
		"no role":       {map[string]any{"project": "my-project", "prompt": "p"}, "runs in a role"},
		"unknown role":  {map[string]any{"project": "my-project", "role": "r_zzzzzzzz", "prompt": "p"}, "unknown role"},
		"bad role id":   {map[string]any{"project": "my-project", "role": "../x", "prompt": "p"}, "bad role id"},
		"other project": {map[string]any{"project": "elsewhere", "role": role, "prompt": "p"}, "doesn't belong to project"},
		"bad bot":       {map[string]any{"project": "my-project", "role": role, "prompt": "p", "bot": "bad bot!"}, "bad bot id"},
		"bad model":     {map[string]any{"project": "my-project", "role": role, "prompt": "p", "model": "x y"}, "bad model"},
		"long name":     {map[string]any{"project": "my-project", "role": role, "prompt": "p", "name": strings.Repeat("n", 65)}, "too long"},
	}
	for name, c := range cases {
		if _, err := handle(t, l.h, "tok", "t3-agent.start", c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (want %q)", name, err, c.want)
		}
	}
	if n := len(l.fake.Calls("t3_thread_launch")); n != 0 {
		t.Errorf("%d launches for refused starts", n)
	}
	// A T3 that refuses is an error, and no session is kept.
	l.fake.Fail("t3_thread_launch", "no room")
	if _, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"project": "my-project", "role": role, "prompt": "p"}); err == nil || !strings.Contains(err.Error(), "no room") {
		t.Errorf("T3 refused: %v", err)
	}
	if rows := sessionList(t, l, role); len(rows) != 0 {
		t.Errorf("a session was recorded for a failed start: %v", rows)
	}
}

// A project folder T3 puts elsewhere than asked is not used.
func TestT3AgentStartChecksTheProjectRoot(t *testing.T) {
	l, role := agentLife(t)
	l.fake.WrongRoot = t.TempDir()
	if _, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"project": "my-project", "role": role, "prompt": "p"}); err == nil ||
		!strings.Contains(err.Error(), "different folder") {
		t.Fatalf("%v", err)
	}
	if len(l.fake.Calls("t3_thread_launch")) != 0 {
		t.Error("launched in the wrong folder")
	}
}

func TestT3AgentLimits(t *testing.T) {
	l, role := agentLife(t)
	if err := l.h.Update(func(c *Config) { c.AgentMaxSessionsPerProfile = 1 }); err != nil {
		t.Fatal(err)
	}
	id := startThread(t, l, role, nil)
	_, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"project": "my-project", "role": role, "prompt": "p"})
	if err == nil || !strings.Contains(err.Error(), "already runs 1 session") {
		t.Fatalf("second start under a limit of 1: %v", err)
	}
	if n := len(l.fake.Calls("t3_thread_launch")); n != 1 {
		t.Errorf("%d launches", n)
	}
	// Once the turn has finished the place is free again.
	l.fake.Complete(id, "completed", "done")
	nextItems(t, l.h, 3)
	startThread(t, l, role, nil)

	// The total on the computer and a Bot's own cap hold too.
	total := 1
	if err := l.h.Update(func(c *Config) { c.AgentMaxSessionsPerProfile, c.AgentMaxSessions = 20, &total }); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"project": "my-project", "role": role, "prompt": "p"}); err == nil ||
		!strings.Contains(err.Error(), "on this computer") {
		t.Errorf("total limit: %v", err)
	}
	zero := 0
	if err := l.h.Update(func(c *Config) { c.AgentMaxSessions = &zero; c.AgentBotModes = map[string]string{"bot-a": "light"} }); err != nil {
		t.Fatal(err)
	}
	startThread(t, l, role, map[string]any{"bot": "bot-a"})
	if _, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"project": "my-project", "role": role, "prompt": "p", "bot": "bot-a"}); err == nil ||
		!strings.Contains(err.Error(), "this Bot already runs") {
		t.Errorf("a Bot's limit: %v", err)
	}
}

// ---- send ----

func TestT3AgentSend(t *testing.T) {
	l, role := agentLife(t)
	id := startThread(t, l, role, nil)
	l.fake.Complete(id, "completed", "first")
	nextItems(t, l.h, 3)

	res, err := handle(t, l.h, "tok", "t3-agent.send", map[string]any{"project": "my-project", "role": role, "id": id, "text": "and more",
		"from": "Alice", "bot": "alice", "message_id": "msg-77"})
	if err != nil {
		t.Fatal(err)
	}
	if res["queued"] != true || res["delivered"] != true || res["position"].(float64) != 0 || res["id"] != "msg-77" {
		t.Errorf("send result: %v", res)
	}
	a := l.fake.Calls("t3_thread_send")[0].Args
	if a["threadId"] != id || a["message"] != "[message from Alice]\nand more" || a["clientRequestId"] != "msg-77" || a["mode"] != nil {
		t.Errorf("send args: %v", a)
	}
	// The same message id is one message.
	if _, err := handle(t, l.h, "tok", "t3-agent.send", map[string]any{"project": "my-project", "role": role, "id": id, "text": "and more",
		"from": "Alice", "message_id": "msg-77"}); err != nil {
		t.Fatal(err)
	}
	if got := len(l.fake.Runs(id)); got != 2 {
		t.Errorf("a repeated message id started %d runs", got)
	}
	// The thread is busy again, then finishes: the result answers the sender and names the messages.
	drain(t, l.h)
	l.fake.Complete(id, "completed", "second")
	r := resultOf(t, nextItems(t, l.h, 2))
	if r["text"] != "second" || r["to"] != "Alice" {
		t.Errorf("result: %v", r)
	}
	if got := fmt.Sprint(r["in_reply_to"]); got != "[msg-77 msg-77]" {
		t.Errorf("in_reply_to %s", got)
	}

	// A steer goes as T3's steer mode; a queued delivery is not "delivered".
	l.fake.SendDelivery = "queued"
	res, _ = handle(t, l.h, "tok", "t3-agent.send", map[string]any{"project": "my-project", "role": role, "id": id, "text": "now!", "steer": true})
	calls := l.fake.Calls("t3_thread_send")
	if calls[len(calls)-1].Args["mode"] != "steer" || res["delivered"] != false {
		t.Errorf("steer: %v %v", calls[len(calls)-1].Args, res)
	}
	if rid, _ := calls[len(calls)-1].Args["clientRequestId"].(string); !strings.HasPrefix(rid, "rubi-") {
		t.Errorf("made-up request id %q", rid)
	}
}

func TestT3AgentSendRefusals(t *testing.T) {
	l, role := agentLife(t)
	id := startThread(t, l, role, nil)
	l.fake.Complete(id, "completed", "x")
	nextItems(t, l.h, 3)
	for name, c := range map[string]struct {
		args map[string]any
		want string
	}{
		"empty":           {map[string]any{"role": role, "id": id, "text": " "}, "text is empty"},
		"bad id":          {map[string]any{"role": role, "id": "a b", "text": "t"}, "bad session id"},
		"bad bot":         {map[string]any{"role": role, "id": id, "text": "t", "bot": "x y"}, "bad bot id"},
		"not Home's":      {map[string]any{"role": role, "id": "thr-from-the-users-own-t3", "text": "t"}, "isn't one of the T3 sessions"},
		"another role":    {map[string]any{"role": "other-role", "id": id, "text": "t"}, "doesn't belong"},
		"another project": {map[string]any{"project": "elsewhere", "role": role, "id": id, "text": "t"}, "doesn't belong"},
	} {
		if _, err := handle(t, l.h, "tok", "t3-agent.send", c.args); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (want %q)", name, err, c.want)
		}
	}
	if n := len(l.fake.Calls("t3_thread_send")); n != 0 {
		t.Errorf("%d sends for refused messages", n)
	}
	// A limit holds a send that would start a turn.
	if err := l.h.Update(func(c *Config) { c.AgentMaxSessionsPerProfile = 1 }); err != nil {
		t.Fatal(err)
	}
	other := startThread(t, l, role, nil)
	if _, err := handle(t, l.h, "tok", "t3-agent.send", map[string]any{"role": role, "id": id, "text": "t"}); err == nil || !strings.Contains(err.Error(), "already runs") {
		t.Errorf("send over the limit: %v", err)
	}
	_ = other
	// T3 refusing is an error.
	if err := l.h.Update(func(c *Config) { c.AgentMaxSessionsPerProfile = 20 }); err != nil {
		t.Fatal(err)
	}
	l.fake.Fail("t3_thread_send", "thread is gone")
	if _, err := handle(t, l.h, "tok", "t3-agent.send", map[string]any{"role": role, "id": id, "text": "t"}); err == nil || !strings.Contains(err.Error(), "thread is gone") {
		t.Errorf("T3 refused: %v", err)
	}
}

// ---- the finished turn ----

func TestT3AgentFinishedTurnBecomesAnItem(t *testing.T) {
	l, role := agentLife(t)
	id := startThread(t, l, role, map[string]any{"from": "Alice", "bot": "alice"})
	first := nextItems(t, l.h, 1)
	if got := kinds(first); len(got) != 1 || got[0] != "event:status:busy" {
		t.Fatalf("after start: %v", got)
	}
	if first[0].SessionID != id || first[0].Project != "my-project" || first[0].Role != role {
		t.Errorf("item: %+v", first[0])
	}
	launchReq := launchArgs(l, 0)["clientRequestId"]

	l.fake.Complete(id, "completed", "All done, tests pass.")
	items := nextItems(t, l.h, 2)
	if got := kinds(items); strings.Join(got, ",") != "event:result:<nil>,event:status:idle" {
		t.Fatalf("items: %v", got)
	}
	r := resultOf(t, items)
	if r["text"] != "All done, tests pass." || r["to"] != "Alice" || r["is_error"] != false || r["subtype"] != "success" || r["truncated"] != false ||
		r["session_id"] != id || r["_project"] != "my-project" || r["_role"] != role || r["_session"] != id {
		t.Errorf("result: %v", r)
	}
	if got := fmt.Sprint(r["in_reply_to"]); got != "["+fmt.Sprint(launchReq)+"]" {
		t.Errorf("in_reply_to %s", got)
	}
	// It reads like a runner event for the plugin: the nested event carries the fields it flattens.
	var it agent.Item = items[0]
	if it.Type != "event" || it.At.IsZero() {
		t.Errorf("item: %+v", it)
	}
	rows := sessionList(t, l, role)
	if rows[0].(map[string]any)["busy"] != false {
		t.Errorf("still busy: %v", rows[0])
	}
	// Nothing more arrives for a turn that is over.
	if more, _ := l.h.Agent.Next(context.Background(), 200*time.Millisecond); len(more) != 0 {
		t.Errorf("extra items: %v", kinds(more))
	}
}

func TestT3AgentFinishedTurnVariants(t *testing.T) {
	l, role := agentLife(t)

	// A user's own session answers nobody in particular.
	id := startThread(t, l, role, nil)
	drain(t, l.h)
	l.fake.Complete(id, "completed", "ok")
	if r := resultOf(t, nextItems(t, l.h, 2)); r["to"] != "local:user" {
		t.Errorf("to = %v", r["to"])
	}

	// A bot with no "from" is answered by its id; a failed run is an error; an interrupted one has no text.
	id2 := startThread(t, l, role, map[string]any{"bot": "bot-a"})
	drain(t, l.h)
	l.fake.Complete(id2, "failed", "")
	r := resultOf(t, nextItems(t, l.h, 2))
	if r["to"] != "bot-a" || r["is_error"] != true || r["subtype"] != "failed" || r["text"] != "" {
		t.Errorf("failed: %v", r)
	}
	id3 := startThread(t, l, role, nil)
	drain(t, l.h)
	if _, err := handle(t, l.h, "tok", "t3-agent.interrupt", map[string]any{"role": role, "id": id3}); err != nil {
		t.Fatal(err)
	}
	r = resultOf(t, nextItems(t, l.h, 2))
	if r["subtype"] != "interrupted" || r["is_error"] != false || r["text"] != "" {
		t.Errorf("interrupted: %v", r)
	}

	// Long text is cut and says so.
	id4 := startThread(t, l, role, nil)
	drain(t, l.h)
	l.fake.Complete(id4, "completed", strings.Repeat("é", maxResultRunes+50))
	r = resultOf(t, nextItems(t, l.h, 2))
	if r["truncated"] != true || len([]rune(r["text"].(string))) != maxResultRunes {
		t.Errorf("long result: truncated %v, %d runes", r["truncated"], len([]rune(fmt.Sprint(r["text"]))))
	}
}

// Messages sent while a turn runs make more runs; each one that ends is reported, and the thread stays busy until the
// last.
func TestT3AgentOneItemPerRun(t *testing.T) {
	l, role := agentLife(t)
	id := startThread(t, l, role, map[string]any{"from": "Alice"})
	drain(t, l.h)
	if _, err := handle(t, l.h, "tok", "t3-agent.send", map[string]any{"role": role, "id": id, "text": "also this", "from": "Bob"}); err != nil {
		t.Fatal(err)
	}
	runs := l.fake.Runs(id)
	if len(runs) != 2 {
		t.Fatalf("runs: %v", runs)
	}
	l.fake.CompleteRun(id, runs[0], "completed", "first answer")
	items := nextItems(t, l.h, 1)
	r := resultOf(t, items)
	if r["text"] != "first answer" || r["run_id"] != runs[0] {
		t.Errorf("first: %v", r)
	}
	if strings.Contains(strings.Join(kinds(items), ","), "idle") {
		t.Error("the thread went idle with a newer run still going")
	}
	if rows := sessionList(t, l, role); rows[0].(map[string]any)["busy"] != true {
		t.Errorf("not busy with a run going: %v", rows[0])
	}
	l.fake.CompleteRun(id, runs[1], "completed", "second answer")
	items = nextItems(t, l.h, 2)
	r = resultOf(t, items)
	if r["text"] != "second answer" || r["to"] != "Bob" || r["run_id"] != runs[1] {
		t.Errorf("second: %v", r)
	}
	if !strings.Contains(strings.Join(kinds(items), ","), "status:idle") {
		t.Errorf("no idle after the last run: %v", kinds(items))
	}
}

// ---- messages ----

func TestT3AgentMessages(t *testing.T) {
	l, role := agentLife(t)
	id := startThread(t, l, role, map[string]any{"prompt": "hello there", "from": "Alice"})
	l.fake.AddItem(id, "user_message", "typed in T3")
	l.fake.AddItem(id, "tool_call", "ignored")
	l.fake.Complete(id, "completed", "General Kenobi")
	nextItems(t, l.h, 3)
	res, err := handle(t, l.h, "tok", "t3-agent.messages", map[string]any{"project": "my-project", "role": role, "id": id})
	if err != nil {
		t.Fatal(err)
	}
	msgs := res["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages: %v", msgs)
	}
	text := func(i int) (typ, role, body string) {
		m := msgs[i].(map[string]any)
		inner := m["message"].(map[string]any)
		switch c := inner["content"].(type) {
		case string:
			body = c
		case []any:
			body = c[0].(map[string]any)["text"].(string)
		}
		return m["type"].(string), inner["role"].(string), body
	}
	if ty, ro, b := text(0); ty != "user" || ro != "user" || b != "[message from Alice]\nhello there" {
		t.Errorf("0: %s %s %q", ty, ro, b)
	}
	if ty, ro, b := text(1); ty != "user" || ro != "user" || b != "typed in T3" {
		t.Errorf("1: %s %s %q", ty, ro, b)
	}
	if ty, ro, b := text(2); ty != "assistant" || ro != "assistant" || b != "General Kenobi" {
		t.Errorf("2: %s %s %q", ty, ro, b)
	}
	if msgs[0].(map[string]any)["session_id"] != id {
		t.Errorf("message: %v", msgs[0])
	}
	// limit keeps the last ones
	res, _ = handle(t, l.h, "tok", "t3-agent.messages", map[string]any{"role": role, "id": id, "limit": 2})
	if got := res["messages"].([]any); len(got) != 2 || got[1].(map[string]any)["type"] != "assistant" {
		t.Errorf("limit: %v", got)
	}
	// only Home's own sessions
	if _, err := handle(t, l.h, "tok", "t3-agent.messages", map[string]any{"role": role, "id": "thr-not-ours"}); err == nil {
		t.Error("read a thread Home didn't start")
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.messages", map[string]any{"role": "other-role", "id": id}); err == nil {
		t.Error("read a thread through another role")
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.messages", map[string]any{"id": "bad id!"}); err == nil {
		t.Error("a bad id was accepted")
	}
}

// ---- rename, fork, interrupt, close, mode ----

func TestT3AgentRenameForkInterruptClose(t *testing.T) {
	l, role := agentLife(t)
	id := startThread(t, l, role, map[string]any{"mode": "acceptEdits", "bot": "bot-a"})
	l.fake.Complete(id, "completed", "done")
	nextItems(t, l.h, 3)

	// rename
	if res, err := handle(t, l.h, "tok", "t3-agent.rename", map[string]any{"role": role, "id": id, "name": " Better name "}); err != nil || res["ok"] != true {
		t.Fatalf("rename: %v %v", res, err)
	}
	a := l.fake.Calls("t3_thread_update")[0].Args
	if a["action"] != "rename" || a["threadId"] != id || a["title"] != "Better name" || l.fake.Title(id) != "Better name" {
		t.Errorf("update args: %v", a)
	}
	if rows := sessionList(t, l, role); rows[0].(map[string]any)["name"] != "Better name" {
		t.Errorf("name in the list: %v", rows[0])
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.rename", map[string]any{"role": role, "id": id, "name": "  "}); err == nil {
		t.Error("an empty name")
	}

	// fork: a new session of the same role and mode, history already seen
	res, err := handle(t, l.h, "tok", "t3-agent.fork", map[string]any{"role": role, "id": id, "name": "the fork"})
	if err != nil {
		t.Fatal(err)
	}
	fid, _ := res["id"].(string)
	tr := l.fake.Calls("t3_thread_transfer")[0].Args
	if fid == "" || fid == id || tr["action"] != "fork" || tr["threadId"] != id || tr["title"] != "the fork" {
		t.Errorf("fork: %v %v", res, tr)
	}
	rows := sessionList(t, l, role)
	if len(rows) != 2 {
		t.Fatalf("sessions: %v", rows)
	}
	f := rows[1].(map[string]any)
	if f["id"] != fid || f["name"] != "the fork" || f["mode"] != "acceptEdits" || f["busy"] != false {
		t.Errorf("fork row: %v", f)
	}
	th, _ := l.t3.threads.get(fid)
	if th.LastPos == 0 || th.Bot != "bot-a" {
		t.Errorf("fork record: %+v", th)
	}
	// The fork's first turn reports only its own answer.
	if _, err := handle(t, l.h, "tok", "t3-agent.send", map[string]any{"role": role, "id": fid, "text": "go on"}); err != nil {
		t.Fatal(err)
	}
	drain(t, l.h)
	l.fake.Complete(fid, "completed", "fork answer")
	if r := resultOf(t, nextItems(t, l.h, 2)); r["text"] != "fork answer" || r["session_id"] != fid {
		t.Errorf("fork result: %v", r)
	}

	// interrupt
	third := startThread(t, l, role, nil)
	if res, err := handle(t, l.h, "tok", "t3-agent.interrupt", map[string]any{"role": role, "id": third}); err != nil || res["ok"] != true {
		t.Fatalf("interrupt: %v %v", res, err)
	}
	if got := l.fake.Calls("t3_thread_interrupt"); len(got) != 1 || got[0].Args["threadId"] != third {
		t.Errorf("interrupt calls: %v", got)
	}

	// close: interrupts a running turn, leaves the list, takes no more messages
	fourth := startThread(t, l, role, nil)
	n := len(l.fake.Calls("t3_thread_interrupt"))
	if res, err := handle(t, l.h, "tok", "t3-agent.close", map[string]any{"role": role, "id": fourth}); err != nil || res["ok"] != true {
		t.Fatalf("close: %v %v", res, err)
	}
	if len(l.fake.Calls("t3_thread_interrupt")) != n+1 {
		t.Error("a running turn was not interrupted by close")
	}
	for _, r := range sessionList(t, l, role) {
		if r.(map[string]any)["id"] == fourth {
			t.Error("a closed session is still listed")
		}
	}
	for _, op := range []string{"send", "interrupt", "rename", "fork"} {
		_, err := handle(t, l.h, "tok", "t3-agent."+op, map[string]any{"role": role, "id": fourth, "text": "t", "name": "n"})
		if err == nil || !strings.Contains(err.Error(), "is closed") {
			t.Errorf("%s on a closed session: %v", op, err)
		}
	}
	// closing again is fine, and so is closing with T3 gone
	if _, err := handle(t, l.h, "tok", "t3-agent.close", map[string]any{"role": role, "id": fourth}); err != nil {
		t.Errorf("close twice: %v", err)
	}
	l.t3.Stop()
	if _, err := handle(t, l.h, "tok", "t3-agent.close", map[string]any{"role": role, "id": id}); err != nil {
		t.Errorf("close without T3: %v", err)
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.close", map[string]any{"role": role, "id": "thr-unknown"}); err == nil {
		t.Error("closed a thread Home doesn't know")
	}
}

func TestT3AgentMode(t *testing.T) {
	l, role := agentLife(t)
	id := startThread(t, l, role, map[string]any{"mode": "acceptEdits"})
	// The mode it has is confirmed.
	if res, err := handle(t, l.h, "tok", "t3-agent.mode", map[string]any{"role": role, "id": id, "mode": "acceptEdits"}); err != nil || res["ok"] != true {
		t.Fatalf("same mode: %v %v", res, err)
	}
	// A different one can't be set: T3 keeps a session's permission mode. The error says why and what to do.
	_, err := handle(t, l.h, "tok", "t3-agent.mode", map[string]any{"role": role, "id": id, "mode": "bypassPermissions"})
	if err == nil || !strings.Contains(err.Error(), "can't change the permission mode") || !strings.Contains(err.Error(), "acceptEdits") {
		t.Errorf("a different mode: %v", err)
	}
	if rows := sessionList(t, l, role); rows[0].(map[string]any)["mode"] != "acceptEdits" {
		t.Errorf("mode changed in the list: %v", rows[0])
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.mode", map[string]any{"role": role, "id": id, "mode": "plan"}); err == nil {
		t.Error("plan accepted for a session that isn't planning")
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.mode", map[string]any{"role": role, "id": id}); err == nil || !strings.Contains(err.Error(), "valid mode") {
		t.Errorf("no mode: %v", err)
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.mode", map[string]any{"role": role, "id": id, "mode": "yolo"}); err == nil {
		t.Error("an unknown mode")
	}
	// A planning session keeps planning.
	pid := startThread(t, l, role, map[string]any{"mode": "plan"})
	if _, err := handle(t, l.h, "tok", "t3-agent.mode", map[string]any{"role": role, "id": pid, "mode": "plan"}); err != nil {
		t.Errorf("plan on a planning session: %v", err)
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.mode", map[string]any{"role": role, "id": pid, "mode": "default"}); err == nil {
		t.Error("default accepted on a planning session")
	}
}

func TestT3ThreadListSurvivesARestart(t *testing.T) {
	l, role := agentLife(t)
	id := startThread(t, l, role, map[string]any{"name": "keep me", "bot": "bot-a"})
	th2 := newT3Home(l.h)
	got, ok := th2.threads.get(id)
	if !ok || got.Name != "keep me" || got.Bot != "bot-a" || got.Role != role || got.Project != "my-project" || !got.Running {
		t.Errorf("thread after a restart: %+v %v", got, ok)
	}
	// A file that isn't ours is moved aside, not trusted.
	if err := os.WriteFile(th2.threads.path, []byte(`{"version":9}`), 0o600); err != nil {
		t.Fatal(err)
	}
	th3 := newT3Home(l.h)
	if _, ok := th3.threads.get(id); ok {
		t.Error("an unknown file version was read")
	}
	if _, err := os.Stat(th3.threads.path + ".bad"); err != nil {
		t.Errorf("not moved aside: %v", err)
	}
}

// A typed brief with standing orders is long; the whole of it goes to T3.
func TestT3AgentStartTakesALongBrief(t *testing.T) {
	l, role := agentLife(t)
	brief := strings.Repeat("x", 4000)
	startThread(t, l, role, map[string]any{"prompt": brief})
	if got := launchArgs(l, 0)["message"]; got != brief {
		t.Errorf("message of %d characters", len(fmt.Sprint(got)))
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"project": "my-project", "role": role, "prompt": strings.Repeat("x", agent.MaxText+1)}); err == nil {
		t.Error("a prompt over the limit was accepted")
	}
}
