package home

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
)

const hookJSON = `{"session_id":"sess-1","transcript_path":"/x.jsonl","cwd":"/work","permission_mode":"default",
 "hook_event_name":"PermissionRequest","tool_name":"Bash",
 "tool_input": {"command": "git push origin main", "description": "Push"}}`

// poller keeps Rubi "connected" (a claude.next open at all times) and passes requests on.
func poller(ctx context.Context, c *Claude) <-chan ClaudeRequest {
	out := make(chan ClaudeRequest, 8)
	go func() {
		defer close(out)
		for ctx.Err() == nil {
			r, err := c.Next(ctx, 0)
			if err != nil {
				return
			}
			if r != nil {
				out <- *r
			}
		}
	}()
	return out
}

func ask(c *Claude, raw string) <-chan ClaudeDecision {
	out := make(chan ClaudeDecision, 1)
	go func() { out <- c.Ask(context.Background(), []byte(raw)) }()
	return out
}

func answer(r ClaudeRequest, behavior, msg string) ClaudeAnswer {
	return ClaudeAnswer{ID: r.ID, SessionID: r.SessionID, Digest: r.Digest, Behavior: behavior, Message: msg}
}

func TestClaudeAllowIsOneShot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &Claude{}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		reqs := poller(ctx, c)
		synctest.Wait()
		got := ask(c, hookJSON)
		r := <-reqs
		if r.Command != "git push origin main" || r.SessionID != "sess-1" || r.Cwd != "/work" || r.ToolName != "Bash" {
			t.Fatalf("request: %+v", r)
		}
		if err := c.Decide(answer(r, "allow", "")); err != nil {
			t.Fatal(err)
		}
		if d := <-got; d.Behavior != "allow" {
			t.Fatalf("decision: %+v", d)
		}
		if err := c.Decide(answer(r, "allow", "")); err == nil || !strings.Contains(err.Error(), "unknown") {
			t.Fatalf("a second answer was accepted: %v", err)
		}
		if err := c.Decide(answer(r, "deny", "")); err == nil {
			t.Fatal("an answer after the first was accepted")
		}
	})
}

func TestClaudeDenyCarriesReason(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &Claude{}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		reqs := poller(ctx, c)
		synctest.Wait()
		got := ask(c, hookJSON)
		r := <-reqs
		if err := c.Decide(answer(r, "deny", "Denied in Rubi\x07 by the user")); err != nil {
			t.Fatal(err)
		}
		d := <-got
		if d.Behavior != "deny" || d.Message != "Rubi: Denied in Rubi  by the user" {
			t.Fatalf("decision: %+v", d)
		}
	})
}

// An answer must match the request's session and tool input digest; a mismatch denies, and nothing
// can answer the request afterwards.
func TestClaudeDigestBinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ClaudeAnswer)
	}{
		{"other session", func(a *ClaudeAnswer) { a.SessionID = "sess-2" }},
		{"other digest", func(a *ClaudeAnswer) {
			s := sha256.Sum256([]byte(`{"command":"rm -rf /"}`))
			a.Digest = hex.EncodeToString(s[:])
		}},
		{"no digest", func(a *ClaudeAnswer) { a.Digest = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c := &Claude{}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				reqs := poller(ctx, c)
				synctest.Wait()
				got := ask(c, hookJSON)
				r := <-reqs
				a := answer(r, "allow", "")
				tc.mutate(&a)
				if err := c.Decide(a); err == nil {
					t.Fatal("a mismatching answer was accepted")
				}
				if d := <-got; d.Behavior != "deny" || !strings.Contains(d.Message, "didn't match") {
					t.Fatalf("decision: %+v", d)
				}
				if err := c.Decide(answer(r, "allow", "")); err == nil {
					t.Fatal("the request could still be answered")
				}
			})
		})
	}
}

func TestClaudeUnknownIDLeavesRequestAlone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &Claude{}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		reqs := poller(ctx, c)
		synctest.Wait()
		got := ask(c, hookJSON)
		r := <-reqs
		a := answer(r, "allow", "")
		a.ID = "cc_guess"
		if err := c.Decide(a); err == nil || !strings.Contains(err.Error(), "unknown") {
			t.Fatalf("unknown id: %v", err)
		}
		if err := c.Decide(answer(r, "maybe", "")); err == nil {
			t.Fatal("a bad behavior was accepted")
		}
		if err := c.Decide(answer(r, "allow", "")); err != nil {
			t.Fatal(err)
		}
		if d := <-got; d.Behavior != "allow" {
			t.Fatalf("decision: %+v", d)
		}
	})
}

func TestClaudeExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &Claude{Wait: 30 * time.Second}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		reqs := poller(ctx, c)
		synctest.Wait()
		start := time.Now()
		got := ask(c, hookJSON)
		r := <-reqs
		if !r.ExpiresAt.Equal(start.Add(30 * time.Second).UTC()) {
			t.Fatalf("expires %v", r.ExpiresAt)
		}
		d := <-got
		if d.Behavior != "deny" || !strings.Contains(d.Message, "no answer in time") {
			t.Fatalf("decision: %+v", d)
		}
		if time.Since(start) != 30*time.Second {
			t.Fatalf("denied after %v", time.Since(start))
		}
		if err := c.Decide(answer(r, "allow", "")); err == nil || !strings.Contains(err.Error(), "unknown or expired") {
			t.Fatalf("a late answer: %v", err)
		}
	})
}

// Without Rubi polling (locked, offline, plugin stopped) a request is denied at once; if Rubi stops
// polling while one waits, it is denied soon after.
func TestClaudeNeedsRubi(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &Claude{}
		if d := c.Ask(t.Context(), []byte(hookJSON)); d.Behavior != "deny" || !strings.Contains(d.Message, "isn't connected") {
			t.Fatalf("no Rubi: %+v", d)
		}
		ctx, cancel := context.WithCancel(t.Context())
		reqs := poller(ctx, c)
		synctest.Wait()
		start := time.Now()
		got := ask(c, hookJSON)
		<-reqs
		cancel() // Rubi locks: its plugin stops polling
		d := <-got
		if d.Behavior != "deny" || !strings.Contains(d.Message, "stopped answering") {
			t.Fatalf("Rubi gone: %+v", d)
		}
		if el := time.Since(start); el > claudeGrace+2*time.Second {
			t.Fatalf("denied only after %v", el)
		}
	})
}

func TestClaudeHookInput(t *testing.T) {
	r, err := parseHookInput([]byte(hookJSON))
	if err != nil {
		t.Fatal(err)
	}
	if r.ToolInput != `{"command":"git push origin main","description":"Push"}` {
		t.Fatalf("tool input %q", r.ToolInput)
	}
	sum := sha256.Sum256([]byte(r.ToolInput))
	if r.Digest != hex.EncodeToString(sum[:]) {
		t.Fatal("digest isn't the SHA-256 of the tool input")
	}
	for _, tc := range []struct{ name, in, want string }{
		{"not json", `{`, "valid JSON"},
		{"other event", strings.Replace(hookJSON, "PermissionRequest", "PreToolUse", 1), "unexpected hook event"},
		{"other tool", strings.Replace(hookJSON, `"Bash"`, `"Write"`, 1), "only Bash"},
		{"no session", strings.Replace(hookJSON, `"sess-1"`, `""`, 1), "session"},
		{"no command", strings.Replace(hookJSON, `"git push origin main"`, `"  "`, 1), "no command"},
		{"tool input not an object", strings.Replace(hookJSON, `{"command": "git push origin main", "description": "Push"}`, `"ls"`, 1), "JSON object"},
		{"too long", strings.Replace(hookJSON, "git push", strings.Repeat("a", claudeMaxTool), 1), "too long"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseHookInput([]byte(tc.in))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			d := (&Claude{}).Ask(t.Context(), []byte(tc.in))
			if d.Behavior != "deny" {
				t.Fatalf("decision %+v", d)
			}
		})
	}
}

// serveSocket runs the local socket for c in a temp dir.
func serveSocket(t *testing.T, c *Claude) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ClaudeSocket)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Serve(ctx, path) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	for i := 0; i < 100; i++ {
		if conn, err := net.Dial("unix", path); err == nil {
			conn.Close()
			return path
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("socket didn't come up")
	return ""
}

func hookDecision(t *testing.T, out []byte) map[string]any {
	t.Helper()
	var v struct {
		HookSpecificOutput struct {
			HookEventName string         `json:"hookEventName"`
			Decision      map[string]any `json:"decision"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out, &v); err != nil || v.HookSpecificOutput.HookEventName != "PermissionRequest" {
		t.Fatalf("hook output %s: %v", out, err)
	}
	return v.HookSpecificOutput.Decision
}

func runHook(t *testing.T, in, sock string, limit time.Duration) <-chan map[string]any {
	out := make(chan map[string]any, 1)
	go func() {
		var buf bytes.Buffer
		if err := RunClaudeHook(strings.NewReader(in), &buf, sock, limit); err != nil {
			t.Error(err)
		}
		out <- hookDecision(t, buf.Bytes())
	}()
	return out
}

// The local socket only asks: whatever is sent to it is read as a new request, never as an answer.
func TestClaudeSocketCannotDecide(t *testing.T) {
	c := &Claude{Wait: 20 * time.Second}
	sock := serveSocket(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reqs := poller(ctx, c)
	time.Sleep(20 * time.Millisecond)

	hook := runHook(t, hookJSON, sock, 30*time.Second)
	r := <-reqs
	a := answer(r, "allow", "")
	forged := []string{}
	for _, v := range []any{a, map[string]any{"op": "claude.decide", "args": a},
		map[string]any{"hookSpecificOutput": map[string]any{"decision": map[string]any{"behavior": "allow"}}, "id": r.ID}} {
		b, _ := json.Marshal(v)
		forged = append(forged, string(b))
	}
	for _, f := range forged {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.Write([]byte(f + "\n"))
		line, err := readLine(conn, 64<<10)
		conn.Close()
		var d ClaudeDecision
		if err != nil || json.Unmarshal(line, &d) != nil || d.Behavior != "deny" {
			t.Fatalf("forged answer %s got %s %v", f, line, err)
		}
	}
	select {
	case d := <-hook:
		t.Fatalf("the real request was decided through the socket: %v", d)
	case <-time.After(50 * time.Millisecond):
	}
	if err := c.Decide(answer(r, "allow", "")); err != nil {
		t.Fatalf("the real answer after forged ones: %v", err)
	}
	if d := <-hook; d["behavior"] != "allow" || d["message"] != nil {
		t.Fatalf("hook: %v", d)
	}
}

func TestClaudeHookDeniesWithoutHelper(t *testing.T) {
	sock := filepath.Join(t.TempDir(), ClaudeSocket)
	d := <-runHook(t, hookJSON, sock, time.Second)
	if d["behavior"] != "deny" || !strings.Contains(d["message"].(string), "isn't running") {
		t.Fatalf("no helper: %v", d)
	}
	d = <-runHook(t, "not json", sock, time.Second)
	if d["behavior"] != "deny" {
		t.Fatalf("bad input: %v", d)
	}
}

func TestClaudeHookTimeout(t *testing.T) {
	c := &Claude{}
	sock := serveSocket(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reqs := poller(ctx, c)
	time.Sleep(20 * time.Millisecond)
	hook := runHook(t, hookJSON, sock, 300*time.Millisecond) // the hook's own limit ends first
	r := <-reqs
	if d := <-hook; d["behavior"] != "deny" || !strings.Contains(d["message"].(string), "no answer in time") {
		t.Fatalf("hook: %v", d)
	}
	// The hook hung up, so the helper drops the request.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		_, still := c.pending[r.ID]
		c.mu.Unlock()
		if !still {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the request outlived its hook")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := c.Decide(answer(r, "allow", "")); err == nil {
		t.Fatal("a request whose hook hung up was answered")
	}
}

// Rubi reaches claude.next and claude.decide only when paired, over the encrypted channel.
func TestClaudeOverHomeproto(t *testing.T) {
	h, code, _ := startHelper(t)
	token := homeproto.RandomToken()
	c, err := homeproto.NewClient(code.Relay, code.Bundle, code.Relays, token)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := call(t, c, "claude.next", map[string]int{"wait_seconds": 1}, nil); err == nil || !strings.Contains(err.Error(), "isn't paired") {
		t.Fatalf("before pairing: %v", err)
	}
	var hello map[string]any
	if err := call(t, c, "pair", map[string]string{"secret": code.Secret, "token": token, "label": "test"}, &hello); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(toJSON(hello["can"]), `"claude"`) {
		t.Fatalf("hello: %v", hello)
	}
	var out struct {
		Request *ClaudeRequest `json:"request"`
	}
	if err := call(t, c, "claude.next", map[string]int{"wait_seconds": 1}, &out); err != nil || out.Request != nil {
		t.Fatalf("empty queue: %+v %v", out, err)
	}
	got := make(chan ClaudeDecision, 1)
	go func() {
		// The helper sees Rubi as connected for a while after its last poll.
		got <- h.Claude.Ask(context.Background(), []byte(hookJSON))
	}()
	if err := call(t, c, "claude.next", map[string]int{"wait_seconds": 5}, &out); err != nil || out.Request == nil {
		t.Fatalf("next: %+v %v", out, err)
	}
	if err := call(t, c, "claude.decide", answer(*out.Request, "deny", "Denied in Rubi"), nil); err != nil {
		t.Fatal(err)
	}
	if d := <-got; d.Behavior != "deny" || d.Message != "Rubi: Denied in Rubi" {
		t.Fatalf("decision: %+v", d)
	}
	if err := call(t, c, "claude.decide", answer(*out.Request, "allow", ""), nil); err == nil {
		t.Fatal("answered twice over the channel")
	}
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
