package home

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeProcs is a fake process tree and socket peer: the helper asks it instead of the kernel. Pids have
// names (as `ps -o comm` shows them); every process added without one is called "proc".
type fakeProcs struct {
	mu     sync.Mutex
	parent map[int]int
	start  map[int]int64
	name   map[int]string
	peer   int
	noPeer bool
	next   int // next claude pid
}

func newFakeProcs() *fakeProcs {
	f := &fakeProcs{parent: map[int]int{}, start: map[int]int64{}, name: map[int]string{}, next: 100}
	f.add(10, 1, "Claude") // the desktop app that starts every claude
	f.add(20, 10, "zsh")   // a login shell shared by the claude processes below
	return f
}

func (f *fakeProcs) add(pid, ppid int, name ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parent[pid], f.start[pid], f.name[pid] = ppid, int64(pid)*1000, "proc"
	if len(name) > 0 {
		f.name[pid] = name[0]
	}
}

func (f *fakeProcs) remove(pid int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.parent, pid)
}

func (f *fakeProcs) setPeer(pid int) { f.mu.Lock(); f.peer = pid; f.mu.Unlock() }

func (f *fakeProcs) PeerPID(net.Conn) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noPeer {
		return 0, errors.New("no peer")
	}
	return f.peer, nil
}

func (f *fakeProcs) Info(pid int) (int, int64, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pp, ok := f.parent[pid]
	if !ok {
		return 0, 0, "", errors.New("no such process")
	}
	return pp, f.start[pid], f.name[pid], nil
}

// id is the peer record of a pid as the helper takes it right after Accept.
func (f *fakeProcs) id(pid int) peerID {
	pp, st, _, err := f.Info(pid)
	_ = pp
	if err != nil {
		return peerID{}
	}
	return peerID{pid, st, true}
}

// attest plays a hook line of session s from process pid.
func (f *fakeProcs) attest(c *Claude, s string, pid int) {
	c.attestHook(f.id(pid), s)
}

// claude starts a `claude` process under the shared shell, with a hook process and an MCP server (node)
// below it; the hook's line for session s is attested.
func (f *fakeProcs) claude(c *Claude, s string) (claudePID, mcpPID int) {
	f.mu.Lock()
	claudePID = f.next
	f.next += 10
	f.mu.Unlock()
	f.add(claudePID, 20, "claude")
	f.add(claudePID+1, claudePID, "rubi-home") // hook
	f.add(claudePID+2, claudePID, "node")      // MCP server
	f.attest(c, s, claudePID+1)
	return claudePID, claudePID + 2
}

// toolEnv is a real helper (Serve on a socket) with the MCP server connected to it over an in-memory
// transport, the way Claude Code talks to `rubi-home claude-mcp`.
type toolEnv struct {
	t       *testing.T
	c       *Claude
	fp      *fakeProcs
	cs      *mcp.ClientSession
	sock    string
	peer    int    // the process the calls come from
	session string // what the MCP server claims in its line (never used for identity)
	cwd     string
}

func newToolEnv(t *testing.T, c *Claude, session, cwd string) *toolEnv {
	t.Helper()
	fp, _ := c.Procs.(*fakeProcs)
	if fp == nil {
		fp = newFakeProcs()
		c.Procs = fp
	}
	e := &toolEnv{t: t, c: c, fp: fp, session: session, cwd: cwd}
	_, e.peer = fp.claude(c, session)
	e.sock = serveSocket(t, c)
	e.connect(e.sock)
	return e
}

func (e *toolEnv) connect(sock string) {
	e.t.Helper()
	srv := NewToolsServer(ToolsConfig{Sock: sock, Session: func() string { return e.session }, Cwd: func() string { return e.cwd }})
	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = srv.Run(ctx, st) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "claude", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { cs.Close(); cancel() })
	e.cs = cs
}

// call returns the tool's result and, for an error result, its text.
func (e *toolEnv) call(name string, args map[string]any) (map[string]any, string) {
	e.t.Helper()
	if e.fp != nil {
		e.fp.setPeer(e.peer)
	}
	res, err := e.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		e.t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return nil, b.String()
	}
	m, _ := res.StructuredContent.(map[string]any)
	if m == nil {
		e.t.Fatalf("%s: no structured result: %+v", name, res)
	}
	return m, ""
}

func (e *toolEnv) ok(name string, args map[string]any) map[string]any {
	e.t.Helper()
	m, errText := e.call(name, args)
	if errText != "" {
		e.t.Fatalf("%s failed: %s", name, errText)
	}
	return m
}

func (e *toolEnv) fails(name string, args map[string]any, want string) {
	e.t.Helper()
	m, errText := e.call(name, args)
	if m != nil || !strings.Contains(errText, want) {
		e.t.Fatalf("%s: result %v, error %q, want an error containing %q", name, m, errText, want)
	}
}

func cwdHook(event, session, cwd string, extra map[string]any) []byte {
	m := map[string]any{"cwd": cwd}
	for k, v := range extra {
		m[k] = v
	}
	return hook(event, session, m)
}

func TestToolsAreListedWithDescriptions(t *testing.T) {
	e := newToolEnv(t, liveClaude(t, "s1"), "s1", "/work")
	res, err := e.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		got[tl.Name] = tl
	}
	for _, n := range []string{"rubi_sisters", "rubi_ask", "rubi_reply", "rubi_inbox_status"} {
		if got[n] == nil || len(got[n].Description) < 40 {
			t.Errorf("tool %s: %+v", n, got[n])
		}
	}
	if len(got) != 4 {
		t.Errorf("%d tools", len(got))
	}
	for tool, props := range map[string][]string{"rubi_ask": {"text", "kind"}, "rubi_reply": {"message_id", "text"}} {
		b, _ := json.Marshal(got[tool].InputSchema)
		var sc struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		}
		_ = json.Unmarshal(b, &sc)
		for _, p := range props {
			if sc.Properties[p].Description == "" {
				t.Errorf("%s.%s has no description: %s", tool, p, b)
			}
		}
	}
}

func TestSistersToolAfterBound(t *testing.T) {
	c := liveClaude(t, "s1")
	e := newToolEnv(t, c, "s1", "/work")
	r := e.ok("rubi_sisters", nil)
	if s, _ := r["sisters"].([]any); len(s) != 0 || r["note"] == nil {
		t.Fatalf("before any binding: %v", r)
	}
	if _, err := c.handle(context.Background(), "claude-sessions.bound", json.RawMessage(`{"session_id":"s1","sister":"Vega","bound":true}`)); err != nil {
		t.Fatal(err)
	}
	r = e.ok("rubi_sisters", nil)
	s, _ := r["sisters"].([]any)
	if len(s) != 1 || s[0].(map[string]any)["name"] != "Vega" || s[0].(map[string]any)["since"] == nil {
		t.Fatalf("sisters: %v", r)
	}
	// Senders of delivered messages show up too.
	if _, _, err := c.Send("s1", "Orion", "hello"); err != nil {
		t.Fatal(err)
	}
	c.Hook(hook("UserPromptSubmit", "s1", nil))
	r = e.ok("rubi_sisters", nil)
	rs, _ := r["recent_senders"].([]any)
	if len(rs) != 1 || rs[0].(map[string]any)["name"] != "Orion" {
		t.Fatalf("recent_senders: %v", r)
	}
	// Unbinding forgets her; binding her to another session moves her.
	_, _ = c.handle(context.Background(), "claude-sessions.bound", json.RawMessage(`{"session_id":"s2","sister":"vega","bound":true}`))
	if got := c.bound["s1"]; len(got) != 0 {
		t.Fatalf("a Bot has one binding: %+v", c.bound)
	}
	_, _ = c.handle(context.Background(), "claude-sessions.bound", json.RawMessage(`{"session_id":"s2","sister":"Vega","bound":false}`))
	if len(c.bound) != 0 {
		t.Fatalf("%+v", c.bound)
	}
	for _, bad := range []string{`{"session_id":"","sister":"V","bound":true}`, `{"session_id":"s","sister":" ","bound":true}`, `[`} {
		if _, err := c.handle(context.Background(), "claude-sessions.bound", json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestAskToolQueuesLikeABlock(t *testing.T) {
	c := liveClaude(t, "s1")
	c.Hook(cwdHook("UserPromptSubmit", "s1", "/work", nil))
	e := newToolEnv(t, c, "s1", "/work")
	r := e.ok("rubi_ask", map[string]any{"text": " What is Go 1.26's release date? ", "kind": "lookup"})
	id, _ := r["id"].(string)
	if !strings.HasPrefix(id, "co_") || r["queued"] != true || r["warning"] == nil {
		t.Fatalf("result: %v", r)
	}
	r2 := e.ok("rubi_ask", map[string]any{"text": "latest advisories for X"})
	if r2["id"] == id {
		t.Fatal("ids must differ")
	}
	o := outs(c)
	if len(o) != 2 || o[0].Kind != "ask" || o[0].ID != id || o[0].SessionID != "s1" || o[0].AskKind != "lookup" ||
		o[0].Text != "What is Go 1.26's release date?" || o[0].Cwd != "/work" || o[1].AskKind != "research" {
		t.Fatalf("outbox: %+v", o)
	}
	// The same checks as a block: size, kinds, and at most 3 a minute.
	e.fails("rubi_ask", map[string]any{"text": strings.Repeat("x", claudeMaxAsk+1)}, "2000")
	e.fails("rubi_ask", map[string]any{"text": "  "}, "empty")
	e.fails("rubi_ask", map[string]any{"text": "q", "kind": "deploy"}, "kind must be")
	e.ok("rubi_ask", map[string]any{"text": strings.Repeat("x", claudeMaxAsk)})
	e.fails("rubi_ask", map[string]any{"text": "one too many"}, "at most 3 asks a minute")
	if len(outs(c)) != 1 {
		t.Fatal("the refused asks were queued")
	}
	// A bound Bot silences the warning.
	_ = c.setBound("s1", "Vega", true)
	c.mu.Lock()
	c.toolHits = nil
	c.mu.Unlock()
	if r := e.ok("rubi_ask", map[string]any{"text": "q"}); r["warning"] != nil {
		t.Fatalf("warning with a bound Bot: %v", r)
	}
}

// The ask reaches the plugin as the same outbox entry a ```rubi-ask``` block makes.
func TestAskToolEntryEqualsBlockEntry(t *testing.T) {
	c := liveClaude(t, "s1")
	c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": "```rubi-ask kind=audit\ncheck x\n```"}))
	block := outs(c)
	e := newToolEnv(t, c, "s1", "/work")
	e.ok("rubi_ask", map[string]any{"text": "check x", "kind": "audit"})
	tool := outs(c)
	if len(block) != 1 || len(tool) != 1 {
		t.Fatalf("%+v %+v", block, tool)
	}
	b, a := block[0], tool[0]
	if a.Kind != b.Kind || a.SessionID != b.SessionID || a.AskKind != b.AskKind || a.Text != b.Text || a.To != b.To || a.Cwd != b.Cwd {
		t.Fatalf("tool %+v, block %+v", a, b)
	}
}

func deliver(t *testing.T, c *Claude, session, from, text string) ClaudeMessage {
	t.Helper()
	m, _, err := c.Send(session, from, text)
	if err != nil {
		t.Fatal(err)
	}
	if r := c.Hook(hook("UserPromptSubmit", session, nil)); !strings.Contains(r.Context, m.ID) {
		t.Fatalf("not delivered: %+v", r)
	}
	return m
}

func TestReplyToolAnswersADeliveredMessage(t *testing.T) {
	c := liveClaude(t, "s1")
	e := newToolEnv(t, c, "s1", "/work")
	queued, _, _ := c.Send("s1", "Vega", "not yet delivered")
	e.fails("rubi_reply", map[string]any{"message_id": queued.ID, "text": "hi"}, "no message with that id was delivered")
	e.fails("rubi_reply", map[string]any{"message_id": "cm_unknown", "text": "hi"}, "no message with that id was delivered")
	c.Hook(hook("UserPromptSubmit", "s1", nil)) // delivers it
	m1 := queued
	e.fails("rubi_reply", map[string]any{"message_id": m1.ID, "text": " "}, "empty")
	e.fails("rubi_reply", map[string]any{"message_id": "", "text": "x"}, "message_id")
	outsAll(c)

	r := e.ok("rubi_reply", map[string]any{"message_id": m1.ID, "text": "done: the answer"})
	if r["to"] != "Vega" || r["truncated"] != false {
		t.Fatalf("result: %v", r)
	}
	all := outsAll(c)
	var replies []ClaudeOut
	var answered []string
	for _, o := range all {
		switch o.Kind {
		case "reply":
			replies = append(replies, o)
		case "status":
			if o.State == "answered" {
				answered = append(answered, o.ID+"->"+o.To)
			}
		}
	}
	if len(replies) != 1 || replies[0].To != "Vega" || len(replies[0].InReplyTo) != 1 || replies[0].InReplyTo[0] != m1.ID ||
		replies[0].Text != "done: the answer" || replies[0].SessionID != "s1" {
		t.Fatalf("replies: %+v", replies)
	}
	if len(answered) != 1 || answered[0] != m1.ID+"->Vega" {
		t.Fatalf("answered: %v", answered)
	}
	// The turn's final answer doesn't send it again.
	c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": "final answer"}))
	for _, o := range outsAll(c) {
		if o.Kind == "reply" || o.State == "answered" {
			t.Fatalf("duplicate after the final answer: %+v", o)
		}
	}
	// A message still owed is still answered by the final answer.
	m2 := deliver(t, c, "s1", "Orion", "second")
	m3, _, _ := c.Send("s1", "Vega", "third")
	_ = m3
	e.ok("rubi_reply", map[string]any{"message_id": m2.ID, "text": "early"})
	c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": "final", "stop_hook_active": false}))
	var final []ClaudeOut
	for _, o := range outsAll(c) {
		if o.Kind == "reply" && o.Text == "final" {
			final = append(final, o)
		}
	}
	if len(final) != 0 {
		t.Fatalf("m2's sender (Orion) got the final answer too: %+v", final)
	}
}

func TestReplyFinalAnswerStillGoesToOtherSenders(t *testing.T) {
	c := liveClaude(t, "s1")
	e := newToolEnv(t, c, "s1", "/work")
	m1, _, _ := c.Send("s1", "Vega", "a")
	m2, _, _ := c.Send("s1", "Orion", "b")
	c.Hook(hook("UserPromptSubmit", "s1", nil))
	outsAll(c)
	e.ok("rubi_reply", map[string]any{"message_id": m1.ID, "text": "early to Vega"})
	c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": "final for Orion"}))
	var to []string
	for _, o := range outsAll(c) {
		if o.Kind == "reply" && o.Text == "final for Orion" {
			to = append(to, o.To)
			if len(o.InReplyTo) != 1 || o.InReplyTo[0] != m2.ID {
				t.Fatalf("in_reply_to: %+v", o)
			}
		}
	}
	if len(to) != 1 || to[0] != "Orion" {
		t.Fatalf("final answer went to %v", to)
	}
}

func TestReplyOnlyForTheLast24Hours(t *testing.T) {
	c := liveClaude(t, "s1")
	e := newToolEnv(t, c, "s1", "/work")
	m := deliver(t, c, "s1", "Vega", "old")
	c.mu.Lock()
	c.delivered["s1"][0].At = time.Now().Add(-25 * time.Hour)
	c.mu.Unlock()
	e.fails("rubi_reply", map[string]any{"message_id": m.ID, "text": "late"}, "no message with that id was delivered")
	// A reply is limited to 3 a minute and clipped like a final answer.
	m2 := deliver(t, c, "s1", "Vega", "new")
	r := e.ok("rubi_reply", map[string]any{"message_id": m2.ID, "text": strings.Repeat("y", claudeMaxReply+5)})
	if r["truncated"] != true {
		t.Fatalf("%v", r)
	}
	// A second reply to an answered message is refused.
	e.fails("rubi_reply", map[string]any{"message_id": m2.ID, "text": "2"}, "already answered")
	// 3 replies a minute from one session, to different messages.
	m3 := deliver(t, c, "s1", "Vega", "3")
	m4 := deliver(t, c, "s1", "Vega", "4")
	m5 := deliver(t, c, "s1", "Vega", "5")
	e.ok("rubi_reply", map[string]any{"message_id": m3.ID, "text": "a"})
	e.ok("rubi_reply", map[string]any{"message_id": m4.ID, "text": "b"})
	e.fails("rubi_reply", map[string]any{"message_id": m5.ID, "text": "c"}, "at most 3 replies")
	// "answered" is announced once.
	n := 0
	for _, o := range outsAll(c) {
		if o.Kind == "status" && o.ID == m2.ID && o.State == "answered" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("answered emitted %d times", n)
	}
}

func TestFinalAnswerMarksDeliveredAnswered(t *testing.T) {
	c := liveClaude(t, "s1")
	e := newToolEnv(t, c, "s1", "/work")
	m := deliver(t, c, "s1", "Vega", "q")
	c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": "a"}))
	outsAll(c)
	// The final answer answered it: a reply afterwards is refused, and nothing is announced twice.
	e.fails("rubi_reply", map[string]any{"message_id": m.ID, "text": "more"}, "already answered")
	if len(outsAll(c)) != 0 {
		t.Fatal("a refused reply queued something")
	}
}

func TestInboxStatusTool(t *testing.T) {
	c := liveClaude(t, "s1")
	e := newToolEnv(t, c, "s1", "/work")
	r := e.ok("rubi_inbox_status", nil)
	if r["session_state"] != "idle" || r["waiting"] != float64(0) || r["waiter"] != false {
		t.Fatalf("empty: %v", r)
	}
	m1 := deliver(t, c, "s1", "Vega", "first")
	m2, _, _ := c.Send("s1", "Orion", "second")
	r = e.ok("rubi_inbox_status", nil)
	if r["session_state"] != "busy" || r["waiting"] != float64(1) {
		t.Fatalf("one waiting: %v", r)
	}
	owed, _ := r["owed_replies"].([]any)
	if len(owed) != 1 || owed[0].(map[string]any)["message_id"] != m1.ID || owed[0].(map[string]any)["from"] != "Vega" {
		t.Fatalf("owed: %v", r["owed_replies"])
	}
	states := map[string]string{}
	for _, x := range r["recent"].([]any) {
		mm := x.(map[string]any)
		states[mm["message_id"].(string)] = mm["state"].(string)
	}
	if states[m1.ID] != "delivered" || states[m2.ID] != "queued" {
		t.Fatalf("recent: %v", states)
	}
	e.ok("rubi_reply", map[string]any{"message_id": m1.ID, "text": "ok"})
	r = e.ok("rubi_inbox_status", nil)
	if owed, _ := r["owed_replies"].([]any); len(owed) != 0 {
		t.Fatalf("still owed: %v", owed)
	}
	for _, x := range r["recent"].([]any) {
		if mm := x.(map[string]any); mm["message_id"] == m1.ID && mm["state"] != "answered" {
			t.Fatalf("state: %v", mm)
		}
	}
	c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": "x"})) // blocks to deliver m2
	if r = e.ok("rubi_inbox_status", nil); r["waiting"] != float64(0) {
		t.Fatalf("after the stop block: %v", r)
	}
}

func TestInboxStatusSeesTheWaiter(t *testing.T) {
	c := liveClaude(t, "s1")
	e := newToolEnv(t, c, "s1", "/work")
	idle(c, "s1")
	startWaiter(e.sock, "s1", time.Minute)
	attached(t, c, "s1")
	if r := e.ok("rubi_inbox_status", nil); r["waiter"] != true {
		t.Fatalf("%v", r)
	}
}

// ---- who is calling ----

func TestToolSessionAnchorsOnTheClaudeProcess(t *testing.T) {
	c := liveClaude(t, "s1", "s2")
	fp := newFakeProcs()
	c.Procs = fp
	claude1, mcp1 := fp.claude(c, "s1")
	claude2, mcp2 := fp.claude(c, "s2")
	who := func(peer int) (string, error) { return c.sessionOfPeer(fp.id(peer)) }

	if s, err := who(mcp1); s != "s1" || err != nil {
		t.Fatalf("claude1 -> node mcp: %q %v", s, err)
	}
	// claude1 -> zsh -> go run: a raw client deep under the session is that session, never another.
	fp.add(500, claude1, "zsh")
	fp.add(501, 500, "go")
	fp.add(502, 501, "client")
	if s, err := who(502); s != "s1" || err != nil {
		t.Fatalf("claude1 -> zsh -> go -> client: %q %v", s, err)
	}
	if s, err := who(mcp2); s != "s2" || err != nil {
		t.Fatalf("claude2: %q %v", s, err)
	}
	_ = claude2
	// Only the claude processes were recorded: never the shared zsh or the app above them.
	c.peers.mu.Lock()
	for pid := range c.peers.m {
		if pid == 10 || pid == 20 {
			t.Errorf("pid %d (a shell or the app) was recorded", pid)
		}
	}
	if len(c.peers.m) != 2 {
		t.Errorf("recorded: %+v", c.peers.m)
	}
	c.peers.mu.Unlock()
	// A process under the shared shell, outside every claude, is nobody; so are missing and tiny pids.
	fp.add(600, 20, "sh")
	fp.add(700, 1)
	for _, pid := range []int{600, 700, 9999, 0, 1} {
		if s, err := who(pid); err == nil {
			t.Errorf("pid %d resolved to %q", pid, s)
		}
	}
	// A reused claude pid (same number, other start time) doesn't inherit the session.
	fp.mu.Lock()
	fp.start[claude1] += 7
	fp.mu.Unlock()
	if s, err := who(mcp1); err == nil {
		t.Fatalf("a reused claude pid resolved to %q", s)
	}
	// ... and when the new process hosts a session itself it starts over.
	fp.add(claude1+5, claude1, "rubi-home")
	fp.attest(c, "s3", claude1+5)
	if s, err := who(mcp1); s != "s3" || err != nil {
		t.Fatalf("after re-attesting: %q %v", s, err)
	}
	// The depth is limited: a client more than 6 levels below its claude process is not attributed.
	deep := claude2
	for i := 0; i < peerMaxDepth+1; i++ {
		fp.add(800+i, deep)
		deep = 800 + i
	}
	if s, err := who(deep); err == nil {
		t.Fatalf("a process %d levels deep resolved to %q", peerMaxDepth+1, s)
	}
}

// M1: a descendant of claude A that sends a hook line naming session B doesn't get A's process recorded as
// B's, can't call tools as B, and doesn't make A ambiguous.
func TestForgedHookLineFromInsideASessionIsIgnored(t *testing.T) {
	c := liveClaude(t, "A", "B")
	fp := newFakeProcs()
	c.Procs = fp
	claudeA, mcpA := fp.claude(c, "A")
	_, mcpB := fp.claude(c, "B")
	fp.add(500, claudeA, "go") // a `go run` of the session's own code
	fp.attest(c, "B", 500)     // ... sends a forged hook line for B
	if s, err := c.sessionOfPeer(fp.id(500)); s != "A" || err != nil {
		t.Fatalf("the forger resolves to %q %v, want its own session A", s, err)
	}
	if s, err := c.sessionOfPeer(fp.id(mcpA)); s != "A" || err != nil {
		t.Fatalf("A became %q %v", s, err)
	}
	if s, err := c.sessionOfPeer(fp.id(mcpB)); s != "B" || err != nil {
		t.Fatalf("B became %q %v", s, err)
	}
}

// M2: claude A crashes without SessionEnd; claude C starts under the same zsh and has no hook yet. C's
// tools are refused; they don't resolve to A.
func TestNewClaudeUnderTheSameShellDoesNotInheritACrashedSession(t *testing.T) {
	c := liveClaude(t, "A")
	fp := newFakeProcs()
	c.Procs = fp
	claudeA, _ := fp.claude(c, "A")
	for _, pid := range []int{claudeA, claudeA + 1, claudeA + 2} {
		fp.remove(pid) // gone, no SessionEnd
	}
	fp.add(900, 20, "claude")
	fp.add(901, 900, "node") // C's MCP server, before any hook of C
	if s, err := c.sessionOfPeer(fp.id(901)); err == nil {
		t.Fatalf("C's tool call resolved to %q", s)
	}
	// Once C's own hook arrives it is C's.
	fp.add(902, 900, "rubi-home")
	fp.attest(c, "C", 902)
	if s, err := c.sessionOfPeer(fp.id(901)); s != "C" || err != nil {
		t.Fatalf("after C's hook: %q %v", s, err)
	}
}

func TestNoClaudeAncestorFallsBackToTheDirectParent(t *testing.T) {
	c := liveClaude(t, "X")
	fp := newFakeProcs()
	c.Procs = fp
	fp.add(300, 20, "sh") // no process named claude anywhere above
	fp.add(301, 300, "rubi-home")
	fp.attest(c, "X", 301)
	c.peers.mu.Lock()
	if len(c.peers.m) != 1 || c.peers.m[300] == nil {
		c.peers.mu.Unlock()
		t.Fatalf("only the direct parent is recorded: %+v", c.peers.m)
	}
	c.peers.mu.Unlock()
	fp.add(302, 300, "node")
	if s, err := c.sessionOfPeer(fp.id(302)); s != "X" || err != nil {
		t.Fatalf("a sibling: %q %v", s, err)
	}
	// A deeper process has another direct parent and isn't attributed (and never reaches the shell).
	fp.add(303, 302, "bash")
	fp.add(304, 303, "client")
	if s, err := c.sessionOfPeer(fp.id(304)); err == nil {
		t.Fatalf("a grandchild resolved to %q", s)
	}
	// The shell itself holds nothing.
	fp.add(305, 20, "client")
	if s, err := c.sessionOfPeer(fp.id(305)); err == nil {
		t.Fatalf("a sibling of the shell's other children resolved to %q", s)
	}
}

func TestFirstClaimOnAProcessWins(t *testing.T) {
	c := liveClaude(t, "S1", "S2")
	fp := newFakeProcs()
	c.Procs = fp
	claude, mcpPID := fp.claude(c, "S1")
	fp.attest(c, "S2", claude+1) // another session's line from the same process
	if s, _ := c.sessionOfPeer(fp.id(mcpPID)); s != "S1" {
		t.Fatalf("the claim moved to %q", s)
	}
	// It is released by SessionEnd ...
	c.peers.forget("S1")
	fp.attest(c, "S2", claude+1)
	if s, _ := c.sessionOfPeer(fp.id(mcpPID)); s != "S2" {
		t.Fatalf("after S1 ended: %q", s)
	}
	// ... by 24 hours without a hook from it.
	c3 := liveClaude(t, "U", "V")
	fp3 := newFakeProcs()
	c3.Procs = fp3
	cl3, m3 := fp3.claude(c3, "U")
	c3.peers.mu.Lock()
	c3.peers.m[cl3].at = time.Now().Add(-peerClaimTTL - time.Minute)
	c3.peers.mu.Unlock()
	if s, err := c3.sessionOfPeer(fp3.id(m3)); err == nil {
		t.Fatalf("a claim silent for more than 24 h still resolves to %q", s)
	}
	fp3.attest(c3, "V", cl3+1)
	if s, _ := c3.sessionOfPeer(fp3.id(m3)); s != "V" {
		t.Fatalf("after the claim expired: %q", s)
	}
	// A refreshed claim of the same session stays.
	fp3.attest(c3, "V", cl3+1)
	if s, _ := c3.sessionOfPeer(fp3.id(m3)); s != "V" {
		t.Fatal("refresh")
	}
}

// The peer is read right after Accept: a pid or start time that changes afterwards is not followed.
func TestPeerIsFixedBeforeTheLineIsRead(t *testing.T) {
	c := liveClaude(t, "s1", "s2")
	fp := newFakeProcs()
	c.Procs = fp
	_, mcp1 := fp.claude(c, "s1")
	_, mcp2 := fp.claude(c, "s2")
	fp.setPeer(mcp1)
	p := c.peerOf(nil) // taken at Accept
	fp.setPeer(mcp2)   // whoever the socket says later
	if s, err := c.sessionOfPeer(p); s != "s1" || err != nil {
		t.Fatalf("%q %v", s, err)
	}
	// The pid was reused by another process between Accept and the lookup: refused.
	fp.mu.Lock()
	fp.start[mcp1]++
	fp.mu.Unlock()
	if s, err := c.sessionOfPeer(p); err == nil {
		t.Fatalf("a reused pid resolved to %q", s)
	}
	// No peer at all.
	fp.mu.Lock()
	fp.noPeer = true
	fp.mu.Unlock()
	if p := c.peerOf(nil); p.ok {
		t.Fatal("a peer without a pid")
	}
}

func TestPeerMapIsBounded(t *testing.T) {
	var p peerMap
	now := time.Now()
	for i := 2; i < peerMaxEntries+300; i++ {
		p.note("s", ancestor{i, int64(i)}, now.Add(time.Duration(i)*time.Millisecond))
	}
	if len(p.m) > peerMaxEntries {
		t.Fatalf("%d entries", len(p.m))
	}
	if _, ok := p.lookup(ancestor{peerMaxEntries + 299, int64(peerMaxEntries + 299)}, now.Add(time.Hour)); !ok {
		t.Fatal("the newest entry was evicted")
	}
}

// A session id or directory in the line is never used for identity: a process of s1 that claims s2 (and
// s2's directory) still gets s1's inbox and can't touch s2's.
func TestToolIgnoresWhatTheLineClaims(t *testing.T) {
	c := liveClaude(t, "s1", "s2")
	c.Hook(cwdHook("SessionStart", "s2", "/p2", nil))
	e1 := newToolEnv(t, c, "s1", "/work")
	e2 := newToolEnv(t, c, "s2", "/p2")
	m2 := deliver(t, c, "s2", "Vega", "for s2")
	e1.session, e1.cwd = "s2", "/p2" // a forged line
	r := e1.ok("rubi_inbox_status", nil)
	if r["recent"].([]any) != nil && len(r["recent"].([]any)) != 0 {
		t.Fatalf("s1's process saw s2's messages: %v", r)
	}
	e1.fails("rubi_reply", map[string]any{"message_id": m2.ID, "text": "hijack"}, "no message with that id was delivered")
	e1.ok("rubi_ask", map[string]any{"text": "from s1"})
	for _, o := range outs(c) {
		if o.SessionID != "s1" {
			t.Fatalf("an ask was queued for %s", o.SessionID)
		}
	}
	e2.ok("rubi_reply", map[string]any{"message_id": m2.ID, "text": "real"})
}

func TestToolsRefusedWhenThePeerIsUnknown(t *testing.T) {
	c := liveClaude(t, "s1", "s2") // both in /work
	e := newToolEnv(t, c, "s1", "/work")
	_ = newToolEnv(t, c, "s2", "/work")
	// A stranger, with a session id and directory that match a live session.
	e.fp.add(900, 1)
	e.peer = 900
	for _, tool := range []string{"rubi_sisters", "rubi_inbox_status"} {
		_, text := e.call(tool, nil)
		if !strings.Contains(text, "isn't known to Rubi Home yet") || !strings.Contains(text, "next prompt") {
			t.Fatalf("%s: %q", tool, text)
		}
		for _, leak := range []string{"s1", "s2", "/work"} {
			if strings.Contains(text, leak) {
				t.Errorf("the error names %q: %s", leak, text)
			}
		}
	}
	e.fails("rubi_ask", map[string]any{"text": "q"}, "isn't known")
	if len(outs(c)) != 0 {
		t.Fatal("a stranger's ask was queued")
	}
	// No peer pid from the kernel (an OS without it): fail closed, even for a known process.
	_, e.peer = e.fp.claude(c, "s9")
	e.ok("rubi_sisters", nil)
	e.fp.mu.Lock()
	e.fp.noPeer = true
	e.fp.mu.Unlock()
	e.fails("rubi_sisters", nil, "isn't known")
	// An ended session has no tools.
	e.fp.mu.Lock()
	e.fp.noPeer = false
	e.fp.mu.Unlock()
	c.peers.forget("s9") // SessionEnd
	e.fails("rubi_sisters", nil, "isn't known")
}

// Hook lines over the real socket attest the peer's claude process; SessionEnd forgets it.
func TestHookLinesAttestOverTheSocket(t *testing.T) {
	c := liveClaude(t)
	fp := newFakeProcs()
	c.Procs = fp
	claude, mcpPID := 100, 102
	fp.add(claude, 20, "claude")
	fp.add(101, claude, "rubi-home")
	fp.add(mcpPID, claude, "node")
	sock := serveSocket(t, c)
	send := func(line string, peer int) {
		fp.setPeer(peer)
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = conn.Write([]byte(line + "\n"))
		_, _ = readLine(conn, 1<<20)
	}
	who := func() (string, error) { return c.sessionOfPeer(fp.id(mcpPID)) }
	if _, err := who(); err == nil {
		t.Fatal("known before any hook")
	}
	send(`{"hook_event_name":"SessionStart","session_id":"sx","cwd":"/work"}`, 101)
	if s, err := who(); s != "sx" || err != nil {
		t.Fatalf("after SessionStart: %q %v", s, err)
	}
	send(`{"hook_event_name":"SessionEnd","session_id":"sx","cwd":"/work"}`, 101)
	if _, err := who(); err == nil {
		t.Fatal("known after SessionEnd")
	}
	for _, ev := range []string{"UserPromptSubmit", "Stop"} {
		send(`{"hook_event_name":"`+ev+`","session_id":"sy"}`, 101)
		if s, err := who(); s != "sy" || err != nil {
			t.Fatalf("after %s: %q %v", ev, s, err)
		}
		c.peers.forget("sy")
	}
	// A line of an event that isn't a session hook (RubiTool itself) attests nothing.
	send(`{"hook_event_name":"RubiTool","tool":"rubi_sisters","session_id":"sz"}`, 101)
	if s, err := who(); err == nil {
		t.Fatalf("a tool line attested %q", s)
	}
}

// The real kernel: the peer pid of a unix socket connection is this process, and its parent and start time
// are readable.
func TestRealPeerPID(t *testing.T) {
	sock := filepath.Join(shortDir(t), "peer.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan int, 1)
	errc := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			errc <- err
			return
		}
		defer conn.Close()
		pid, err := osProcs{}.PeerPID(conn)
		if err != nil {
			errc <- err
			return
		}
		got <- pid
	}()
	cl, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	select {
	case pid := <-got:
		if pid != os.Getpid() {
			t.Fatalf("peer pid %d, want %d", pid, os.Getpid())
		}
	case err := <-errc:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
		t.Fatal("no peer pid")
	}
	if _, err := (osProcs{}).PeerPID(nil); err == nil {
		t.Error("a nil connection has a peer")
	}
	ppid, start, name, err := osProcs{}.Info(os.Getpid())
	if err != nil || ppid != os.Getppid() || start == 0 || name == "" {
		t.Fatalf("Info: %d %d %q %v (parent %d)", ppid, start, name, err, os.Getppid())
	}
	if want := filepath.Base(os.Args[0]); !strings.HasPrefix(want, name) && !strings.HasPrefix(name, want[:min(len(want), 15)]) {
		t.Errorf("name %q for %q", name, want)
	}
	_, start2, _, _ := osProcs{}.Info(os.Getpid())
	if start2 != start {
		t.Fatal("the start time changes")
	}
	if _, _, _, err := (osProcs{}).Info(1 << 30); err == nil {
		t.Error("a missing process has info")
	}
	// The real peer of a connection can be anchored (its parent exists, unless this process is a child of init).
	if os.Getppid() > 1 {
		if _, ok := anchor(osProcs{}, peerID{os.Getpid(), start, true}); !ok {
			t.Error("no anchor for this process")
		}
	}
}

func TestToolsWithAnOlderHelper(t *testing.T) {
	sock := filepath.Join(shortDir(t), "old.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = readLine(conn, 1<<20)
			_, _ = conn.Write([]byte("{}\n")) // an unknown event is a no-op
			conn.Close()
		}
	}()
	e := &toolEnv{t: t, session: "s1", cwd: "/work"}
	e.connect(sock)
	for _, tool := range []string{"rubi_sisters", "rubi_inbox_status"} {
		e.fails(tool, nil, "too old")
	}
	e.fails("rubi_ask", map[string]any{"text": "q"}, "install-hook")
	// No helper at all.
	e.connect(filepath.Join(shortDir(t), "none.sock"))
	e.fails("rubi_sisters", nil, "isn't running")
}

// The line the MCP server sends is stamped like a hook's, and an older helper's event path ignores it.
func TestToolLineIsStampedAndInvisibleToTheEventPath(t *testing.T) {
	var got []byte
	sock := filepath.Join(shortDir(t), "spy.sock")
	ln, _ := net.Listen("unix", sock)
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		got, _ = readLine(conn, 1<<20)
		_, _ = conn.Write([]byte(`{"ok":true,"sisters":[]}` + "\n"))
	}()
	if _, err := callTool(context.Background(), ToolsConfig{Sock: sock, Session: func() string { return "s1" }, Cwd: func() string { return "/w" }}, "rubi_sisters", nil); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(got, &m); err != nil {
		t.Fatalf("%s: %v", got, err)
	}
	if m["hook_event_name"] != "RubiTool" || m["tool"] != "rubi_sisters" || m["claude_session_id"] != "s1" || m["tool_cwd"] != "/w" ||
		m["rubi_hook_protocol"] != float64(ClaudeHookProtocol) || m["rubi_hook_version"] == nil {
		t.Fatalf("line: %s", got)
	}
	if _, has := m["session_id"]; has {
		t.Fatalf("an older helper's event path would register this session: %s", got)
	}
	old := &Claude{}
	if r := old.hook(got, nil); r != (HookReply{}) {
		t.Fatalf("%+v", r)
	}
	if old.reg.stateOf("s1") != "" || old.reg.cwdOf("s1") != "" {
		t.Fatal("an older helper registered a session from a tool line")
	}
}

func TestEnvSessionIDIsUsedByDefault(t *testing.T) {
	t.Setenv(SessionEnvVar, "  from-env ")
	sock := filepath.Join(shortDir(t), "spy.sock")
	ln, _ := net.Listen("unix", sock)
	defer ln.Close()
	got := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		l, _ := readLine(conn, 1<<20)
		got <- l
		_, _ = conn.Write([]byte(`{"ok":true}` + "\n"))
	}()
	if _, err := callTool(context.Background(), ToolsConfig{Sock: sock}, "rubi_sisters", nil); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(<-got, &m)
	wd, _ := os.Getwd()
	if m["claude_session_id"] != "from-env" || m["tool_cwd"] != wd {
		t.Fatalf("%v", m)
	}
}

// A RubiTool line is neither an approval nor an event, whatever else it holds.
func TestRubiToolIsNotAnApprovalOrAnEvent(t *testing.T) {
	c := liveClaude(t, "s1")
	fp := newFakeProcs()
	c.Procs = fp
	_, mcpPID := fp.claude(c, "s1")
	fp.setPeer(mcpPID)
	c.Wait = 20 * time.Second
	sock := serveSocket(t, c)
	send := func(line string) map[string]any {
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = conn.Write([]byte(line + "\n"))
		resp, err := readLine(conn, 1<<20)
		if err != nil {
			t.Fatalf("no answer: %v", err)
		}
		var m map[string]any
		_ = json.Unmarshal(resp, &m)
		return m
	}
	before := c.reg.stateOf("s1")
	m := send(`{"hook_event_name":"RubiTool","tool":"Bash","session_id":"s1","claude_session_id":"s1","tool_name":"Bash","tool_input":{"command":"rm -rf /"},"decision":"allow","behavior":"allow"}`)
	if m["behavior"] != nil || m["error"] == nil {
		t.Fatalf("answer: %v", m)
	}
	m = send(`{"hook_event_name":"RubiTool","tool":"rubi_inbox_status","session_id":"s1","claude_session_id":"s1","tool_cwd":"/work","tool_name":"Bash","tool_input":{"command":"x"}}`)
	if m["ok"] != true || m["behavior"] != nil {
		t.Fatalf("answer: %v", m)
	}
	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 0 || c.reg.stateOf("s1") != before {
		t.Fatalf("%d approvals pending, state %q -> %q", n, before, c.reg.stateOf("s1"))
	}
	// A tool call changes no session: not even its state or its waiters.
	if c.reg.stateOf("ghost") != "" {
		t.Fatal("a session appeared")
	}
	// A tool call can't decide anything: there's no op for it, and Decide only takes ids Rubi was handed.
	if err := c.Decide(ClaudeAnswer{ID: "cc_x", Behavior: "allow"}); err == nil {
		t.Fatal("decided")
	}
}

func TestToolSlotsAreSeparateAndBounded(t *testing.T) {
	c := liveClaude(t, "s1")
	slots := make(chan struct{}, claudeMaxToolCalls)
	for i := 0; i < claudeMaxToolCalls; i++ {
		slots <- struct{}{}
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go c.serveTool(a, []byte(`{"tool":"rubi_sisters","claude_session_id":"s1"}`), slots, peerID{})
	_ = b.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := readLine(b, 1<<16)
	if err != nil || !strings.Contains(string(line), "too many tool calls") {
		t.Fatalf("%s %v", line, err)
	}
	if claudeMaxToolCalls != 16 {
		t.Fatalf("slots: %d", claudeMaxToolCalls)
	}
	// Event and permission slots are other channels: Serve builds each of its own.
	s := &claudeSlots{tools: make(chan struct{}, claudeMaxToolCalls), events: make(chan struct{}, claudeMaxEvents)}
	if cap(s.tools) == cap(s.events) && claudeMaxEvents != claudeMaxToolCalls {
		t.Fatal("shared capacity")
	}
}

// A handler that can't get the helper's lock is answered by the deadline, and releases its slot after.
func TestToolDeadline(t *testing.T) {
	old := claudeToolDeadline
	claudeToolDeadline = 100 * time.Millisecond
	defer func() { claudeToolDeadline = old }()
	c := liveClaude(t, "s1")
	fp := newFakeProcs()
	c.Procs = fp
	_, mcpPID := fp.claude(c, "s1")
	fp.setPeer(mcpPID)
	slots := make(chan struct{}, 2)
	c.mu.Lock()
	a, b := net.Pipe()
	defer b.Close()
	done := make(chan struct{})
	go func() {
		c.serveTool(a, []byte(`{"tool":"rubi_inbox_status","claude_session_id":"s1"}`), slots, fp.id(mcpPID))
		a.Close()
		close(done)
	}()
	_ = b.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	line, err := readLine(b, 1<<16)
	if err != nil || !strings.Contains(string(line), "took too long") || time.Since(start) > time.Second {
		t.Fatalf("%s %v after %s", line, err, time.Since(start))
	}
	<-done
	if len(slots) != 1 {
		t.Fatalf("the stuck handler keeps its slot while it runs: %d", len(slots))
	}
	c.mu.Unlock()
	waitFor(t, "the slot", func() bool { return len(slots) == 0 })
}

func TestToolBurstHoldsSlots(t *testing.T) {
	c := liveClaude(t, "s1")
	fp := newFakeProcs()
	c.Procs = fp
	_, mcpPID := fp.claude(c, "s1")
	fp.setPeer(mcpPID)
	sock := serveSocket(t, c)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := callTool(context.Background(), ToolsConfig{Sock: sock, Session: func() string { return "s1" }, Cwd: func() string { return "/work" }}, "rubi_inbox_status", nil)
			if err != nil && !strings.Contains(err.Error(), "too many tool calls") {
				t.Errorf("%v %v", r, err)
			}
		}()
	}
	wg.Wait()
}

func TestBoundAndDeliveredPersist(t *testing.T) {
	file := filepath.Join(t.TempDir(), ClaudeQueueFile)
	c := liveClaude(t, "s1")
	c.QueueFile = file
	_ = c.setBound("s1", "Vega", true)
	m := deliver(t, c, "s1", "Vega", "hello")
	if err := c.saveQueue(); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	b, _ := os.ReadFile(file)
	_ = json.Unmarshal(b, &raw)
	if raw["version"] != float64(1) || raw["bound"] == nil || raw["delivered"] == nil {
		t.Fatalf("file: %s", b)
	}
	d := &Claude{QueueFile: file}
	d.loadQueue()
	if bs := d.bound["s1"]; len(bs) != 1 || bs[0].Sister != "Vega" || bs[0].Since.IsZero() {
		t.Fatalf("bound: %+v", d.bound)
	}
	if ds := d.delivered["s1"]; len(ds) != 1 || ds[0].ID != m.ID || ds[0].From != "Vega" {
		t.Fatalf("delivered: %+v", d.delivered)
	}
	// A restarted helper can still take the reply.
	d.Hook(hook("SessionStart", "s1", nil))
	e := newToolEnv(t, d, "s1", "/work")
	e.ok("rubi_reply", map[string]any{"message_id": m.ID, "text": "after restart"})
	// A file written before these keys existed loads.
	old := []byte(`{"version":1,"saved_at":"2026-01-01T00:00:00Z","inbox":{"s9":[{"id":"cm_1","from":"V","text":"t","at":"` + time.Now().UTC().Format(time.RFC3339) + `"}]}}`)
	f2 := filepath.Join(t.TempDir(), ClaudeQueueFile)
	_ = os.WriteFile(f2, old, 0o600)
	g := &Claude{QueueFile: f2}
	g.loadQueue()
	if len(g.inbox["s9"]) != 1 || len(g.bound) != 0 {
		t.Fatalf("%+v", g.inbox)
	}
}

func TestDeliveredIsBoundedAndExpires(t *testing.T) {
	c := liveClaude(t, "s1")
	c.mu.Lock()
	for i := 0; i < claudeMaxDelivered+30; i++ {
		c.noteDeliveredLocked("s1", ClaudeMessage{ID: "cm_" + string(rune('A'+i%26)) + string(rune('a'+i/26)), From: "V"})
	}
	n := len(c.delivered["s1"])
	c.delivered["s1"][0].At = time.Now().Add(-30 * time.Hour)
	c.sweepLocked(time.Now().UTC())
	n2 := len(c.delivered["s1"])
	c.mu.Unlock()
	if n != claudeMaxDelivered || n2 != claudeMaxDelivered-1 {
		t.Fatalf("%d then %d", n, n2)
	}
}

func TestTurnReleaseKeepsTheRestOwed(t *testing.T) {
	tn := &turnState{}
	tn.note([]ClaudeMessage{{ID: "a", From: "V"}, {ID: "b", From: "O"}, {ID: "c", From: "V"}})
	tn.release("b")
	if len(tn.ids) != 2 || len(tn.from) != 1 || tn.from[0] != "V" || len(tn.idFrom) != 2 {
		t.Fatalf("%+v", tn)
	}
	tn.release("zzz")
	tn.release("a")
	tn.release("c")
	if len(tn.ids) != 0 || len(tn.from) != 0 {
		t.Fatalf("%+v", tn)
	}
	old := &turnState{from: []string{"V"}, ids: []string{"a"}} // restored from an older file: no idFrom
	old.release("a")
	if len(old.from) != 0 {
		t.Fatalf("%+v", old)
	}
}

func TestToolRateKeysAreBounded(t *testing.T) {
	c := &Claude{}
	now := time.Now()
	c.mu.Lock()
	for i := 0; i < claudeMaxToolKeys+200; i++ {
		c.toolAllowLocked("ask|phantom"+string(rune('a'+i%26))+strings.Repeat("x", i/26), now.Add(time.Duration(i)*time.Millisecond))
	}
	n := len(c.toolHits)
	_, newest := c.toolHits["ask|phantom"+string(rune('a'+(claudeMaxToolKeys+199)%26))+strings.Repeat("x", (claudeMaxToolKeys+199)/26)]
	c.mu.Unlock()
	if n > claudeMaxToolKeys || !newest {
		t.Fatalf("%d keys, newest kept %v", n, newest)
	}
	// The limit of a busy key survives the eviction of others (no wipe).
	c.mu.Lock()
	for i := 0; i < claudeToolsPerMin; i++ {
		c.toolAllowLocked("ask|busy", now.Add(time.Hour))
	}
	for i := 0; i < 100; i++ {
		c.toolAllowLocked("ask|other"+strings.Repeat("y", i), now.Add(time.Hour+time.Millisecond))
	}
	ok := c.toolAllowLocked("ask|busy", now.Add(time.Hour+2*time.Millisecond))
	c.mu.Unlock()
	if ok {
		t.Fatal("a busy session's limit was wiped by other keys")
	}
}

func TestToolsHaveAGlobalCap(t *testing.T) {
	c := liveClaude(t)
	fp := newFakeProcs()
	c.Procs = fp
	var envs []*toolEnv
	for i := 0; i < 8; i++ {
		id := "sess" + string(rune('a'+i))
		c.Hook(hook("SessionStart", id, nil))
		envs = append(envs, newToolEnv(t, c, id, "/work"))
	}
	n := 0
	for _, e := range envs {
		for k := 0; k < claudeToolsPerMin; k++ {
			if _, errText := e.call("rubi_ask", map[string]any{"text": "q"}); errText == "" {
				n++
			} else if !strings.Contains(errText, "sister tool messages a minute") {
				t.Fatalf("unexpected error: %s", errText)
			}
		}
	}
	if n != claudeToolsGlobal {
		t.Fatalf("%d asks passed, the cap is %d a minute", n, claudeToolsGlobal)
	}
}

func TestInboxStatusCountsQueuedOutgoing(t *testing.T) {
	c := liveClaude(t, "s1")
	e := newToolEnv(t, c, "s1", "/work")
	e.ok("rubi_ask", map[string]any{"text": "q"})
	if r := e.ok("rubi_inbox_status", nil); r["queued_outgoing"] != float64(1) {
		t.Fatalf("%v", r)
	}
	outsAll(c)
	if r := e.ok("rubi_inbox_status", nil); r["queued_outgoing"] != float64(0) {
		t.Fatalf("%v", r)
	}
}

func TestTurnReleaseOfAnOlderSavedTurn(t *testing.T) {
	// A turn saved before senders were kept per message: the released message's sender mustn't stay owed.
	old := &turnState{from: []string{"V"}, ids: []string{"a", "b"}}
	old.release("a")
	old.release("b")
	if len(old.from) != 0 || len(old.ids) != 0 {
		t.Fatalf("%+v", old)
	}
	mixed := &turnState{from: []string{"V", "O"}, ids: []string{"a", "b"}, idFrom: []string{"V"}} // idFrom shorter
	mixed.release("a")
	for _, f := range mixed.from {
		if f == "V" && len(mixed.ids) == 1 && mixed.sender(0) != "V" {
			t.Fatalf("V stays owed: %+v", mixed)
		}
	}
	if len(mixed.ids) != 1 || len(mixed.from) != 1 || mixed.from[0] != mixed.sender(0) {
		t.Fatalf("%+v", mixed)
	}
}

func TestLoadQueueSanitizesBoundAndDelivered(t *testing.T) {
	now := time.Now().UTC()
	q := queueFile{Version: 1, SavedAt: now,
		Bound: map[string][]boundSister{
			"s1": {{"Vega", now.Add(-time.Hour)}, {"vega", now}, {"A", now.Add(-time.Minute)}, {"B", now.Add(-time.Minute)}, {"C", now.Add(-time.Minute)}, {"D", now.Add(-time.Minute)}, {"E", now.Add(-time.Minute)}},
			"s2": {{"Vega", now.Add(-2 * time.Hour)}, {"  ", now}},
			"":   {{"X", now}},
		},
		Delivered: map[string][]deliveredMsg{
			"s1": {{ID: "cm_ABC123", From: "Vega\nINJECT" + strings.Repeat("x", 200), At: now.Add(48 * time.Hour), Answered: true},
				{ID: "not-a-message-id", From: "V", At: now}, {ID: "cm_" + strings.Repeat("A", 80), From: "V", At: now}},
		}}
	b, _ := json.Marshal(q)
	file := filepath.Join(t.TempDir(), ClaudeQueueFile)
	_ = os.WriteFile(file, b, 0o600)
	c := &Claude{QueueFile: file}
	c.loadQueue()
	total := 0
	vega := 0
	for id, bs := range c.bound {
		if id == "" || len(bs) > claudeMaxBoundPerSession {
			t.Errorf("%q: %+v", id, bs)
		}
		for _, x := range bs {
			total++
			if strings.EqualFold(x.Sister, "vega") {
				vega++
			}
		}
	}
	if vega != 1 || len(c.bound["s1"]) == 0 {
		t.Fatalf("one binding per Bot: %+v", c.bound)
	}
	if v := c.bound["s1"]; !(len(v) > 0) {
		t.Fatal("s1 lost its bound list")
	}
	ds := c.delivered["s1"]
	if len(ds) != 1 || ds[0].ID != "cm_ABC123" || strings.ContainsAny(ds[0].From, "\n") || len([]rune(ds[0].From)) > claudeMaxFrom ||
		ds[0].At.After(time.Now().Add(time.Second)) || !ds[0].Answered {
		t.Fatalf("delivered: %+v", ds)
	}
}

// A SessionEnd line releases a claim only when it comes from the process that holds it.
func TestSessionEndFromAnotherTreeDoesNotReleaseTheClaim(t *testing.T) {
	c := liveClaude(t)
	fp := newFakeProcs()
	c.Procs = fp
	claudeA, mcpA := fp.claude(c, "A")
	fp.add(600, 20, "sh") // outside every claude tree
	fp.add(601, 600, "rubi-home")
	sock := serveSocket(t, c)
	end := func(peer int) {
		fp.setPeer(peer)
		conn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = conn.Write([]byte(`{"hook_event_name":"SessionEnd","session_id":"A"}` + "\n"))
		_, _ = readLine(conn, 1<<20)
	}
	end(601) // forged, from outside A's tree
	if s, err := c.sessionOfPeer(fp.id(mcpA)); s != "A" || err != nil {
		t.Fatalf("a SessionEnd from another tree released A's claim: %q %v", s, err)
	}
	fp.add(500, claudeA, "go") // inside A's tree: it may end A's claim (as /clear does)
	end(500)
	if s, err := c.sessionOfPeer(fp.id(mcpA)); err == nil {
		t.Fatalf("A's own SessionEnd didn't release: %q", s)
	}
}

// A claim whose session has been silent for 10 minutes moves to another session's hook from the same process.
func TestSilentClaimCanBeTakenOver(t *testing.T) {
	c := liveClaude(t)
	fp := newFakeProcs()
	c.Procs = fp
	claude, mcpPID := fp.claude(c, "old")
	age := func(d time.Duration) {
		c.peers.mu.Lock()
		c.peers.m[claude].at = time.Now().Add(-d)
		c.peers.mu.Unlock()
	}
	age(peerTakeoverIdle - time.Minute)
	fp.attest(c, "new", claude+1)
	if s, _ := c.sessionOfPeer(fp.id(mcpPID)); s != "old" {
		t.Fatalf("taken over after only %s: %q", peerTakeoverIdle-time.Minute, s)
	}
	age(peerTakeoverIdle + time.Minute)
	fp.attest(c, "new", claude+1)
	if s, _ := c.sessionOfPeer(fp.id(mcpPID)); s != "new" {
		t.Fatalf("not taken over after %s: %q", peerTakeoverIdle+time.Minute, s)
	}
	// The session that just lost its claim can't take it back while the new one is active.
	fp.attest(c, "old", claude+1)
	if s, _ := c.sessionOfPeer(fp.id(mcpPID)); s != "new" {
		t.Fatalf("taken back at once: %q", s)
	}
}
