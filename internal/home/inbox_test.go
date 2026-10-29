package home

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func hook(event, session string, extra map[string]any) []byte {
	m := map[string]any{"hook_event_name": event, "session_id": session, "cwd": "/work"}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return b
}

func liveClaude(t *testing.T, sessions ...string) *Claude {
	c := &Claude{Transcripts: t.TempDir()}
	for _, s := range sessions {
		c.Hook(hook("SessionStart", s, nil))
	}
	return c
}

// outs drains what sessions said, without waiting.
func outs(c *Claude) []ClaudeOut {
	var got []ClaudeOut
	for {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c.mu.Lock()
		n := len(c.outbox)
		c.mu.Unlock()
		if n == 0 {
			return got
		}
		_, o, _ := c.next(ctx, 0, false, true)
		got = append(got, *o)
	}
}

func TestSendNeedsAKnownLiveSession(t *testing.T) {
	c := liveClaude(t, "s1")
	if _, _, err := c.Send("nope", "Vega", "hi"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown session: %v", err)
	}
	if _, _, err := c.Send("s1", "Vega", strings.Repeat("x", claudeMaxText+1)); err == nil {
		t.Fatal("a long message was queued")
	}
	if _, _, err := c.Send("s1", "", "hi"); err == nil {
		t.Fatal("no sender")
	}
	c.Hook(hook("SessionEnd", "s1", nil))
	if _, _, err := c.Send("s1", "Vega", "hi"); err == nil || !strings.Contains(err.Error(), "ended") {
		t.Fatalf("ended session: %v", err)
	}
}

func TestPromptCarriesMessagesAndTheAnswerGoesBack(t *testing.T) {
	c := liveClaude(t, "s1", "s2")
	if _, n, err := c.Send("s1", "Vega", "Which tests cover the parser?"); err != nil || n != 1 {
		t.Fatalf("send: %d %v", n, err)
	}
	if r := c.Hook(hook("UserPromptSubmit", "s2", nil)); r.Context != "" {
		t.Fatalf("another session got the message: %+v", r)
	}
	r := c.Hook(hook("UserPromptSubmit", "s1", nil))
	if !strings.Contains(r.Context, "Which tests cover the parser?") || !strings.Contains(r.Context, "from Vega") {
		t.Fatalf("context: %+v", r)
	}
	out := EventHookOutput("UserPromptSubmit", r)
	if !strings.Contains(string(out), `"additionalContext"`) || !strings.Contains(string(out), `"hookEventName":"UserPromptSubmit"`) {
		t.Fatalf("hook output: %s", out)
	}
	if s := c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": "parser_test.go and fuzz_test.go"})); s.Block {
		t.Fatalf("stop was blocked with nothing waiting: %+v", s)
	}
	got := outs(c)
	if len(got) != 1 || got[0].Kind != "reply" || got[0].To != "Vega" || got[0].SessionID != "s1" || got[0].Text != "parser_test.go and fuzz_test.go" {
		t.Fatalf("outs: %+v", got)
	}
	// The next turn carried nothing: its answer stays here.
	c.Hook(hook("UserPromptSubmit", "s1", nil))
	c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": "private"}))
	if got := outs(c); len(got) != 0 {
		t.Fatalf("an answer nobody asked for went out: %+v", got)
	}
}

func TestStopDeliversAtMostFiveInARow(t *testing.T) {
	c := liveClaude(t, "s1")
	blocks := 0
	for i := 0; i < 8; i++ {
		if _, _, err := c.Send("s1", "Vega", "next"); err != nil {
			t.Fatal(err)
		}
		r := c.Hook(hook("Stop", "s1", map[string]any{"stop_hook_active": i > 0, "last_assistant_message": "done"}))
		if r.Block {
			blocks++
			if !strings.Contains(string(EventHookOutput("Stop", r)), `"decision":"block"`) {
				t.Fatal("stop output")
			}
		} else {
			break
		}
	}
	if blocks != claudeMaxChain {
		t.Fatalf("blocks in a row: %d", blocks)
	}
	if s := c.Sessions("", false); s[0].Waiting != 1 {
		t.Fatalf("the message after the cap should wait: %+v", s[0])
	}
	// A fresh stop (not a continuation) delivers again.
	if r := c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": "done"})); !r.Block {
		t.Fatal("a fresh stop didn't deliver")
	}
	if got := outs(c); len(got) != claudeMaxChain { // each delivered message got its answer
		t.Fatalf("replies: %d", len(got))
	}
}

func TestAnswerFromTranscriptOnlyUnderProjects(t *testing.T) {
	c := liveClaude(t, "s1")
	tr := filepath.Join(c.Transcripts, "-work", "s1.jsonl")
	writeFile(t, tr, `{"type":"user","message":{"content":"q"}}`+"\n"+
		`{"type":"assistant","message":{"content":[{"type":"text","text":"first"}]}}`+"\n"+
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash"}]}}`+"\n"+
		`{"type":"assistant","message":{"content":[{"type":"text","text":"the answer"}]}}`+"\n")
	outside := filepath.Join(t.TempDir(), "secret.jsonl")
	writeFile(t, outside, `{"type":"assistant","message":{"content":[{"type":"text","text":"secret"}]}}`+"\n")
	for _, tc := range []struct{ path, want string }{{tr, "the answer"}, {outside, ""}} {
		_, _, _ = c.Send("s1", "Vega", "q")
		c.Hook(hook("UserPromptSubmit", "s1", nil))
		c.Hook(hook("Stop", "s1", map[string]any{"transcript_path": tc.path}))
		got := outs(c)
		if tc.want == "" && len(got) != 0 || tc.want != "" && (len(got) != 1 || got[0].Text != tc.want) {
			t.Fatalf("%s: %+v", tc.path, got)
		}
	}
}

func TestAsksFromAnswers(t *testing.T) {
	c := liveClaude(t, "s1")
	answer := "Working on it.\n```rubi-ask kind=audit\nIs our use of crypto/subtle in claude.go right?\n```\n" +
		"```rubi-ask\n" + strings.Repeat("y", claudeMaxAsk+1) + "\n```\n```rubi-ask kind=bogus\nFind the latest Go release.\n```"
	c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": answer}))
	got := outs(c)
	if len(got) != 2 || got[0].Kind != "ask" || got[0].AskKind != "audit" || !strings.Contains(got[0].Text, "crypto/subtle") ||
		got[1].AskKind != "research" || got[1].Text != "Find the latest Go release." {
		t.Fatalf("asks: %+v", got)
	}
}

func TestEventHooksOverTheSocket(t *testing.T) {
	c := liveClaude(t, "s1")
	sock := serveSocket(t, c)
	_, _, _ = c.Send("s1", "Vega", "hello there")
	var out strings.Builder
	if err := RunClaudeHook(strings.NewReader(string(hook("UserPromptSubmit", "s1", nil))), &out, sock, 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "hello there") {
		t.Fatalf("output: %q", out.String())
	}
	out.Reset()
	if err := RunClaudeHook(strings.NewReader(string(hook("Stop", "s1", nil))), &out, sock, 0); err != nil || out.Len() != 0 {
		t.Fatalf("stop with nothing waiting printed %q (%v)", out.String(), err)
	}
	out.Reset()
	if err := RunClaudeHook(strings.NewReader(string(hook("Stop", "s1", nil))), &out, filepath.Join(t.TempDir(), "none"), 0); err != nil || out.Len() != 0 {
		t.Fatalf("no helper: %q (%v)", out.String(), err)
	}
}

// Approvals and sessions are separate capabilities: claude.next hands out only permission requests,
// claude-sessions.next only what sessions say, and polling it doesn't count as Rubi being there to approve.
func TestSessionsPollIsSeparate(t *testing.T) {
	c := liveClaude(t, "s1")
	c.mu.Lock()
	c.outbox = append(c.outbox, ClaudeOut{ID: "o1", Kind: "reply", SessionID: "s1"})
	c.mu.Unlock()
	ctx := context.Background()
	got, err := c.handle(ctx, "claude.next", json.RawMessage(`{"wait_seconds":1}`))
	if err != nil || strings.Contains(toJSON(got), "o1") {
		t.Fatalf("claude.next handed out a session message: %v %v", toJSON(got), err)
	}
	got, err = c.handle(ctx, "claude-sessions.next", json.RawMessage(`{"wait_seconds":1}`))
	if err != nil || !strings.Contains(toJSON(got), `"o1"`) {
		t.Fatalf("claude-sessions.next: %v %v", toJSON(got), err)
	}
	c2 := liveClaude(t, "s1")
	if _, err := c2.handle(ctx, "claude-sessions.next", json.RawMessage(`{"wait_seconds":1}`)); err != nil {
		t.Fatal(err)
	}
	c2.mu.Lock()
	present := c2.connectedLocked(time.Now())
	c2.mu.Unlock()
	if present {
		t.Fatal("polling for session messages made Rubi count as present for approvals")
	}
	if _, err := c.handle(ctx, "claude.sessions", nil); err == nil {
		t.Fatal("the old op name still works")
	}
}
