package home

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/Deikus-LXXVII/rubi/internal/t3"
	"github.com/Deikus-LXXVII/rubi/internal/t3/t3test"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/agenttask"
)

func TestT3TaskStartRecoveryReusesWorkspaceAndNativeReceipt(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	args := map[string]any{"project": "my-project", "role": role, "prompt": "work", "task": true, "from": "phone", "message_id": "stable-native-receipt"}
	l.fake.Fail("t3_thread_launch", "temporary launch failure")
	if _, err := handle(t, l.h, "tok", "t3-agent.start", args); err == nil {
		t.Fatal("fixture launch did not fail")
	}
	states, err := l.t3.taskStore().List(context.Background())
	if err != nil || len(states) != 1 || states[0].Phase != agenttask.Starting {
		t.Fatal("launch intent was not durable", err)
	}
	l.fake.Fail("t3_thread_launch", "")
	l.t3.recoverTasks(context.Background())
	states, _ = l.t3.taskStore().List(context.Background())
	if len(states) != 1 || states[0].ID == "" || states[0].Thread == "" || states[0].Phase != agenttask.Running {
		t.Fatal("pending launch not recovered")
	}
	calls := l.fake.Calls("t3_thread_launch")
	if len(calls) != 2 || calls[0].Args["clientRequestId"] != calls[1].Args["clientRequestId"] {
		t.Fatal("recovery changed native idempotency receipt")
	}
	result, err := handle(t, l.h, "tok", "t3-agent.start", args)
	if err != nil || result["task"] != states[0].ID || len(l.fake.Calls("t3_thread_launch")) != 2 {
		t.Fatal("retry duplicated task", err)
	}
	workspaces, err := agenttask.List(l.h.Dir)
	if err != nil || len(workspaces) != 1 {
		t.Fatal("recovery created another workspace", err)
	}
}
func TestT3TaskRecoveryReconcilesPersistedNativeOutcome(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	for _, status := range []string{"completed", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			l, role := agentLife(t)
			initTaskRepo(t, l.proj)
			id := startThread(t, l, role, map[string]any{"task": true})
			l.t3.stopWatch(id)
			th, _ := l.t3.threads.update(id, func(x *t3Thread) { x.Running = false; x.RunID = ""; x.TaskOutcome = status })
			// A fresh thread cache observes only what was durably written before restart.
			restarted := newT3Home(l.h)
			restarted.recoverTasks(context.Background())
			state, err := l.t3.taskStore().Get(context.Background(), th.Task)
			want := agenttask.Waiting
			if status == "failed" {
				want = agenttask.Failed
			}
			if status == "interrupted" {
				want = agenttask.Stopped
			}
			if err != nil || state.Phase != want {
				t.Fatal("restart lost native completion", state.Phase, err)
			}
			revision := state.Revision
			l.t3.recoverTasks(context.Background())
			state, _ = l.t3.taskStore().Get(context.Background(), th.Task)
			if state.Revision != revision {
				t.Fatal("recovery not idempotent")
			}
		})
	}
}
func TestT3TaskStopSealsAllForksAndAppendCannotReviveIt(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	id := startThread(t, l, role, map[string]any{"task": true})
	parent, _ := l.t3.threads.get(id)
	if _, err := handle(t, l.h, "tok", "t3-agent.fork", map[string]any{"id": id}); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.task.stop", map[string]any{"id": parent.Task}); err != nil {
		t.Fatal(err)
	}
	state, _ := l.t3.taskStore().Get(context.Background(), parent.Task)
	if state.Phase != agenttask.Stopped {
		t.Fatal("stop not terminal")
	}
	revision := state.Revision
	deadline := time.Now().Add(3 * time.Second)
	for {
		running := false
		for _, th := range l.t3.threads.snapshot() {
			if th.Task == parent.Task && th.Running {
				running = true
			}
		}
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fork remained running")
		}
		time.Sleep(10 * time.Millisecond)
	}
	calls := len(l.fake.Calls("t3_thread_send"))
	if _, err := handle(t, l.h, "tok", "t3-agent.send", map[string]any{"id": id, "text": "revive"}); err == nil || len(l.fake.Calls("t3_thread_send")) != calls {
		t.Fatal("terminal append reached provider")
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.task.stop", map[string]any{"id": parent.Task}); err != nil {
		t.Fatal(err)
	}
	state, _ = l.t3.taskStore().Get(context.Background(), parent.Task)
	if state.Revision != revision {
		t.Fatal("duplicate stop changed terminal state")
	}
}
func TestT3TaskAmbiguousForkFailsClosedOnRecovery(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	id := startThread(t, l, role, map[string]any{"task": true})
	th, _ := l.t3.threads.get(id)
	err := l.t3.taskStore().With(context.Background(), th.Task, func(tx *agenttask.Tx) error {
		tx.State.Pending = &agenttask.Command{Kind: "fork", Thread: id, Request: "unknown-fork"}
		return tx.Save()
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := len(l.fake.Calls("t3_thread_transfer"))
	l.t3.recoverTasks(context.Background())
	state, _ := l.t3.taskStore().Get(context.Background(), th.Task)
	if state.Phase != agenttask.Failed || len(l.fake.Calls("t3_thread_transfer")) != calls {
		t.Fatal("unknown native fork duplicated")
	}
}
func TestT3TaskCredentialHelperCannotComeFromProjectOrTaskCopy(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	// Existing executable inside the enrolled directory is still agent-writable.
	if err := os.WriteFile(filepath.Join(l.proj, "helper"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := l.h.Update(func(c *Config) { c.AgentSandbox.AgentExec = filepath.Join(l.proj, "helper") }); err != nil {
		t.Fatal(err)
	}
	if _, err := l.t3.publisherHelper(); err == nil {
		t.Fatal("project-writable credential helper accepted")
	}
	id := startThread(t, l, role, map[string]any{"task": true})
	th, _ := l.t3.threads.get(id)
	var record agenttask.State
	data, err := handle(t, l.h, "tok", "t3-agent.task.status", map[string]any{"id": th.Task})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(data)
	_ = json.Unmarshal(b, &record)
	if record.Spec.Proof != "" || record.Spec.Prompt != "" {
		t.Fatal("status leaks private mandate")
	}
}

func TestT3TaskRevokedProjectCannotPublishAndRecoversFailed(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	id := startThread(t, l, role, map[string]any{"task": true})
	th, _ := l.t3.threads.get(id)
	l.t3.stopWatch(id)
	_, _ = l.t3.threads.update(id, func(x *t3Thread) { x.Running = false; x.TaskOutcome = "completed" })
	_, _ = l.t3.taskStore().Outcome(context.Background(), th.Task, id, agenttask.Waiting)
	if err := l.h.Update(func(c *Config) { c.AgentProjects = nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.task.publish", map[string]any{"id": th.Task}); err == nil {
		t.Fatal("revoked project published")
	}
	l.t3.recoverTasks(context.Background())
	state, _ := l.t3.taskStore().Get(context.Background(), th.Task)
	if state.Phase != agenttask.Failed {
		t.Fatal("revoked binding not terminal after recovery")
	}
}

func TestT3TaskPublisherUsesAdjacentPackagedHelperAndRefusesMissing(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	l, _ := agentLife(t)
	install := t.TempDir()
	helper := filepath.Join(install, "rubi-agent-exec")
	if err := l.h.Update(func(c *Config) { c.AgentSandbox.AgentExec = "" }); err != nil {
		t.Fatal(err)
	}
	if _, err := l.t3.publisherHelperFrom(filepath.Join(install, "rubi-home")); err == nil {
		t.Fatal("missing adjacent helper accepted")
	}
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	resolved, err := l.t3.publisherHelperFrom(filepath.Join(install, "rubi-home"))
	want, _ := filepath.EvalSymlinks(helper)
	if err != nil || resolved != want {
		t.Fatal("packaged adjacent helper not resolved", err)
	}
}

func TestT3TaskOlderFailureCannotEndNewerRunOrLoseReplies(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	id := startThread(t, l, role, map[string]any{"task": true, "from": "Alice"})
	drain(t, l.h)
	if _, err := handle(t, l.h, "tok", "t3-agent.send", map[string]any{"id": id, "text": "new work", "from": "Bob", "message_id": "new-reply"}); err != nil {
		t.Fatal(err)
	}
	runs := l.fake.Runs(id)
	if len(runs) != 2 {
		t.Fatal("fixture did not create two runs")
	}
	l.fake.CompleteRun(id, runs[0], "failed", "old failure")
	_ = resultOf(t, nextItems(t, l.h, 1))
	th, _ := l.t3.threads.get(id)
	s, _ := l.t3.taskStore().Get(context.Background(), th.Task)
	if !th.Running || s.Phase != agenttask.Running || len(th.ReplyTo) == 0 {
		t.Fatal("stale failure ended new work or erased reply receipt")
	}
	l.fake.CompleteRun(id, runs[1], "completed", "new answer")
	r := resultOf(t, nextItems(t, l.h, 2))
	if r["text"] != "new answer" {
		t.Fatal("stale completion skipped new answer")
	}
	replies, _ := r["in_reply_to"].([]any)
	found := false
	for _, reply := range replies {
		if reply == "new-reply" {
			found = true
		}
	}
	if !found {
		t.Fatal("new requester reply receipt lost")
	}
	s, _ = l.t3.taskStore().Get(context.Background(), th.Task)
	if s.Phase != agenttask.Waiting {
		t.Fatal("new successful turn not waiting")
	}
}

func TestT3TaskStopCannotRaceAnInflightNativeLaunch(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	target, _ := url.Parse(l.fake.URL())
	proxy := httputil.NewSingleHostReverseProxy(target)
	entered, release := make(chan struct{}), make(chan struct{})
	var once, unblock sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(b))
			if bytes.Contains(b, []byte("t3_thread_launch")) {
				once.Do(func() { close(entered) })
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	defer unblock.Do(func() { close(release) })
	l.t3.setClient(t3.New(server.URL, t3.Token{Access: t3test.Token, Expires: time.Now().Add(time.Hour), Scopes: t3.PairingScopes("full-access"), ClientID: "fixture"}, l.t3.Ceiling))
	started := make(chan error, 1)
	go func() {
		_, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"project": "my-project", "role": role, "task": true, "prompt": "work", "from": "phone", "message_id": "launch-race"})
		started <- err
	}()
	select {
	case <-entered:
	case err := <-started:
		t.Fatal("launch did not reach held RPC", err)
	case <-time.After(5 * time.Second):
		t.Fatal("native launch not reached")
	}
	states, err := l.t3.taskStore().List(context.Background())
	if err != nil || len(states) != 1 {
		t.Fatal("missing launch intent", err)
	}
	id := states[0].ID
	stopped := make(chan error, 1)
	go func() { _, err := l.t3.stopTask(context.Background(), id); stopped <- err }()
	select {
	case <-stopped:
		t.Fatal("stop returned before inflight admission resolved")
	case <-time.After(20 * time.Millisecond):
	}
	unblock.Do(func() { close(release) })
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	state, err := l.t3.taskStore().Get(context.Background(), id)
	if err != nil || state.Phase != agenttask.Stopped {
		t.Fatal("late launch revived stopped task", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		th, _ := l.t3.threads.get(state.Thread)
		if !th.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("late launch was not interrupted")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func TestT3TaskStoppedCursorCannotRestartAProjectServer(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	w, err := agenttask.Prepare(context.Background(), l.h.Dir, l.proj, "my-project", role)
	if err != nil {
		t.Fatal(err)
	}
	spec := agenttask.Spec{Supervision: 1, Project: w.Project, Role: w.Role, Prompt: "work", From: "phone", MessageID: "cursor-restart", Provider: "cursor"}
	if err := l.t3.taskStore().Create(context.Background(), w, spec); err != nil {
		t.Fatal(err)
	}
	th := t3Thread{ID: "cursor-thread", Task: w.ID, Branch: w.Branch, Provider: "cursor", Environment: "old-private-server", Project: w.Project, Role: w.Role, Running: true, RunID: "old-run"}
	if err := l.t3.threads.add(th); err != nil {
		t.Fatal(err)
	}
	_, _ = l.t3.taskStore().Outcome(context.Background(), w.ID, th.ID, agenttask.Running)
	if _, err := l.t3.stopTask(context.Background(), w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.t3.readyForTaskWatch(context.Background(), th); err == nil || !strings.Contains(err.Error(), "owned project server is not running") {
		t.Fatal("ended task attempted Cursor provisioning/restart", err)
	}
	current, _ := l.t3.threads.get(th.ID)
	if current.Running {
		t.Fatal("dead owned server left stopped Cursor task busy")
	}
}

func TestT3TaskUnknownLaunchCancellationNeverClaimsAConfirmedInterruption(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	l.fake.Fail("t3_thread_launch", "lost response")
	if _, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"project": "my-project", "role": role, "task": true, "prompt": "work", "message_id": "uncertain-launch"}); err == nil {
		t.Fatal("fixture launch did not fail")
	}
	states, _ := l.t3.taskStore().List(context.Background())
	if len(states) != 1 {
		t.Fatal("missing intent")
	}
	id := states[0].ID
	lease, err := l.t3.taskStore().Admit(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	cancelCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err = l.t3.stopTask(cancelCtx, id); err == nil {
		t.Fatal("stop acknowledged a live unknown launcher")
	}
	<-lease.Context().Done()
	lease.Close()
	if _, err = l.t3.stopTask(context.Background(), id); err != nil {
		t.Fatal("drained unknown launch not confirmed", err)
	}
	if _, err = l.t3.taskStore().Admit(context.Background(), id); err == nil {
		t.Fatal("cancelled late launch admitted")
	}
	state, _ := l.t3.taskStore().Get(context.Background(), id)
	if state.Phase != agenttask.Stopped || state.TerminalAt.IsZero() {
		t.Fatal("cancellation mandate not durable")
	}
	calls := len(l.fake.Calls("t3_thread_launch"))
	l.fake.Fail("t3_thread_launch", "")
	l.t3.recoverTasks(context.Background())
	if len(l.fake.Calls("t3_thread_launch")) != calls {
		t.Fatal("cancelled ambiguous launch replayed new work")
	}
}

func TestT3TaskSupervisorStopJoinsWatchersBeforeReturning(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	id := startThread(t, l, role, map[string]any{"task": true})
	l.t3.mu.Lock()
	watch := l.t3.watch[id]
	l.t3.mu.Unlock()
	if watch == nil {
		t.Fatal("no active native watcher")
	}
	l.t3.Stop()
	select {
	case <-watch.done:
	default:
		t.Fatal("supervisor returned while watcher could still write thread/task state")
	}
	l.t3.mu.Lock()
	remaining := len(l.t3.watch)
	l.t3.mu.Unlock()
	if remaining != 0 {
		t.Fatal("supervisor retained active watchers")
	}

	state, err := l.t3.taskStore().Get(context.Background(), l.t3.threads.snapshot()[0].Task)
	if err != nil || state.Phase != agenttask.Running {
		t.Fatal("supervisor shutdown incorrectly completed active task", err)
	}
}

func TestT3TaskReplacingWatcherKeepsNewGenerationOwned(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	id := startThread(t, l, role, map[string]any{"task": true})
	for i := 0; i < 10; i++ {
		l.t3.mu.Lock()
		old := l.t3.watch[id]
		l.t3.mu.Unlock()
		if old == nil {
			t.Fatal("running task lost its watcher")
		}
		l.t3.stopWatch(id)
		l.t3.startWatch(id)
		l.t3.mu.Lock()
		replacement := l.t3.watch[id]
		l.t3.mu.Unlock()
		if replacement == nil || replacement == old {
			t.Fatal("new watch not created")
		}
		select {
		case <-old.done:
		case <-time.After(3 * time.Second):
			t.Fatal("cancelled watch did not end")
		}
		l.t3.mu.Lock()
		current := l.t3.watch[id]
		l.t3.mu.Unlock()
		if current != replacement {
			t.Fatal("old watcher removed the new generation")
		}
	}
}

func TestT3TaskEndedEventIsExplicitlyRequesterAddressedAndRedacted(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	l, role := agentLife(t)
	id := "task_0123456789abcdef01234567"
	s := agenttask.State{Version: 1, ID: id, Workspace: agenttask.Workspace{ID: id, Project: "my-project", Role: role, Branch: "rubi/" + id}, Spec: agenttask.Spec{Project: "my-project", Role: role, From: "requester-only", Bot: "bot-one", MessageID: "original-message", Prompt: "private task prompt", Proof: "private mandate proof"}, Phase: agenttask.Failed, Thread: "owned-thread"}
	l.t3.emitTaskResult(s)
	items := nextItems(t, l.h, 1)
	found := false
	for _, item := range items {
		var event map[string]any
		if json.Unmarshal(item.Event, &event) != nil || event["kind"] != "task_result" {
			continue
		}
		found = true
		if event["to"] != "requester-only" || event["event_id"] != id+":terminal" || event["bot"] != "bot-one" {
			t.Fatal("ended result could fall back to broad delivery")
		}
		replies, _ := event["in_reply_to"].([]any)
		if len(replies) != 1 || replies[0] != "original-message" {
			t.Fatal("ended result lost original request binding")
		}
		if strings.Contains(string(item.Event), "private task prompt") || strings.Contains(string(item.Event), "private mandate proof") {
			t.Fatal("ended event exposed private state")
		}
	}
	if !found {
		t.Fatal("no ended event delivered")
	}
}

func TestT3TaskStatusReportCannotFinalizeANewerAppend(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	id := startThread(t, l, role, map[string]any{"task": true, "auto_finish": true, "mode": "bypassPermissions", "from": "requester"})
	th, _ := l.t3.threads.get(id)
	l.fake.Complete(id, "completed", "Work done\nSTATUS: PASS\nCOMMIT: none\nEVIDENCE: fixture tests passed")
	deadline := time.Now().Add(3 * time.Second)
	for {
		v, _ := l.t3.threads.get(id)
		if !v.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("turn did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	out, err := handle(t, l.h, "tok", "t3-agent.task.status", map[string]any{"id": th.Task})
	if err != nil {
		t.Fatal(err)
	}
	summary := out["summary"].(map[string]any)
	if summary["status"] != "PASS" {
		t.Fatal("explicit completion report missing")
	}
	if _, err = handle(t, l.h, "tok", "t3-agent.task.append", map[string]any{"id": th.Task, "text": "one more change", "from": "phone"}); err != nil {
		t.Fatal(err)
	}
	out, err = handle(t, l.h, "tok", "t3-agent.task.status", map[string]any{"id": th.Task})
	if err != nil {
		t.Fatal(err)
	}
	summary = out["summary"].(map[string]any)
	if summary["status"] == "PASS" {
		t.Fatal("stale PASS finalized a newer turn")
	}
	if _, err = handle(t, l.h, "tok", "t3-agent.task.stop", map[string]any{"id": th.Task}); err != nil {
		t.Fatal(err)
	}
	if _, err = handle(t, l.h, "tok", "t3-agent.task.append", map[string]any{"id": th.Task, "text": "restart", "from": "phone"}); err == nil {
		t.Fatal("ended mandate revived")
	}
}
func TestT3TaskRejectsLegacyHelperBeforeCreatingACopy(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	l.t3.TaskSupervisor = nil
	helper := filepath.Join(t.TempDir(), "old-helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := l.h.Update(func(c *Config) { c.AgentSandbox.AgentExec = helper }); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"task": true, "role": role, "prompt": "work"}); err == nil {
		t.Fatal("old untracked helper allowed")
	}
	states, _ := l.t3.taskStore().List(context.Background())
	if len(states) != 0 || len(l.fake.Calls("t3_thread_launch")) != 0 {
		t.Fatal("refusal created a task/native thread")
	}
}

func TestT3TaskQuiescentNativeCleanupFailureDoesNotConsumeCapacity(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	id := startThread(t, l, role, map[string]any{"task": true})
	th, _ := l.t3.threads.get(id)
	l.fake.Fail("t3_thread_interrupt", "native server temporarily unavailable")
	out, err := l.t3.stopTask(context.Background(), th.Task)
	if err != nil || out.(map[string]any)["native_cleanup_pending"] != true {
		t.Fatal("proved stop confused with native cleanup", err)
	}
	current, _ := l.t3.threads.get(id)
	if current.Running || !current.NativeCleanupPending {
		t.Fatal("quiescent task retained running capacity or lost cleanup intent")
	}
	l.fake.Fail("t3_thread_interrupt", "")
	l.t3.recoverTasks(context.Background())
	current, _ = l.t3.threads.get(id)
	if current.Running || current.NativeCleanupPending {
		t.Fatal("restart reconciliation lost native cleanup intent")
	}
}

func TestT3AutomaticTaskRefusesInteractiveOrClampedModeBeforeCreatingWork(t *testing.T) {
	if assertUnsupportedTaskPlatform(t) {
		return
	}
	l, role := agentLife(t)
	initTaskRepo(t, l.proj)
	for _, mode := range []string{"", "bypassPermissions"} {
		if mode != "" {
			l.t3.Ceiling = t3.AutoAcceptEdits
		}
		_, err := handle(t, l.h, "tok", "t3-agent.start", map[string]any{"role": role, "task": true, "auto_finish": true, "mode": mode, "prompt": "work"})
		if err == nil {
			t.Fatal("interactive/clamped unattended task accepted")
		}
		states, _ := l.t3.taskStore().List(context.Background())
		if len(states) != 0 || len(l.fake.Calls("t3_thread_launch")) != 0 {
			t.Fatal("unattended mode refusal created work")
		}
	}
}
