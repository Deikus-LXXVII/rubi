package home

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/agentsandbox"
	"github.com/Deikus-LXXVII/rubi/internal/agenttask"
	"github.com/Deikus-LXXVII/rubi/internal/home/agent/roles"
	"github.com/Deikus-LXXVII/rubi/internal/t3"
)

// The Home RPC, native T3 MCP/orchestration, shim, srt, git and commit are real.
// Only the model provider is a synthetic app-server; no account is used.
func TestNativeT3TaskCopyCommitsThroughSandboxedCodex(t *testing.T) {
	native, relay := os.Getenv("RUBI_REAL_T3"), os.Getenv("RUBI_REAL_RELAY")
	if native == "" || relay == "" {
		t.Skip("set RUBI_REAL_T3 and RUBI_REAL_RELAY for disposable native task proof")
	}
	srt, err := exec.LookPath("srt")
	if err != nil {
		t.Fatal("srt missing")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("Node missing")
	}
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	head := taskGit(t, l.proj, "rev-parse", "HEAD")
	tools := t.TempDir()
	fake := filepath.Join(tools, "codex")
	script, err := os.ReadFile("testdata/task-codex.py")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fake, script, 0700); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	secret := filepath.Join(l.h.Dir, "fake-secret")
	marker := filepath.Join(other, "fixture-b")
	for _, p := range []string{secret, marker} {
		if err := os.WriteFile(p, []byte("fixture only"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	paths, _ := json.Marshal([]string{secret, marker, filepath.Join(l.proj, "base")})
	if err := os.WriteFile(filepath.Join(tools, "probe-paths.json"), paths, 0600); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(tools, "t3-launcher.cjs")
	js := "const c=require('child_process').spawn(" + string(mustJSON(native)) + ",process.argv.slice(2),{stdio:'inherit'});c.on('exit',x=>process.exit(x??1));"
	if err := os.WriteFile(launcher, []byte(js), 0600); err != nil {
		t.Fatal(err)
	}
	port, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	server := &t3.Server{Node: node, Bin: launcher, BaseDir: filepath.Join(t.TempDir(), "t3"), Port: port, RuntimeHome: t.TempDir()}
	t.Cleanup(server.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := server.Start(ctx); err != nil {
		t.Fatal("native T3 start failed (details suppressed)")
	}
	code, err := server.PairingCode(ctx, t3.PairingScopes("full-access"), time.Minute, "disposable task")
	if err != nil {
		t.Fatal("pairing code failed")
	}
	token, err := t3.Pair(ctx, http.DefaultClient, server.Base(), code, "full-access", "disposable task")
	if err != nil {
		t.Fatal("native pairing failed")
	}
	cl := t3.New(server.Base(), token, t3.FullAccess)
	l.t3.setClient(cl)
	real, err := filepath.EvalSymlinks(srt)
	if err != nil {
		t.Fatal(err)
	}
	install := agentsandbox.Installation{AgentExec: relay, SRT: srt, RuntimeRead: []string{filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(real))))}, Providers: map[string]agentsandbox.Provider{"codex": {CLI: fake, RuntimeRead: []string{tools}}}}
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(p) {
			if st, e := os.Stat(p); e == nil && st.IsDir() {
				install.SearchPath = append(install.SearchPath, p)
				if strings.HasPrefix(p, "/workspace/") {
					install.RuntimeRead = append(install.RuntimeRead, p)
				}
			}
		}
	}
	if err := l.h.Update(func(c *Config) {
		c.T3Port = port
		c.T3BaseDir = server.BaseDir
		c.AgentSandbox = install
		c.AgentProjects = append(c.AgentProjects, other)
	}); err != nil {
		t.Fatal(err)
	}
	result, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"role": role, "prompt": "Edit task-edit.txt and commit", "provider": "codex", "model": "gpt-5.4", "task": true, "from": "requester"})
	if err != nil {
		t.Fatal(err)
	}
	taskID, _ := result["task"].(string)
	id, _ := result["id"].(string)
	w, err := agenttask.Resolve(l.h.Dir, taskID, "my-project", role, roles.DirHash(l.proj))
	if err != nil {
		t.Fatal(err)
	}
	if taskGit(t, w.Dir, "rev-parse", "HEAD") == head {
		deadline := time.Now().Add(50 * time.Second)
		for time.Now().Before(deadline) {
			th, _ := l.t3.threads.get(id)
			if !th.Running || taskGit(t, w.Dir, "rev-parse", "HEAD") != head {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if taskGit(t, w.Dir, "rev-parse", "HEAD") == head {
		_, items, _ := cl.Read(ctx, id, 0)
		for _, item := range items {
			if item.Type != "message" {
				t.Log("fixture native item", item.Type, item.Text)
			}
		}
		if b, e := os.ReadFile(filepath.Join(w.Dir, "fixture-methods")); e == nil {
			t.Log("fixture methods", string(b))
		}
		if b, e := os.ReadFile(filepath.Join(w.Dir, "fixture-error")); e == nil {
			t.Log("fixture failure category", string(b))
		}
		t.Fatal("native T3 did not produce a sandboxed provider commit; inspect disposable T3 state only")
	}
	for _, p := range []string{secret, marker} {
		b, e := os.ReadFile(p)
		if e != nil || string(b) != "fixture only" {
			t.Fatal("outside host fixture modified")
		}
	}
	if b, e := os.ReadFile(filepath.Join(l.proj, "base")); e != nil || string(b) != "base" {
		t.Fatal("source file modified")
	}
	if taskGit(t, l.proj, "rev-parse", "HEAD") != head {
		t.Fatal("source HEAD changed")
	}
	if _, err := os.Stat(filepath.Join(l.proj, "task-edit.txt")); !os.IsNotExist(err) {
		t.Fatal("source checkout edited")
	}
	event := resultOf(t, nextItems(t, l.h, 3))
	if event["to"] != "requester" || event["task"] != taskID {
		t.Fatal("result lost requester/task binding")
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.send", map[string]any{"id": id, "text": "hold-fixture", "from": "phone"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, e := os.Stat(filepath.Join(w.Dir, "fixture-held")); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native provider did not start held turn")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.send", map[string]any{"id": id, "text": "append from phone", "from": "phone", "steer": true}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(filepath.Join(w.Dir, "fixture-methods"))
		if strings.Contains(string(b), "turn/steer") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native provider did not receive mid-turn append")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.interrupt", map[string]any{"id": id}); err != nil {
		t.Fatal(err)
	}
	stopped := resultOf(t, nextItems(t, l.h, 3))
	if stopped["to"] != "phone" || stopped["task"] != taskID || (stopped["subtype"] != "cancelled" && stopped["subtype"] != "interrupted") {
		t.Fatal("stop result lost task/requester binding")
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.close", map[string]any{"id": id}); err != nil {
		t.Fatal(err)
	}
	failedStart, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"role": role, "prompt": "fail-fixture", "provider": "codex", "model": "gpt-5.4", "task": true, "from": "failure-requester"})
	if err != nil {
		t.Fatal(err)
	}
	failed := resultOf(t, nextItems(t, l.h, 3))
	if failed["to"] != "failure-requester" || failed["is_error"] != true || failed["task"] != failedStart["task"] {
		t.Fatal("failure lost requester/task binding")
	}
	if len(l.t3.threads.snapshot()) != 2 {
		t.Fatal("failure created an extra thread")
	}
	t.Log("real Home RPC -> native T3 -> task shim -> real srt -> fake Codex edits/commits; source and outside fixtures unchanged; network deny, mid-turn append, stop and requester-only failure verified")
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
