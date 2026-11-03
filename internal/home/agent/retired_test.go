package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What the claude-code plugin's users are told; the plugin's own wording and Home's must stay the same.
const retiredWording = "The agent path moved to the T3 Agents plugin (id t3agents): use t3_start, t3_role_start, t3_send, t3_fork, t3_mode. " +
	"Roles, the skill library and SSH grants carry over; bindings, project trusts and mandates are granted again in T3 Agents."

// The Agent SDK runner path is retired: it needed a stored token and an auth gateway. start, send, fork and mode answer a plain
// message that points to the T3 Agents plugin, and nothing is started or queued; reading what is there still works.
func TestRunnerPathRefusesStartSendAndFork(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.AllowRunnerSessions = false })
	for op, args := range map[string]map[string]any{
		"start": {"project": "my-project", "prompt": "do it"},
		"send":  {"project": "my-project", "id": "s-1", "text": "more"},
		"fork":  {"project": "my-project", "id": "s-1"},
		"mode":  {"project": "my-project", "id": "s-1", "mode": "default"},
	} {
		_, err := e.op(op, args)
		if err == nil || err.Error() != retiredWording {
			t.Errorf("%s: %v", op, err)
		}
	}
	if e.fake.State() != StateStopped && e.fake.State() != "" {
		t.Errorf("a VM was started for a refused op: %s", e.fake.State())
	}
	if e.m.q.len() != 0 {
		t.Errorf("a refused start was queued")
	}
	if _, err := os.Stat(filepath.Join(e.dir, QueueFile)); err == nil {
		t.Error("a queue file was written")
	}
}

func TestRunnerPathStillReadsAndManages(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.AllowRunnerSessions = false })
	if res, err := e.op("projects", nil); err != nil || res["projects"] == nil {
		t.Errorf("projects: %v %v", res, err)
	}
	if res, err := e.op("status", nil); err != nil || res["slots"] == nil {
		t.Errorf("status: %v %v", res, err)
	}
	if _, err := e.op("limits", map[string]any{"per_profile": 2}); err != nil && !strings.Contains(err.Error(), "can't be changed here") {
		t.Errorf("limits: %v", err)
	}
	// reading sessions boots the project's VM (the runner lists them from disk) and needs no model
	if res, err := e.op("sessions", map[string]any{"project": "my-project"}); err != nil || res["sessions"] == nil {
		t.Errorf("sessions: %v %v", res, err)
	}
	if res, err := e.op("role.list", nil); err != nil || res == nil {
		t.Errorf("role.list: %v %v", res, err)
	}
	if _, err := e.op("ssh.list", nil); err != nil {
		t.Errorf("ssh.list: %v", err)
	}
	// what the claude-code plugin still reads: none of these is the retired refusal
	for op, args := range map[string]map[string]any{
		"messages": {"project": "my-project", "id": "s-1"}, "rename": {"project": "my-project", "id": "s-1", "name": "n"},
		"interrupt": {"project": "my-project", "id": "s-1"}, "close": {"project": "my-project", "id": "s-1"},
	} {
		if _, err := e.op(op, args); err != nil && err.Error() == retiredWording {
			t.Errorf("%s was refused as retired", op)
		}
	}
	if res, err := e.op("next", map[string]any{"wait_seconds": 1}); err != nil || res["items"] == nil {
		t.Errorf("next: %v %v", res, err)
	}
	if _, err := e.op("decide", map[string]any{"ask_id": "none", "behavior": "deny"}); err != nil && err.Error() == retiredWording {
		t.Error("decide was refused as retired")
	}
}

// An item an earlier Home queued is not delivered: its sender hears that it failed, with the same plain message.
func TestRunnerPathDoesNotDeliverWhatAnEarlierHomeQueued(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.AllowRunnerSessions = false })
	body, _ := json.Marshal(queueDisk{Version: 1, SavedAt: time.Now(), Items: []QueueItem{
		{ID: "aq_old", Op: "start", Project: "my-project", Text: "left over", At: time.Now()},
		{ID: "aq_old2", Op: "send", Project: "my-project", Session: "s-1", Text: "left over", At: time.Now()},
	}})
	if err := os.WriteFile(filepath.Join(e.dir, QueueFile), body, 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(func() Options { o := e.opt; o.Now = nil; return o }())
	defer m.Close()
	m.Resume()
	var failed []Item
	deadline := time.Now().Add(10 * time.Second)
	for len(failed) < 2 && time.Now().Before(deadline) {
		items, _ := m.Next(e.ctx, 300*time.Millisecond)
		for _, it := range items {
			if it.Type == "queue" && it.Action == "failed" {
				failed = append(failed, it)
			}
		}
	}
	if len(failed) != 2 {
		t.Fatalf("queue items that failed: %+v", failed)
	}
	for _, it := range failed {
		if it.Error != retiredWording {
			t.Errorf("failure text: %q", it.Error)
		}
	}
	if m.q.len() != 0 {
		t.Error("the old items stay queued")
	}
	if e.fake.State() == StateRunning {
		t.Error("a VM was started to deliver them")
	}
}
