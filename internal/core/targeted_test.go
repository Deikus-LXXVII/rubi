package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// targetCore is a core with two Bots, Main (the administrator) and Mail, and a Bot entry without a
// webhook (Half), all subscribed to the demo-mail plugin.
func targetCore(t *testing.T) *Core {
	c, hook, _ := botCore(t)
	_ = c.Vault.Update(func(d *vault.Data) error {
		d.Agents = append(d.Agents,
			&vault.Agent{Name: "Main", URL: hook, Key: "k1", Default: true, Subscriptions: []string{"demo-mail"}},
			&vault.Agent{Name: "Mail", URL: hook + "x", Key: "k2", Subscriptions: []string{"demo-mail"}},
			&vault.Agent{Name: "Half", Subscriptions: []string{"demo-mail"}})
		return nil
	})
	return c
}

func names(as []vault.Agent) string {
	var out []string
	for _, a := range as {
		out = append(out, a.Name)
	}
	return strings.Join(out, ",")
}

// An addressed event goes to that Bot's webhook or to no one: never to the subscribers or the administrator.
func TestRecipientsOfAnAddressedEvent(t *testing.T) {
	c := targetCore(t)
	for target, want := range map[string]string{"mail": "Mail", "Mail": "Mail", "Ghost": "", "Half": "", "": "Main,Mail,Half"} {
		if got := names(c.recipients(target, "demo-mail")); got != want {
			t.Errorf("recipients(%q) = %q, want %q", target, got, want)
		}
	}
	if got := names(c.recipients("Ghost", "rubi")); got != "" {
		t.Errorf("Rubi's own event for an unknown Bot went to %q", got)
	}
	if n, ok := c.webhookAgent("MAIL"); !ok || n != "Mail" {
		t.Errorf("webhookAgent: %q %v", n, ok)
	}
	if _, ok := c.webhookAgent("Half"); ok {
		t.Error("a Bot without a webhook counts as connected")
	}
}

// The event list: an event addressed to Mail is Mail's alone, the administrator included.
func TestAddressedEventsAreTheTargetsAlone(t *testing.T) {
	c := targetCore(t)
	c.Events.EmitFor("Mail", false, "demo-mail", "ping", map[string]any{}, nil)
	c.Events.EmitFor("", false, "demo-mail", "pong", map[string]any{}, nil)
	count := func(agent string) (n int) {
		evs, err := c.EventsFor(agent, c.agentCodeFor(agent), false)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range evs {
			if e.Type == "ping" {
				n++
			}
		}
		return n
	}
	if count("Main") != 0 || count("Mail") != 1 {
		t.Fatalf("addressed event visible to Main %d, Mail %d", count("Main"), count("Mail"))
	}
}

func callerArgs(t *testing.T, c *Core, in map[string]any) (map[string]any, error) {
	b, _ := json.Marshal(in)
	out, err := c.verifyCaller("demo-mail", b)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	return m, nil
}

// A plugin learns the calling Bot's registered name only from a proof, never from what the caller says.
func TestVerifiedCaller(t *testing.T) {
	c := targetCore(t)
	code := c.agentCodeFor("Mail")
	if m, _ := callerArgs(t, c, map[string]any{"x": 1, "rubi_verified_agent": "Main"}); m["rubi_verified_agent"] != nil || m["x"] != float64(1) {
		t.Fatalf("a forged name passed: %v", m)
	}
	if m, _ := callerArgs(t, c, map[string]any{"rubi_agent": "Main"}); m["rubi_verified_agent"] != nil {
		t.Fatalf("a name without proof was verified: %v", m)
	}
	m, err := callerArgs(t, c, map[string]any{"rubi_agent": "mail", "rubi_agent_code": code, "rubi_verified_agent": "Main"})
	if err != nil || m["rubi_verified_agent"] != "Mail" || m["rubi_agent_code"] != nil || m["rubi_agent"] != "mail" {
		t.Fatalf("with its code: %v %v", m, err)
	}
	if _, err := callerArgs(t, c, map[string]any{"rubi_agent": "Main", "rubi_agent_code": code}); err == nil {
		t.Fatal("Mail's code verified Main")
	}
	if _, err := callerArgs(t, c, map[string]any{"rubi_agent": "Half", "rubi_agent_code": "x"}); err == nil || !strings.Contains(err.Error(), "no Bot named") {
		t.Fatalf("a Bot without a webhook: %v", err)
	}
	if _, err := callerArgs(t, c, map[string]any{"rubi_agent": "Mail", "rubi_agent_code": ""}); err == nil || !strings.Contains(err.Error(), "rubi_verify") {
		t.Fatalf("an empty code: %v", err)
	}
	if out, err := c.verifyCaller("demo-mail", json.RawMessage(`[1]`)); err != nil || string(out) != `[1]` {
		t.Fatalf("non-object arguments: %s %v", out, err)
	}
}

// "claude" (approvals) and "claude-sessions" (sessions and messages) don't reach each other's operations.
func TestClaudeCapabilitiesAreSeparate(t *testing.T) {
	c := targetCore(t)
	for _, tc := range []struct{ cap, op string }{{"claude", "claude-sessions.list"}, {"claude", "claude-sessions.next"},
		{"claude-sessions", "claude.decide"}, {"claude-sessions", "claude.next"},
		{"claude", "claude-start.create"}, {"claude-sessions", "claude-start.create"}, {"claude-start", "claude-sessions.send"},
		{"claude-start", "claude.decide"}, {"claude", "claude-agent.next"}, {"claude-sessions", "claude-agent.decide"},
		{"claude-agent", "claude.decide"}, {"claude-agent", "claude-sessions.send"}, {"claude-agent", "claude-start.create"},
		{"claude-agent", "agent-setup.run"}, {"agent-setup", "claude-agent.start"}, {"agent-setup", "claude-agent.decide"}} {
		if _, err := c.homeCall(context.Background(), "p", []string{tc.cap}, "", tc.op, nil); err == nil || !strings.Contains(err.Error(), "doesn't allow") {
			t.Errorf("%s reached %s: %v", tc.cap, tc.op, err)
		}
	}
	if _, ok := homeCapabilities["claude-sessions"]; !ok || strings.Contains(homeCapabilities["claude"], "sessions") ||
		strings.Contains(homeCapabilities["claude-sessions"], "start") || !strings.Contains(homeCapabilities["claude-start"], "can run any command") || homeCapabilities["claude-agent"] == "" ||
		!strings.Contains(homeCapabilities["agent-setup"], "administrator") {
		t.Errorf("install screen texts: %v", homeCapabilities)
	}
}
