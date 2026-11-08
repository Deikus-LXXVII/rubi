package home

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
	"github.com/Deikus-LXXVII/rubi/internal/agenttask"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
)

func taskGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("/usr/bin/git", append([]string{"-c", "core.hooksPath=/dev/null", "-C", dir}, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git: %v", err)
	}
	return strings.TrimSpace(string(out))
}
func initTaskRepo(t *testing.T, dir string) {
	t.Helper()
	taskGit(t, dir, "init", "-b", "main")
	taskGit(t, dir, "config", "user.name", "Fixture")
	taskGit(t, dir, "config", "user.email", "fixture@localhost")
	if err := os.WriteFile(filepath.Join(dir, "base"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	taskGit(t, dir, "add", "base")
	taskGit(t, dir, "commit", "-m", "base")
}
func TestT3TaskStartPersistsPrivateCopyAndInstance(t *testing.T) {
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	head := taskGit(t, l.proj, "rev-parse", "HEAD")
	res, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"project": "my-project", "role": role, "prompt": "work", "task": true, "from": "requester"})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res["id"].(string)
	task, _ := res["task"].(string)
	if id == "" || !agenttask.ValidID(task) {
		t.Fatal("no task/session binding")
	}
	th, ok := l.t3.threads.get(id)
	if !ok || th.Task != task || th.Branch != res["branch"] {
		t.Fatal("thread lost workspace binding")
	}
	w, err := agenttask.Resolve(l.h.Dir, task, "my-project", role, roles.DirHash(l.proj))
	if err != nil {
		t.Fatal(err)
	}
	listed, err := handle(t, l.h, "tok", "t3-agent.sessions", map[string]any{"role": role})
	if err != nil {
		t.Fatal(err)
	}
	rows := listed["sessions"].([]any)
	if len(rows) != 1 {
		t.Fatal("task session missing")
	}
	row := rows[0].(map[string]any)
	if row["dir"] != w.Dir || row["task"] != task || row["branch"] != w.Branch {
		t.Fatal("task listing points to the source checkout")
	}
	if taskGit(t, w.Dir, "rev-parse", "HEAD") != head || taskGit(t, w.Dir, "remote") != "" {
		t.Fatal("wrong copy/remote")
	}
	cfg := filepath.Join(l.h.Dir, "t3", "sandbox", role, "claudeAgent", "tasks", task, "binding.json")
	var b agentsandbox.Binding
	data, err := os.ReadFile(cfg)
	if err != nil || json.Unmarshal(data, &b) != nil || b.Task != task {
		t.Fatal("task shim missing", err)
	}
	if err := l.h.Update(func(c *Config) {
		c.AgentSandbox = agentsandbox.Installation{SRT: "/usr/bin/true", SearchPath: []string{"/usr/bin"}, Providers: map[string]agentsandbox.Provider{"claudeAgent": {CLI: "/usr/bin/true"}}}
	}); err != nil {
		t.Fatal(err)
	}
	launch, err := ResolveAgentLaunch(b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(launch.Policy.Projects) != 1 || launch.Policy.Projects[0] != w.Dir || launch.Cwd != w.Dir {
		t.Fatal("provider still writes enrolled checkout")
	}
	if _, err := agentsandbox.Build(launch.Policy, append(launch.Protected, cfg)); err != nil {
		t.Fatal(err)
	}
	if taskGit(t, l.proj, "rev-parse", "HEAD") != head {
		t.Fatal("source branch changed")
	}
	// The same persisted task is revalidated before append/stop/reconnect.
	th.Project = "another-project"
	if _, err := l.t3.readyForThread(context.Background(), th); err == nil {
		t.Fatal("task migrated to another project")
	}
}
func TestT3TaskStartRefusesBeforeCreatingAThread(t *testing.T) {
	l, role := agentLife(t)
	_, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"role": role, "prompt": "work", "task": true})
	if err == nil {
		t.Fatal("non-repository task accepted")
	}
	if len(l.t3.threads.snapshot()) != 0 || len(l.fake.Calls("t3_thread_launch")) != 0 {
		t.Fatal("failure created a T3 thread")
	}
}

func TestT3TaskForkKeepsBindingAndStopSurvivesDamagedCopy(t *testing.T) {
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	id := startThread(t, l, role, map[string]any{"task": true})
	parent, _ := l.t3.threads.get(id)
	fork, err := handle(t, l.h, "tok", "t3-agent.fork", map[string]any{"role": role, "id": id})
	if err != nil {
		t.Fatal(err)
	}
	child, _ := l.t3.threads.get(fork["id"].(string))
	if child.Task != parent.Task || child.Branch != parent.Branch {
		t.Fatal("fork escaped task binding")
	}
	w, err := agenttask.Resolve(l.h.Dir, parent.Task, parent.Project, role, roles.DirHash(l.proj))
	if err != nil {
		t.Fatal(err)
	}
	taskGit(t, w.Dir, "config", "alias.changed", "!false")
	if _, err := handle(t, l.h, "tok", "t3-agent.send", map[string]any{"id": id, "text": "more"}); err == nil {
		t.Fatal("damaged binding resumed")
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.interrupt", map[string]any{"id": id}); err != nil {
		t.Fatal("damaged copy prevented stop", err)
	}
}

func TestT3NativeOpaqueThreadIDSupportsPhoneControls(t *testing.T) {
	l, role := agentLife(t)
	l.fake.ThreadIDPrefix = "mcp:launch"
	id := startThread(t, l, role, nil)
	if !strings.Contains(id, ":") {
		t.Fatal("fixture did not reproduce native ID")
	}
	for _, op := range []string{"messages", "send", "rename", "interrupt", "close"} {
		args := map[string]any{"id": id, "text": "append", "name": "renamed", "from": "requester"}
		if _, err := handle(t, l.h, "tok", "t3-agent."+op, args); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.interrupt", map[string]any{"id": "mcp:launch:not-owned"}); err == nil {
		t.Fatal("opaque ID bypassed thread ownership")
	}
	for _, bad := range []string{"", "mcp:launch:\nforged", strings.Repeat("x", 513)} {
		if validT3ThreadID(bad) {
			t.Fatal("invalid opaque ID accepted")
		}
	}
}
