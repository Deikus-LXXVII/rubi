package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// AskTool is the one approval path of the runner's perm.ask and of a T3 session's hook gate: the item the plugin
// gets carries the role, the session and the mode, secrets are redacted, and only an allow from the plugin allows.
func TestAskToolQueuesTheItemAndTakesTheDecision(t *testing.T) {
	e := newEnv(t)
	pl := startPoller(t, e.m)
	eventually(t, "the plugin polling", e.m.connected)
	in := json.RawMessage(`{"command":"echo sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"}`)
	done := make(chan AskResult, 1)
	go func() {
		done <- e.m.AskTool(context.Background(), "my-project", "role-1", AskParams{SessionID: "s-1", Tool: "Bash", ToolUseID: "tu-1", Via: "hook",
			Input: in, Cwd: "/w", Mode: "default", AgentID: "a-1", AgentType: "x"})
	}()
	it := pl.wait(t, "perm.ask")
	if it.Project != "my-project" || it.Role != "role-1" || it.SessionID != "s-1" || it.Tool != "Bash" || it.ToolUseID != "tu-1" ||
		it.Via != "hook" || it.Cwd != "/w" || it.Mode != "default" || it.AgentID != "a-1" || it.AgentType != "x" {
		t.Fatalf("item: %+v", it)
	}
	if strings.Contains(string(it.Input), "sk-ant-api03") {
		t.Fatalf("a credential reached the plugin: %s", it.Input)
	}
	if err := e.m.Decide(it.AskID, "allow", "", nil); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Behavior != "allow" {
		t.Fatalf("result: %+v", r)
	}
}

func TestAskToolDeniesAnOversizedInputWithoutAsking(t *testing.T) {
	e := newEnv(t)
	big := json.RawMessage(`{"x":"` + strings.Repeat("a", maxInput) + `"}`)
	if r := e.m.AskTool(e.ctx, "my-project", "my-project", AskParams{Tool: "Write", Input: big}); r.Behavior != "deny" {
		t.Fatalf("result: %+v", r)
	}
	if len(e.m.out) != 0 {
		t.Fatalf("queued for the plugin: %v", e.m.out)
	}
}

func TestAskToolDeniesWhenNobodyIsConnected(t *testing.T) {
	e := newEnv(t)
	if r := e.m.AskTool(e.ctx, "my-project", "my-project", AskParams{Tool: "Bash", Input: json.RawMessage(`{}`)}); r.Behavior != "deny" || r.Message != DenyLocked {
		t.Fatalf("result: %+v", r)
	}
}
