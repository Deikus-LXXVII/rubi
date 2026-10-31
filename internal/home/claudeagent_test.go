package home

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
)

// agentHelper is a paired helper whose agent runs on the fake VM, with one project.
func agentHelper(t *testing.T) (*Helper, *agent.Fake, string) {
	t.Helper()
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := h.Update(func(c *Config) {
		c.AgentProjects = []string{proj}
		c.AgentGitHosts = []string{"git.example.com"}
		c.Paired = []Pairing{{TokenHash: tokenHash("tok")}}
	}); err != nil {
		t.Fatal(err)
	}
	fake := &agent.Fake{}
	h.Agent.Close()
	h.Agent = agent.New(agent.Options{Dir: h.Dir, Projects: func() []string { return h.Fresh().AgentProjects },
		GitHosts: func() []string { return h.Fresh().AgentGitHosts },
		NewVM:    func(agent.Project) agent.VM { return fake }, Wait: 5 * time.Second})
	t.Cleanup(h.Agent.Close)
	return h, fake, proj
}

func handle(t *testing.T, h *Helper, tok, op string, args any) (map[string]any, error) {
	t.Helper()
	b, _ := json.Marshal(args)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	v, err := h.Handle(ctx, homeproto.Envelope{Op: op, Token: tok, Args: b})
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

func TestClaudeAgentOpsThroughHandle(t *testing.T) {
	h, fake, _ := agentHelper(t)
	// Every op needs the pairing.
	for _, op := range []string{"projects", "status", "next", "start"} {
		if _, err := handle(t, h, "wrong", "claude-agent."+op, map[string]any{}); err == nil || !strings.Contains(err.Error(), "paired") {
			t.Fatalf("%s unpaired: %v", op, err)
		}
	}
	hello, _ := handle(t, h, "tok", "hello", nil)
	found := false
	for _, c := range hello["can"].([]any) {
		found = found || c == "claude-agent"
	}
	if !found {
		t.Fatalf("hello: %v", hello)
	}
	res, err := handle(t, h, "tok", "claude-agent.projects", nil)
	if err != nil || len(res["projects"].([]any)) != 1 || res["projects"].([]any)[0].(map[string]any)["id"] != "my-project" {
		t.Fatalf("projects: %v %v", res, err)
	}
	res, err = handle(t, h, "tok", "claude-agent.start", map[string]any{"project": "my-project", "prompt": "hello", "mode": "default", "from": "alice"})
	if err != nil || res["id"] != "sess-1" {
		t.Fatalf("start: %v %v", res, err)
	}
	res, err = handle(t, h, "tok", "claude-agent.start", map[string]any{"project": "my-project", "prompt": "again"})
	if err != nil || res["queued"] != true || res["position"].(float64) != 1 {
		t.Fatalf("busy start: %v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(h.Dir, agent.QueueFile)); err != nil {
		t.Fatal("no queue file:", err)
	}
	st, err := handle(t, h, "tok", "claude-agent.status", nil)
	if err != nil || st["slot"].(map[string]any)["busy"] != true || len(st["queue"].([]any)) != 1 {
		t.Fatalf("status: %v %v", st, err)
	}
	// A tool call: next hands it out, decide answers it.
	done := make(chan agent.AskResult, 1)
	if _, err := handle(t, h, "tok", "claude-agent.next", map[string]any{"wait_seconds": 1}); err != nil {
		t.Fatal(err)
	}
	go func() {
		r, _ := fake.Runner().Ask(context.Background(), agent.AskParams{SessionID: "sess-1", Tool: "Bash", Input: json.RawMessage(`{"command":"ls"}`)})
		done <- r
	}()
	var ask map[string]any
	for ask == nil {
		next, err := handle(t, h, "tok", "claude-agent.next", map[string]any{"wait_seconds": 1})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range next["items"].([]any) {
			if m := it.(map[string]any); m["type"] == "perm.ask" {
				ask = m
			}
		}
	}
	if _, err := handle(t, h, "tok", "claude-agent.decide", map[string]any{"ask_id": ask["ask_id"], "behavior": "allow"}); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Behavior != "allow" {
		t.Fatalf("decision: %+v", r)
	}
	for op, args := range map[string]map[string]any{"sessions": {}, "messages": {"id": "s"}, "rename": {"id": "s", "name": "x"},
		"fork": {"id": "s"}, "interrupt": {"id": "s"}, "mode": {"id": "s", "mode": "plan"}, "close": {"id": "s"}} {
		args["project"] = "my-project"
		if _, err := handle(t, h, "tok", "claude-agent."+op, args); err != nil {
			t.Errorf("%s: %v", op, err)
		}
	}
	if _, err := handle(t, h, "tok", "claude-agent.bogus", nil); err == nil {
		t.Fatal("unknown op served")
	}
}

func TestClaudeAgentConfigAndDefaults(t *testing.T) {
	h, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer h.Agent.Close()
	if err := h.Update(func(c *Config) { c.Paired = []Pairing{{TokenHash: tokenHash("tok")}} }); err != nil {
		t.Fatal(err)
	}
	res, err := handle(t, h, "tok", "claude-agent.projects", nil)
	if err != nil || len(res["projects"].([]any)) != 0 {
		t.Fatalf("no projects configured: %v %v", res, err)
	}
	if _, err := handle(t, h, "tok", "claude-agent.start", map[string]any{"project": "x", "prompt": "p"}); err == nil {
		t.Fatal("start without a project")
	}
	// A project that holds the helper's directory is never offered.
	if err := h.Update(func(c *Config) { c.AgentProjects = []string{filepath.Dir(h.Dir), h.Dir} }); err != nil {
		t.Fatal(err)
	}
	res, _ = handle(t, h, "tok", "claude-agent.projects", nil)
	if len(res["projects"].([]any)) != 0 {
		t.Fatalf("projects over the helper dir: %v", res)
	}
}

func TestUsageModeOpSavesTheConfig(t *testing.T) {
	h, _, _ := agentHelper(t)
	h.Agent.Close()
	h.Agent = h.newAgent()
	t.Cleanup(h.Agent.Close)
	if _, err := handle(t, h, "wrong", "claude-agent.usage_mode", map[string]any{"all": true, "mode": "light"}); err == nil {
		t.Fatal("an unpaired caller switched the mode")
	}
	// A Bot is met on its first start and gets the default; then the user changes it.
	if _, err := handle(t, h, "tok", "claude-agent.usage_mode", map[string]any{"default": true, "mode": "light"}); err != nil {
		t.Fatal(err)
	}
	if _, err := handle(t, h, "tok", "claude-agent.start", map[string]any{"project": "my-project", "prompt": "x", "bot": "bot-a"}); err != nil {
		t.Fatal(err)
	}
	if got := h.Fresh().AgentBotModes["bot-a"]; got != "light" {
		t.Fatalf("a new Bot's mode %q", got)
	}
	st, err := handle(t, h, "tok", "claude-agent.usage_mode", map[string]any{"bot": "bot-a", "mode": "maximum"})
	if err != nil || h.Fresh().AgentBotModes["bot-a"] != "maximum" || st["usage"].(map[string]any)["default"] != "light" {
		t.Fatalf("switch: %v %v", st, err)
	}
	if _, err := handle(t, h, "tok", "claude-agent.usage_mode", map[string]any{"all": true, "mode": "medium"}); err != nil {
		t.Fatal(err)
	}
	if c := h.Fresh(); c.AgentDefaultMode != "medium" || c.AgentBotModes["bot-a"] != "medium" {
		t.Fatalf("saved: %+v", c)
	}
}

func TestLimitsOpSavesTheConfig(t *testing.T) {
	h, _, _ := agentHelper(t)
	h.Agent.Close()
	h.Agent = h.newAgent()
	t.Cleanup(h.Agent.Close)
	two, zero := 2, 0
	if _, err := handle(t, h, "wrong", "claude-agent.limits", map[string]any{"per_profile": two}); err == nil {
		t.Fatal("an unpaired caller changed the limits")
	}
	if _, err := handle(t, h, "tok", "claude-agent.limits", map[string]any{}); err == nil {
		t.Fatal("an empty change")
	}
	for _, bad := range []map[string]any{{"per_profile": 0}, {"per_profile": 1000}, {"per_project": -1}, {"total": -1}} {
		if _, err := handle(t, h, "tok", "claude-agent.limits", bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	st, err := handle(t, h, "tok", "claude-agent.limits", map[string]any{"per_profile": two, "per_project": 3, "total": zero})
	if err != nil {
		t.Fatal(err)
	}
	lim := st["limits"].(map[string]any)
	c := h.Fresh()
	if c.AgentMaxSessionsPerProfile != 2 || c.AgentMaxSessionsPerProject != 3 || c.AgentMaxSessions == nil || *c.AgentMaxSessions != 0 ||
		lim["per_profile"] != float64(2) || lim["per_project"] != float64(3) || lim["total"] != float64(0) {
		t.Fatalf("saved %+v, status %v", c, lim)
	}
	// The cap can be taken off again (0), and a value not given stays.
	if _, err := handle(t, h, "tok", "claude-agent.limits", map[string]any{"per_project": 0}); err != nil {
		t.Fatal(err)
	}
	if c := h.Fresh(); c.AgentMaxSessionsPerProject != 0 || c.AgentMaxSessionsPerProfile != 2 {
		t.Fatalf("saved %+v", c)
	}
}
