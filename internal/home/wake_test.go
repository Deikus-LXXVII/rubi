package home

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortDir is a temp directory with a path short enough for a unix socket (long test names are not).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "rh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type waitResult struct {
	code   int
	stderr string
	took   time.Duration
}

const stopJSON = `{"hook_event_name":"Stop","session_id":"%s","cwd":"/work","stop_hook_active":false}`

// startWaiter runs `claude-hook --wait` for the session against sock.
func startWaiter(sock, session string, limit time.Duration) <-chan waitResult {
	out := make(chan waitResult, 1)
	go func() {
		var errb bytes.Buffer
		start := time.Now()
		code := RunClaudeWait(strings.NewReader(fmt.Sprintf(stopJSON, session)), &errb, sock, limit)
		out <- waitResult{code, errb.String(), time.Since(start)}
	}()
	return out
}

func gotResult(t *testing.T, ch <-chan waitResult) waitResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter didn't return")
		return waitResult{}
	}
}

func attached(t *testing.T, c *Claude, session string) {
	t.Helper()
	waitFor(t, "the waiter to attach", func() bool { return c.hasWaiter(session) })
}

func idle(c *Claude, sessions ...string) {
	for _, s := range sessions {
		c.Hook(hook("Stop", s, nil))
	}
}

func inboxLen(c *Claude, id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.inbox[id])
}

func statusStates(c *Claude, msgID string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, o := range c.outbox {
		if o.Kind == "status" && o.ID == msgID {
			out = append(out, o.State)
		}
	}
	return out
}

func TestWaiterIsWokenBySend(t *testing.T) {
	c := liveClaude(t, "s1")
	idle(c, "s1")
	sock := serveSocket(t, c)
	w := startWaiter(sock, "s1", time.Minute)
	attached(t, c, "s1")

	m, _, err := c.Send("s1", "Vega", "Which tests cover the parser?")
	if err != nil {
		t.Fatal(err)
	}
	r := gotResult(t, w)
	if r.code != 2 || !strings.Contains(r.stderr, "Which tests cover the parser?") || !strings.Contains(r.stderr, "While this session was idle") ||
		!strings.Contains(r.stderr, "sent back to the sender") {
		t.Fatalf("waiter: %+v", r)
	}
	waitFor(t, "the delivered status", func() bool { return strings.Join(statusStates(c, m.ID), ",") == "queued,delivered" })
	if n := inboxLen(c, "s1"); n != 0 {
		t.Fatalf("%d messages still in the inbox", n)
	}
	if c.hasWaiter("s1") {
		waitFor(t, "the waiter to go", func() bool { return !c.hasWaiter("s1") })
	}
	if c.reg.stateOf("s1") != "busy" {
		t.Fatalf("a woken session is in a turn, not %q", c.reg.stateOf("s1"))
	}
	// The woken turn ends: its answer goes back to the sender.
	c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": "parser_test.go"}))
	var reply *ClaudeOut
	for _, o := range outsAll(c) {
		if o.Kind == "reply" {
			reply = &o
		}
	}
	if reply == nil || reply.To != "Vega" || reply.Text != "parser_test.go" || len(reply.InReplyTo) != 1 || reply.InReplyTo[0] != m.ID {
		t.Fatalf("reply: %+v", reply)
	}
	// outsAll drained the box; the answered status was part of it.
	c.Hook(hook("UserPromptSubmit", "s1", nil))
	if s := c.Hook(hook("Stop", "s1", nil)); s.Block {
		t.Fatalf("nothing should be delivered twice: %+v", s)
	}
}

func TestAnsweredStatus(t *testing.T) {
	c := liveClaude(t, "s1")
	m, _, _ := c.Send("s1", "Vega", "q")
	c.Hook(hook("UserPromptSubmit", "s1", nil))
	c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": "a"}))
	if got := strings.Join(statusStates(c, m.ID), ","); got != "queued,delivered,answered" {
		t.Fatalf("states: %s", got)
	}
	for _, o := range outsAll(c) {
		if o.Kind == "status" && (o.To != "Vega" || o.SessionID != "s1") {
			t.Fatalf("status: %+v", o)
		}
	}
}

func TestMessagesWaitingWakeTheWaiterAtConnect(t *testing.T) {
	c := liveClaude(t, "s1")
	idle(c, "s1")
	sock := serveSocket(t, c)
	for _, text := range []string{"first", "second"} {
		if _, _, err := c.Send("s1", "Vega", text); err != nil {
			t.Fatal(err)
		}
	}
	r := gotResult(t, startWaiter(sock, "s1", time.Minute))
	i, j := strings.Index(r.stderr, "first"), strings.Index(r.stderr, "second")
	if r.code != 2 || i < 0 || j < i {
		t.Fatalf("waiter: %+v", r)
	}
	waitFor(t, "the inbox to be empty", func() bool { return inboxLen(c, "s1") == 0 })
}

func TestNewerWaiterClosesTheOlder(t *testing.T) {
	c := liveClaude(t, "s1")
	idle(c, "s1")
	sock := serveSocket(t, c)
	first := startWaiter(sock, "s1", time.Minute)
	attached(t, c, "s1")
	c.wmu.Lock()
	old := c.waiters["s1"]
	c.wmu.Unlock()
	second := startWaiter(sock, "s1", time.Minute)
	waitFor(t, "the newer waiter", func() bool {
		c.wmu.Lock()
		defer c.wmu.Unlock()
		return c.waiters["s1"] != nil && c.waiters["s1"] != old
	})
	if r := gotResult(t, first); r.code != 0 || r.stderr != "" {
		t.Fatalf("older waiter: %+v", r)
	}
	if _, _, err := c.Send("s1", "Vega", "hello"); err != nil {
		t.Fatal(err)
	}
	if r := gotResult(t, second); r.code != 2 || !strings.Contains(r.stderr, "hello") {
		t.Fatalf("newer waiter: %+v", r)
	}
}

func TestPromptAndSessionEndCloseTheWaiter(t *testing.T) {
	for _, ev := range []string{"UserPromptSubmit", "SessionEnd"} {
		t.Run(ev, func(t *testing.T) {
			c := liveClaude(t, "s1")
			idle(c, "s1")
			sock := serveSocket(t, c)
			w := startWaiter(sock, "s1", time.Minute)
			attached(t, c, "s1")
			c.Hook(hook(ev, "s1", nil))
			if r := gotResult(t, w); r.code != 0 || r.stderr != "" {
				t.Fatalf("waiter: %+v", r)
			}
			if c.hasWaiter("s1") {
				t.Fatal("the waiter is still registered")
			}
			if ev == "UserPromptSubmit" { // the message waits for the prompt, which delivers it once
				m, _, err := c.Send("s1", "Vega", "after the close")
				if err != nil {
					t.Fatal(err)
				}
				if r := c.Hook(hook("UserPromptSubmit", "s1", nil)); !strings.Contains(r.Context, "after the close") {
					t.Fatalf("prompt: %+v", r)
				}
				if r := c.Hook(hook("Stop", "s1", nil)); r.Block {
					t.Fatalf("delivered twice: %+v", r)
				}
				if got := strings.Join(statusStates(c, m.ID), ","); got != "queued,delivered" {
					t.Fatalf("states: %s", got)
				}
			}
		})
	}
}

// A Stop that blocks (the helper's own delivery) closes the waiter and nothing is delivered twice.
func TestStopBlockClosesTheWaiter(t *testing.T) {
	c := liveClaude(t, "s1")
	sock := serveSocket(t, c)
	w := startWaiter(sock, "s1", time.Minute)
	attached(t, c, "s1")
	// A message that arrived before the waiter registered would have woken it; here the Stop hook takes it
	// first, so the waiter has nothing to deliver.
	c.mu.Lock()
	c.inbox = map[string][]ClaudeMessage{"s1": {{ID: "cm_x", From: "Vega", Text: "t", At: time.Now()}}}
	c.mu.Unlock()
	r := c.Hook(hook("Stop", "s1", nil))
	if !r.Block {
		t.Fatalf("stop: %+v", r)
	}
	if res := gotResult(t, w); res.code != 0 {
		t.Fatalf("waiter: %+v", res)
	}
}

func TestWakeWithoutAckPutsMessagesBack(t *testing.T) {
	old := claudeWakeAck
	claudeWakeAck = 150 * time.Millisecond
	defer func() { claudeWakeAck = old }()
	for _, how := range []string{"closes", "stays silent"} {
		t.Run(how, func(t *testing.T) {
			c := liveClaude(t, "s1")
			idle(c, "s1")
			sock := serveSocket(t, c)
			var ids []string
			for _, text := range []string{"one", "two", "three"} {
				m, _, err := c.Send("s1", "Vega", text)
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, m.ID)
			}
			// A raw client plays the waiter hook.
			conn, err := net.Dial("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			fmt.Fprintf(conn, `{"hook_event_name":"RubiWait","session_id":"s1"}`+"\n")
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			line, err := readLine(conn, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			var wk struct {
				Wake string   `json:"wake"`
				IDs  []string `json:"ids"`
			}
			if json.Unmarshal(line, &wk) != nil || len(wk.IDs) != 3 || !strings.Contains(wk.Wake, "three") {
				t.Fatalf("wake line: %s", line)
			}
			if how == "closes" {
				conn.Close()
			}
			waitFor(t, "the messages to come back", func() bool { return inboxLen(c, "s1") == 3 })
			c.mu.Lock()
			for i, m := range c.inbox["s1"] {
				if m.ID != ids[i] {
					t.Errorf("message %d is %s, want %s", i, m.ID, ids[i])
				}
			}
			c.mu.Unlock()
			for _, id := range ids {
				if got := strings.Join(statusStates(c, id), ","); got != "queued" {
					t.Fatalf("states of %s: %s", id, got)
				}
			}
			c.mu.Lock()
			noted := len(c.turns["s1"].from)
			c.mu.Unlock()
			if noted != 0 {
				t.Fatal("senders were noted for an undelivered wake")
			}
			// They are delivered by the next prompt instead.
			if r := c.Hook(hook("UserPromptSubmit", "s1", nil)); !strings.Contains(r.Context, "one") || !strings.Contains(r.Context, "three") {
				t.Fatalf("prompt: %+v", r)
			}
		})
	}
}

func TestOldHelperAnswersEmpty(t *testing.T) {
	sock := filepath.Join(shortDir(t), "old.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = readLine(conn, 1<<20)
		_, _ = conn.Write([]byte("{}\n")) // an unknown event is a no-op
	}()
	if r := gotResult(t, startWaiter(sock, "s1", time.Minute)); r.code != 0 || r.stderr != "" {
		t.Fatalf("waiter: %+v", r)
	}
	if code := RunClaudeWait(strings.NewReader(`{"session_id":"s1"}`), new(bytes.Buffer), filepath.Join(shortDir(t), "none.sock"), time.Second); code != 0 {
		t.Fatalf("no helper: exit %d", code)
	}
	if code := RunClaudeWait(strings.NewReader(`not json`), new(bytes.Buffer), sock, time.Second); code != 0 {
		t.Fatalf("bad input: exit %d", code)
	}
}

// RubiWait is neither a PermissionRequest nor a normal event for this helper, whatever else the line holds.
func TestRubiWaitIsNotAnApprovalOrAnEvent(t *testing.T) {
	c := liveClaude(t, "s1")
	c.Wait = 20 * time.Second
	sock := serveSocket(t, c)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, `{"hook_event_name":"RubiWait","session_id":"s1","tool_name":"Bash","tool_input":{"command":"rm -rf /"}}`+"\n")
	waitFor(t, "the waiter", func() bool { return c.hasWaiter("s1") })
	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d approvals pending", n)
	}
	_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if line, err := readLine(conn, 1<<20); err == nil {
		t.Fatalf("a waiting waiter got an answer: %s", line)
	}
	if got := c.reg.stateOf("s1"); got != "idle" {
		t.Fatalf("a waiter must not change the session state: %q", got)
	}
	// A session that has ended has nothing to wait for.
	c.Hook(hook("SessionEnd", "s1", nil))
	conn2, _ := net.Dial("unix", sock)
	defer conn2.Close()
	fmt.Fprintf(conn2, `{"hook_event_name":"RubiWait","session_id":"s1"}`+"\n")
	_ = conn2.SetReadDeadline(time.Now().Add(3 * time.Second))
	if line, err := readLine(conn2, 1<<20); err != nil || string(line) != `{"close":true}` {
		t.Fatalf("ended session: %s %v", line, err)
	}
}

func TestWaiterSlotsAreSeparateAndBounded(t *testing.T) {
	c := liveClaude(t, "s1")
	sock := serveSocket(t, c)
	var conns []net.Conn
	defer func() {
		for _, cn := range conns {
			cn.Close()
		}
	}()
	for i := 0; i < claudeMaxWaiters; i++ {
		cn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, cn)
		fmt.Fprintf(cn, `{"hook_event_name":"RubiWait","session_id":"w%d"}`+"\n", i)
	}
	waitFor(t, "all waiters", func() bool {
		c.wmu.Lock()
		defer c.wmu.Unlock()
		return len(c.waiters) == claudeMaxWaiters
	})
	if r := gotResult(t, startWaiter(sock, "extra", time.Minute)); r.code != 0 || r.took > 2*time.Second {
		t.Fatalf("waiter over the slots: %+v", r)
	}
	cn, _ := net.Dial("unix", sock)
	defer cn.Close()
	fmt.Fprintf(cn, `{"hook_event_name":"RubiWait","session_id":"extra2"}`+"\n")
	_ = cn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if line, err := readLine(cn, 1<<20); err != nil || string(line) != "{}" {
		t.Fatalf("full slots: %s %v", line, err)
	}
	// Events and approvals are not held up by waiters.
	start := time.Now()
	c.Hook([]byte(hookEvents()[0]))
	conn, _ := net.Dial("unix", sock)
	defer conn.Close()
	_, _ = conn.Write([]byte(hookEvents()[1] + "\n"))
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := readLine(conn, 1<<20); err != nil || time.Since(start) > time.Second {
		t.Fatalf("event with full waiter slots: %v after %s", err, time.Since(start))
	}
}

func TestWaiterGivesUpAtItsLimit(t *testing.T) {
	c := liveClaude(t, "s1")
	sock := serveSocket(t, c)
	r := gotResult(t, startWaiter(sock, "s1", 200*time.Millisecond))
	if r.code != 0 || r.stderr != "" || r.took < 150*time.Millisecond || r.took > 3*time.Second {
		t.Fatalf("waiter: %+v", r)
	}
	waitFor(t, "the waiter to go", func() bool { return !c.hasWaiter("s1") })
}

func TestStopOfServeReleasesWaiters(t *testing.T) {
	c := liveClaude(t, "s1")
	path := filepath.Join(shortDir(t), ClaudeSocket)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Serve(ctx, path) }()
	waitFor(t, "the socket", func() bool { _, err := os.Stat(path); return err == nil })
	w := startWaiter(path, "s1", time.Minute)
	attached(t, c, "s1")
	cancel()
	if r := gotResult(t, w); r.code != 0 {
		t.Fatalf("waiter: %+v", r)
	}
	<-done
}

func TestSendResultSaysWhetherASessionIsWoken(t *testing.T) {
	c := liveClaude(t, "s1")
	idle(c, "s1")
	send := func() map[string]any {
		v, err := c.handle(context.Background(), "claude-sessions.send", json.RawMessage(`{"session_id":"s1","from":"Vega","text":"hi"}`))
		if err != nil {
			t.Fatal(err)
		}
		return v.(map[string]any)
	}
	if r := send(); r["session_state"] != "idle" || r["waiter"] != false {
		t.Fatalf("no waiter: %v", r)
	}
	c.takeLocked("s1", time.Now()) // an inbox without the message
	c.Hook(hook("UserPromptSubmit", "s1", nil))
	if r := send(); r["session_state"] != "busy" {
		t.Fatalf("busy: %v", r)
	}
	c.Hook(hook("UserPromptSubmit", "s1", nil)) // takes the message the busy send left
	sock := serveSocket(t, c)
	idle(c, "s1")
	w := startWaiter(sock, "s1", time.Minute)
	attached(t, c, "s1")
	if ss := c.Sessions("", true); len(ss) != 1 || !ss[0].Waiter {
		t.Fatalf("the list doesn't show the waiter: %+v", ss)
	}
	if r := send(); r["waiter"] != true {
		t.Fatalf("waiter: %v", r)
	}
	gotResult(t, w)
}

func TestHooksStayFastWithWaitersAndASlowSaver(t *testing.T) {
	dir := t.TempDir()
	c := liveClaude(t, "s1")
	c.QueueFile = filepath.Join(dir, ClaudeQueueFile)
	block := make(chan struct{})
	entered := make(chan struct{}, 1)
	c.writeQueue = func(string, []byte) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-block
		return nil
	}
	oldDelay := claudeSaveDelay
	claudeSaveDelay = time.Millisecond
	defer func() { claudeSaveDelay = oldDelay }()
	sock := serveSocket(t, c)
	t.Cleanup(func() { close(block) }) // runs before serveSocket's cleanup
	idle(c, "s1")
	w := startWaiter(sock, "s1", time.Minute)
	attached(t, c, "s1")
	c.mu.Lock()
	c.dirtyLocked()
	c.mu.Unlock()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("the saver didn't start writing")
	}
	for _, raw := range hookEvents() {
		start := time.Now()
		c.Hook([]byte(raw))
		if took := time.Since(start); took > time.Second {
			t.Fatalf("%s took %s", hookEvent([]byte(raw)), took)
		}
	}
	start := time.Now()
	if _, _, err := c.Send("s1", "Vega", "while the disk is slow"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("send took %s", took)
	}
	if r := gotResult(t, w); r.code == 2 {
		if !strings.Contains(r.stderr, "while the disk is slow") {
			t.Fatalf("waiter: %+v", r)
		}
	}
}

// ---- status entries ----

func TestOutboxEvictsStatusesFirst(t *testing.T) {
	c := &Claude{}
	for i := 0; i < claudeMaxOut; i++ {
		c.pushOutLocked(ClaudeOut{Kind: "status", ID: fmt.Sprintf("m%d", i), State: "queued"})
	}
	c.pushOutLocked(ClaudeOut{Kind: "reply", To: "Vega", Text: "r1"})
	if len(c.outbox) != claudeMaxOut || c.outbox[0].ID != "m1" || c.outbox[len(c.outbox)-1].Text != "r1" {
		t.Fatalf("the oldest status should have gone: %d, first %s", len(c.outbox), c.outbox[0].ID)
	}
	// Full of replies and asks: a status is dropped itself, a reply evicts the oldest.
	c = &Claude{}
	for i := 0; i < claudeMaxOut; i++ {
		c.pushOutLocked(ClaudeOut{Kind: "reply", To: "Vega", Text: fmt.Sprint("r", i), InReplyTo: []string{fmt.Sprint("m", i)}})
	}
	c.pushOutLocked(ClaudeOut{Kind: "status", ID: "mx", State: "queued"})
	for _, o := range c.outbox {
		if o.Kind == "status" {
			t.Fatalf("a status pushed out a reply: %+v", o)
		}
	}
	c.pushOutLocked(ClaudeOut{Kind: "ask", Text: "a"})
	if len(c.outbox) != claudeMaxOut || c.outbox[0].Text != "r1" || c.outbox[len(c.outbox)-1].Kind != "ask" {
		t.Fatalf("outbox: first %q len %d", c.outbox[0].Text, len(c.outbox))
	}
}

// ---- persistence ----

func TestQueueRoundTrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), ClaudeQueueFile)
	c := liveClaude(t, "s1", "s2")
	c.QueueFile = file
	_, _, _ = c.Send("s1", "Vega", "waiting one")
	_, _, _ = c.Send("s1", "Orion", "waiting two")
	// s2: a turn that carried a message from Vega and whose answer is still owed.
	m3, _, _ := c.Send("s2", "Vega", "q")
	c.Hook(hook("UserPromptSubmit", "s2", nil))
	if _, _, err := c.Send("s2", "Orion", "more"); err != nil {
		t.Fatal(err)
	}
	c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": "x"})) // blocks, delivers s1's two
	c.mu.Lock()
	c.pushOutLocked(ClaudeOut{Kind: "ask", SessionID: "s2", Text: "what is go 1.26?", AskKind: "lookup"})
	c.mu.Unlock()
	if err := c.saveQueue(); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("queue file: %v %v", fi, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(file), ".claude-queue-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}

	d := &Claude{QueueFile: file}
	d.loadQueue()
	if len(d.inbox["s2"]) != 1 || d.inbox["s2"][0].Text != "more" {
		t.Fatalf("inbox: %+v", d.inbox)
	}
	if tn := d.turns["s2"]; tn == nil || len(tn.from) != 1 || tn.from[0] != "Vega" || len(tn.ids) != 1 || tn.ids[0] != m3.ID ||
		len(tn.idFrom) != 1 || tn.idFrom[0] != "Vega" {
		t.Fatalf("owed: %+v", d.turns["s2"])
	}
	c.mu.Lock()
	want := append([]ClaudeOut(nil), c.outbox...)
	c.mu.Unlock()
	if len(d.outbox) != len(want) {
		t.Fatalf("outbox: %d entries, want %d", len(d.outbox), len(want))
	}
	for i := range want {
		if d.outbox[i].ID != want[i].ID || d.outbox[i].Kind != want[i].Kind || d.outbox[i].State != want[i].State || d.outbox[i].Text != want[i].Text {
			t.Fatalf("outbox[%d]: %+v, want %+v", i, d.outbox[i], want[i])
		}
	}
	// The owed reply still goes back after a restart.
	d.Hook(hook("Stop", "s2", map[string]any{"last_assistant_message": "the answer"}))
	var reply bool
	for _, o := range outs(d) {
		reply = reply || (o.Kind == "reply" && o.To == "Vega" && o.Text == "the answer" && len(o.InReplyTo) == 1 && o.InReplyTo[0] == m3.ID)
	}
	if !reply {
		t.Fatal("the owed reply wasn't sent after loading")
	}
}

func TestInflightMessagesAreSaved(t *testing.T) {
	file := filepath.Join(t.TempDir(), ClaudeQueueFile)
	c := liveClaude(t, "s1")
	c.QueueFile = file
	m, _, _ := c.Send("s1", "Vega", "on its way")
	if got := c.takeForWake("s1"); len(got) != 1 {
		t.Fatal("nothing taken")
	}
	if err := c.saveQueue(); err != nil {
		t.Fatal(err)
	}
	d := &Claude{QueueFile: file}
	d.loadQueue()
	if len(d.inbox["s1"]) != 1 || d.inbox["s1"][0].ID != m.ID {
		t.Fatalf("inbox: %+v", d.inbox)
	}
}

func TestQueueTTL(t *testing.T) {
	file := filepath.Join(t.TempDir(), ClaudeQueueFile)
	c := liveClaude(t, "s1")
	c.QueueFile = file
	m, _, _ := c.Send("s1", "Vega", "old news")
	fresh, _, _ := c.Send("s1", "Vega", "fresh")
	c.mu.Lock()
	c.inbox["s1"][0].At = time.Now().Add(-25 * time.Hour)
	c.outbox = append(c.outbox, ClaudeOut{ID: "co_old", Kind: "reply", To: "Vega", At: time.Now().Add(-25 * time.Hour)})
	c.mu.Unlock()
	if err := c.saveQueue(); err != nil { // saving sweeps too
		t.Fatal(err)
	}
	d := &Claude{QueueFile: file}
	d.loadQueue()
	if len(d.inbox["s1"]) != 1 || d.inbox["s1"][0].ID != fresh.ID {
		t.Fatalf("inbox: %+v", d.inbox)
	}
	var expired bool
	for _, o := range d.outbox {
		if o.ID == "co_old" {
			t.Fatal("an old outbox entry was kept")
		}
		expired = expired || (o.Kind == "status" && o.ID == m.ID && o.State == "expired" && o.To == "Vega")
	}
	if !expired {
		t.Fatalf("no expired status: %+v", d.outbox)
	}
	// A message that expires between sweeps isn't delivered either.
	c.mu.Lock()
	c.inbox["s1"][0].At = time.Now().Add(-25 * time.Hour)
	c.mu.Unlock()
	if r := c.Hook(hook("UserPromptSubmit", "s1", nil)); r.Context != "" {
		t.Fatalf("an expired message was delivered: %+v", r)
	}
	if got := strings.Join(statusStates(c, fresh.ID), ","); got != "queued,expired" {
		t.Fatalf("states: %s", got)
	}
}

func TestCorruptQueueFileIsMovedAside(t *testing.T) {
	for name, body := range map[string]string{"garbage": "{not json", "version": `{"version":99}`} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), ClaudeQueueFile)
			if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			c := &Claude{QueueFile: file}
			c.loadQueue()
			if len(c.inbox) != 0 || len(c.outbox) != 0 {
				t.Fatal("not empty")
			}
			if b, err := os.ReadFile(file + ".bad"); err != nil || string(b) != body {
				t.Fatalf(".bad: %q %v", b, err)
			}
			if _, err := os.Stat(file); err == nil {
				t.Fatal("the corrupt file is still in place")
			}
			if err := c.saveQueue(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSaverWritesAfterChangesAndOnStop(t *testing.T) {
	old := claudeSaveDelay
	claudeSaveDelay = 20 * time.Millisecond
	defer func() { claudeSaveDelay = old }()
	file := filepath.Join(t.TempDir(), ClaudeQueueFile)
	c := liveClaude(t, "s1")
	c.QueueFile = file
	path := filepath.Join(shortDir(t), ClaudeSocket)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Serve(ctx, path) }()
	waitFor(t, "the socket", func() bool { _, err := os.Stat(path); return err == nil })
	if _, _, err := c.Send("s1", "Vega", "saved soon"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the queue file", func() bool { b, _ := os.ReadFile(file); return strings.Contains(string(b), "saved soon") })
	if _, _, err := c.Send("s1", "Vega", "saved at the end"); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-done
	b, _ := os.ReadFile(file)
	if !strings.Contains(string(b), "saved at the end") {
		t.Fatalf("no final save: %s", b)
	}
}

func TestNoQueueFileMeansNoPersistence(t *testing.T) {
	c := liveClaude(t, "s1")
	if _, _, err := c.Send("s1", "Vega", "x"); err != nil {
		t.Fatal(err)
	}
	if err := c.saveQueue(); err != nil || c.saveKick != nil {
		t.Fatalf("saved without a file: %v", err)
	}
}

// ---- review follow-ups ----

// A waiter that is attached while the session is busy doesn't wake it: the messages wait for the turn's hooks.
func TestBusySessionIsNotWoken(t *testing.T) {
	t.Run("prompt after the waiter connected", func(t *testing.T) {
		c := liveClaude(t, "s1")
		idle(c, "s1")
		sock := serveSocket(t, c)
		w := startWaiter(sock, "s1", time.Minute)
		attached(t, c, "s1")
		c.reg.touch("s1", "", "", "", "busy", time.Now()) // the prompt began; its close of the waiter is late
		m, _, err := c.Send("s1", "Vega", "mid-turn")
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
		if inboxLen(c, "s1") != 1 || !c.hasWaiter("s1") {
			t.Fatalf("a busy session was woken: inbox %d", inboxLen(c, "s1"))
		}
		if r := c.Hook(hook("UserPromptSubmit", "s1", nil)); !strings.Contains(r.Context, "mid-turn") {
			t.Fatalf("prompt: %+v", r)
		}
		if r := gotResult(t, w); r.code != 0 || r.stderr != "" {
			t.Fatalf("waiter: %+v", r)
		}
		if got := strings.Join(statusStates(c, m.ID), ","); got != "queued,delivered" {
			t.Fatalf("states: %s", got)
		}
	})
	t.Run("stop blocks then the waiter connects", func(t *testing.T) {
		c := liveClaude(t, "s1")
		sock := serveSocket(t, c)
		_, _, _ = c.Send("s1", "Vega", "first")
		if r := c.Hook(hook("Stop", "s1", nil)); !r.Block {
			t.Fatalf("stop: %+v", r)
		}
		w := startWaiter(sock, "s1", time.Minute) // the parallel waiter hook of the same Stop, late
		attached(t, c, "s1")
		m, _, _ := c.Send("s1", "Vega", "second")
		time.Sleep(100 * time.Millisecond)
		if inboxLen(c, "s1") != 1 {
			t.Fatal("a busy session was woken")
		}
		// The continuation's Stop delivers it; the waiter is closed by the blocking Stop.
		if r := c.Hook(hook("Stop", "s1", map[string]any{"stop_hook_active": true})); !r.Block || !strings.Contains(r.Reason, "second") {
			t.Fatalf("stop: %+v", r)
		}
		if r := gotResult(t, w); r.code != 0 {
			t.Fatalf("waiter: %+v", r)
		}
		if got := strings.Join(statusStates(c, m.ID), ","); got != "queued,delivered" {
			t.Fatalf("states: %s", got)
		}
	})
	t.Run("send after the session went idle wakes", func(t *testing.T) {
		c := liveClaude(t, "s1")
		idle(c, "s1")
		sock := serveSocket(t, c)
		w := startWaiter(sock, "s1", time.Minute)
		attached(t, c, "s1")
		c.reg.touch("s1", "", "", "", "busy", time.Now())
		c.reg.touch("s1", "", "", "", "idle", time.Now())
		if _, _, err := c.Send("s1", "Vega", "now idle"); err != nil {
			t.Fatal(err)
		}
		if r := gotResult(t, w); r.code != 2 || !strings.Contains(r.stderr, "now idle") {
			t.Fatalf("waiter: %+v", r)
		}
	})
	t.Run("waiter wakes at the stop that doesn't block", func(t *testing.T) {
		c := liveClaude(t, "s1")
		idle(c, "s1")
		sock := serveSocket(t, c)
		w := startWaiter(sock, "s1", time.Minute)
		attached(t, c, "s1")
		c.reg.touch("s1", "", "", "", "busy", time.Now())
		// A message that Stop will not take (the chain is used up) stays; the Stop's kick wakes the waiter.
		_, _, _ = c.Send("s1", "Vega", "after the chain")
		time.Sleep(50 * time.Millisecond)
		c.mu.Lock()
		c.turnLocked("s1").chain = claudeMaxChain
		c.mu.Unlock()
		if r := c.Hook(hook("Stop", "s1", map[string]any{"stop_hook_active": true})); r.Block {
			t.Fatalf("stop blocked: %+v", r)
		}
		if r := gotResult(t, w); r.code != 2 || !strings.Contains(r.stderr, "after the chain") {
			t.Fatalf("waiter: %+v", r)
		}
	})
}

// The waiter says ok before it writes the text, and a failure after the wake is exit 0.
func TestWaiterAcksBeforeWriting(t *testing.T) {
	sock := filepath.Join(shortDir(t), "fake.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	acked := make(chan string, 1)
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_, _ = readLine(conn, 1<<20)
			_, _ = conn.Write([]byte(`{"wake":"text","ids":["a"]}` + "\n"))
			l, _ := readLine(conn, 100)
			if i == 0 {
				acked <- string(l)
			}
			conn.Close()
		}
	}()
	var errb bytes.Buffer
	code := RunClaudeWait(strings.NewReader(fmt.Sprintf(stopJSON, "s1")), &errb, sock, time.Minute)
	if code != 2 || errb.String() != "text\n" || <-acked != "ok" {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	// A broken stderr (a panic in the writer) still exits 0.
	if code := RunClaudeWait(strings.NewReader(fmt.Sprintf(stopJSON, "s1")), nil, sock, time.Minute); code != 0 {
		t.Fatalf("panic: exit %d", code)
	}
}

func TestLoadQueueLimits(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, ClaudeQueueFile)
	t.Run("too large", func(t *testing.T) {
		if err := os.WriteFile(file, bytes.Repeat([]byte(" "), claudeMaxQueueFile+1), 0o600); err != nil {
			t.Fatal(err)
		}
		c := &Claude{QueueFile: file}
		c.loadQueue()
		if _, err := os.Stat(file + ".bad"); err != nil {
			t.Fatalf("not moved aside: %v", err)
		}
		os.Remove(file + ".bad")
	})
	t.Run("clamped", func(t *testing.T) {
		q := queueFile{Version: 1, Inbox: map[string][]ClaudeMessage{}}
		for i := 0; i < claudeMaxInbox+5; i++ {
			q.Inbox["s1"] = append(q.Inbox["s1"], ClaudeMessage{ID: fmt.Sprint("m", i), From: "V", Text: strings.Repeat("x", claudeMaxText+10), At: time.Now()})
		}
		for i := 0; i < claudeMaxOut+5; i++ {
			q.Outbox = append(q.Outbox, ClaudeOut{ID: fmt.Sprint("o", i), Kind: "reply", Text: strings.Repeat("y", claudeMaxReply+10), At: time.Now()})
		}
		q.Outbox = append(q.Outbox, ClaudeOut{ID: "bogus", Kind: "weird", At: time.Now()})
		b, _ := json.Marshal(q)
		if err := os.WriteFile(file, b, 0o600); err != nil {
			t.Fatal(err)
		}
		c := &Claude{QueueFile: file}
		c.loadQueue()
		if len(c.inbox["s1"]) != claudeMaxInbox || len([]rune(c.inbox["s1"][0].Text)) != claudeMaxText {
			t.Fatalf("inbox: %d, %d runes", len(c.inbox["s1"]), len([]rune(c.inbox["s1"][0].Text)))
		}
		if len(c.outbox) != claudeMaxOut || len([]rune(c.outbox[0].Text)) != claudeMaxReply {
			t.Fatalf("outbox: %d", len(c.outbox))
		}
		for _, o := range c.outbox {
			if o.Kind == "weird" {
				t.Fatal("unknown kind kept")
			}
		}
	})
}
