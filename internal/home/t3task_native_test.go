package home

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	server := &t3.Server{Home: t.TempDir(), Node: node, Bin: launcher, BaseDir: filepath.Join(t.TempDir(), "t3"), Port: port, RuntimeHome: t.TempDir()}
	t.Cleanup(server.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
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
	l.t3.TaskSupervisor = nil // Probe the REAL helper, not the fake-native unit seam.
	result, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"role": role, "prompt": "Edit task-edit.txt and commit", "provider": "codex", "model": "gpt-5.4", "task": true, "mode": "bypassPermissions", "auto_finish": true, "from": "requester"})
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
	nativeThread, _, nativeErr := t3.New(server.Base(), token, t3.FullAccess).Read(ctx, id, 0)
	if nativeErr != nil || nativeThread.RuntimeMode != string(t3.FullAccess) {
		t.Fatal("native T3 did not apply the explicitly approved autonomous mode")
	}
	event := nativeTaskResult(t, l.h)
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
	stopped := nativeTaskResult(t, l.h)
	if stopped["to"] != "phone" || stopped["task"] != taskID || (stopped["subtype"] != "cancelled" && stopped["subtype"] != "interrupted") {
		t.Fatal("stop result lost task/requester binding")
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.close", map[string]any{"id": id}); err != nil {
		t.Fatal(err)
	}
	failedStart, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"role": role, "prompt": "fail-fixture", "provider": "codex", "model": "gpt-5.4", "task": true, "mode": "bypassPermissions", "auto_finish": true, "from": "failure-requester"})
	if err != nil {
		t.Fatal(err)
	}
	failed := nativeTaskResult(t, l.h)
	if failed["to"] != "failure-requester" || failed["is_error"] != true || failed["task"] != failedStart["task"] {
		t.Fatal("failure lost requester/task binding")
	}
	if len(l.t3.threads.snapshot()) != 2 {
		t.Fatal("failure created an extra thread")
	}
	completed, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"role": role, "prompt": "Edit task-edit.txt and commit", "provider": "codex", "model": "gpt-5.4", "task": true, "mode": "bypassPermissions", "auto_finish": true, "from": "completion-requester"})
	if err != nil {
		t.Fatal(err)
	}
	completedEvent := nativeTaskResult(t, l.h)
	if completedEvent["task"] != completed["task"] || completedEvent["is_error"] == true {
		t.Fatal("completed task result missing")
	}
	finished, err := handle(t, l.h, "tok", "t3-agent.task.publish", map[string]any{"id": completed["task"]})
	if err != nil || finished["phase"] != "finished" {
		t.Fatal("native task could not be durably finalized", err)
	}
	state, err := l.t3.taskStore().Get(ctx, completed["task"].(string))
	if err != nil || state.Commit == "" || state.Phase != agenttask.Finished {
		t.Fatal("native finalization not persisted", err)
	}
	again, err := handle(t, l.h, "tok", "t3-agent.task.publish", map[string]any{"id": completed["task"]})
	if err != nil || again["revision"] != finished["revision"] {
		t.Fatal("native finalization retry not idempotent", err)
	}
	// Lose an ACCEPTED launch reply before Home learns a native thread ID. The
	// actual process still edits/runs inside real bwrap+srt, not a mocked cancel.
	lost := &lostTaskReply{base: http.DefaultTransport, beforeMask: func() {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			states, _ := l.t3.taskStore().List(ctx)
			for _, state := range states {
				if state.Spec.From != "lost-reply-requester" {
					continue
				}
				w, e := agenttask.Resolve(l.h.Dir, state.ID, "my-project", role, roles.DirHash(l.proj))
				if e == nil {
					if b, e := os.ReadFile(filepath.Join(w.Dir, "fixture-heartbeat")); e == nil && len(b) > 0 {
						return
					}
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Error("accepted native launch never started a real sandbox helper")
	}}
	l.t3.setClient(t3.NewWithTransport(server.Base(), token, t3.FullAccess, lost))
	_, err = handle(t, l.h, "tok", "t3-agent.start", map[string]any{"role": role, "prompt": "hold-fixture", "provider": "codex", "model": "gpt-5.4", "task": true, "mode": "bypassPermissions", "auto_finish": true, "from": "lost-reply-requester", "message_id": "lost-native-reply"})
	if err == nil || !lost.dropped.Load() {
		t.Fatal("fixture did not lose an accepted native receipt")
	}
	states, _ := l.t3.taskStore().List(ctx)
	var unknown agenttask.State
	for _, s := range states {
		if s.Spec.From == "lost-reply-requester" {
			unknown = s
		}
	}
	if unknown.ID == "" || unknown.Thread != "" {
		t.Fatal("unknown launch fixture not persisted")
	}
	unknownWorkspace, e := agenttask.Resolve(l.h.Dir, unknown.ID, "my-project", role, roles.DirHash(l.proj))
	if e != nil {
		t.Fatal(e)
	}
	heartbeat := filepath.Join(unknownWorkspace.Dir, "fixture-heartbeat")
	deadline = time.Now().Add(20 * time.Second)
	for {
		if b, e := os.ReadFile(heartbeat); e == nil && len(b) > 0 {
			break
		}
		if time.Now().After(deadline) {

			entries, _ := os.ReadDir(unknownWorkspace.Dir)
			for _, entry := range entries {
				t.Log("fixture name:", entry.Name())
			}
			t.Fatal("unknown sandboxed worker did not run")
		}
		time.Sleep(20 * time.Millisecond)
	}
	stoppedUnknown, err := handle(t, l.h, "tok", "t3-agent.task.stop", map[string]any{"id": unknown.ID})
	if err != nil || stoppedUnknown["stopped"] != true {
		t.Fatal("unknown worker not confirmed stopped", err)
	}
	before, e := os.ReadFile(heartbeat)
	if e != nil {
		t.Fatal(e)
	}
	time.Sleep(300 * time.Millisecond)
	after, e := os.ReadFile(heartbeat)
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("detached sandbox descendant kept executing AFTER confirmed stop")
	}
	if _, e := l.t3.taskStore().Admit(ctx, unknown.ID); e == nil {
		t.Fatal("late launcher ignored stop fence")
	}
	t.Log("accepted native reply loss: no thread ID; real sandbox worker and detached heartbeat stop before acknowledgement; late launch refused")

	t.Log("real Home RPC -> native T3 -> task shim -> real srt -> fake Codex edits/commits; source and outside fixtures unchanged; network deny, mid-turn append, stop, requester-only failure and durable/idempotent local finalization verified")
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// Test-only transport: consume one successful launch reply, then emulate a lost
// connection. Never prints auth headers, pairing data, or native credentials.
type lostTaskReply struct {
	beforeMask func()
	base       http.RoundTripper
	dropped    atomic.Bool
}

func (l *lostTaskReply) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body == nil {
		return l.base.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &envelope)
	resp, err := l.base.RoundTrip(req)
	if err == nil && envelope.Method == "tools/call" && envelope.Params.Name == "t3_thread_launch" && l.dropped.CompareAndSwap(false, true) {
		if l.beforeMask != nil {
			l.beforeMask()
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		// Mask the accepted result with an RPC error, not a transport teardown:
		// closing the MCP session itself can cancel the native launch before it runs.
		replacement := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"fixture: accepted launch receipt unavailable"}}`, envelope.ID))
		resp.Header.Set("Content-Type", "application/json")
		resp.Header.Del("Content-Encoding")
		resp.Header.Del("Content-Length")
		resp.ContentLength = int64(len(replacement))
		resp.Body = io.NopCloser(bytes.NewReader(replacement))
		return resp, nil
	}
	return resp, err
}

func nativeTaskResult(t *testing.T, h *Helper) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		items, err := h.Agent.Next(context.Background(), 300*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			var ev map[string]any
			if it.Type == "event" && json.Unmarshal(it.Event, &ev) == nil && ev["kind"] == "result" {
				return ev
			}
		}
	}
	t.Fatal("no native task result before deadline")
	return nil
}
