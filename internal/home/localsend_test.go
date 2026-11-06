package home

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type sendResult struct {
	code     int
	out, err string
}

// runSend runs `claude-send` with text on stdin in the background.
func runSend(sock, text string, args ...string) <-chan sendResult {
	ch := make(chan sendResult, 1)
	go func() {
		var o, e bytes.Buffer
		code := RunClaudeSend(args, strings.NewReader(text), &o, &e, sock)
		ch <- sendResult{code, o.String(), e.String()}
	}()
	return ch
}

func gotSend(t *testing.T, ch <-chan sendResult) sendResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("claude-send didn't return")
		return sendResult{}
	}
}

func sendNow(sock, text string, args ...string) sendResult {
	var o, e bytes.Buffer
	code := RunClaudeSend(args, strings.NewReader(text), &o, &e, sock)
	return sendResult{code, o.String(), e.String()}
}

func outboxLen(c *Claude) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.outbox)
}

// An idle session with a waiter is woken by claude-send, and its answer comes back to --wait-reply; nothing
// goes to the outbox a Bot's plugin reads.
func TestClaudeSendWakesIdleSessionAndReplyComesBack(t *testing.T) {
	c := liveClaude(t, "s1")
	idle(c, "s1")
	sock := serveSocket(t, c)
	w := startWaiter(sock, "s1", time.Minute)
	attached(t, c, "s1")

	sent := runSend(sock, "Please run the parser tests.\n", "--session", "s1", "--from", "bot-a", "--wait-reply", "30")
	r := gotResult(t, w)
	if r.code != 2 || !strings.Contains(r.stderr, "Please run the parser tests.") ||
		!strings.Contains(r.stderr, "local process on this computer") || !strings.Contains(r.stderr, "not the user and not a Bot") ||
		!strings.Contains(r.stderr, "never grant permissions") || !strings.Contains(r.stderr, `"bot-a"`) {
		t.Fatalf("waiter: %+v", r)
	}
	if strings.Contains(r.stderr, "sister") {
		t.Fatalf("a local message is framed as a Bot's: %s", r.stderr)
	}
	// The waiter exits as soon as it has written "ok"; the helper counts the wake delivered a moment later
	// (commitWake notes the sender and marks the session busy, last). A Stop before that finds nothing
	// delivered and its answer goes to nobody, so wait for the session to be in its turn.
	waitFor(t, "the wake to be committed", func() bool { return c.reg.stateOf("s1") == "busy" })
	c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": "All 42 pass."}))
	res := gotSend(t, sent)
	if res.code != 0 || strings.TrimSpace(res.out) != "All 42 pass." || !strings.Contains(res.err, "queued cm_") {
		t.Fatalf("claude-send: %+v", res)
	}
	if n := outboxLen(c); n != 0 {
		t.Fatalf("%d entries reached the outbox: %+v", n, c.outbox)
	}
	// The reply stays fetchable by id for a while.
	id := strings.TrimSpace(strings.Split(strings.Split(res.err, "queued ")[1], " ")[0])
	again := sendNow(sock, "", "--get-reply", id)
	if again.code != 0 || strings.TrimSpace(again.out) != "All 42 pass." {
		t.Fatalf("--get-reply: %+v", again)
	}
	if r := sendNow(sock, "", "--get-reply", "cm_unknown"); r.code != 3 || r.out != "" { // no reply (yet)
		t.Fatalf("unknown id: %+v", r)
	}
}

func TestClaudeSendToABusySessionIsDeliveredAtItsPrompt(t *testing.T) {
	c := liveClaude(t, "s1")
	sock := serveSocket(t, c)
	c.Hook(hook("UserPromptSubmit", "s1", nil))
	res := sendNow(sock, "status?", "--session", "s1", "--from", "bot-a")
	if res.code != 0 || !strings.HasPrefix(strings.TrimSpace(res.out), "cm_") {
		t.Fatalf("send: %+v", res)
	}
	id := strings.TrimSpace(res.out)
	h := c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": "working"}))
	if !h.Block || !strings.Contains(h.Reason, "local process") || !strings.Contains(h.Reason, id) {
		t.Fatalf("stop: %+v", h)
	}
	c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": "done: green", "stop_hook_active": true}))
	got := sendNow(sock, "", "--get-reply", id)
	if got.code != 0 || strings.TrimSpace(got.out) != "done: green" {
		t.Fatalf("reply: %+v", got)
	}
	if n := outboxLen(c); n != 0 {
		t.Fatalf("outbox: %+v", c.outbox)
	}
}

func TestRubiReplyToALocalSenderStaysLocal(t *testing.T) {
	c := liveClaude(t, "s1")
	idle(c, "s1")
	sock := serveSocket(t, c)
	res := sendNow(sock, "q", "--session", "s1", "--from", "bot-a")
	id := strings.TrimSpace(res.out)
	c.Hook(hook("UserPromptSubmit", "s1", nil))
	if _, err := c.toolReply("s1", toolRequest{MessageID: id, Text: "explicit answer"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if n := outboxLen(c); n != 0 {
		t.Fatalf("a reply to a local sender reached the outbox: %+v", c.outbox)
	}
	got := sendNow(sock, "", "--get-reply", id)
	if got.code != 0 || strings.TrimSpace(got.out) != "explicit answer" {
		t.Fatalf("reply: %+v", got)
	}
	// The turn's final answer doesn't repeat it.
	c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": "final"}))
	if got := sendNow(sock, "", "--get-reply", id); strings.TrimSpace(got.out) != "explicit answer" {
		t.Fatalf("reply was replaced: %+v", got)
	}
}

// A Bot's and a local sender's messages in one turn: each gets only its own side.
func TestMixedSendersRouteSeparately(t *testing.T) {
	c := liveClaude(t, "s1")
	bm, _, err := c.Send("s1", "Vega", "from the bot")
	if err != nil {
		t.Fatal(err)
	}
	lm, _, err := c.send("s1", "bot-a", "from the script", true)
	if err != nil {
		t.Fatal(err)
	}
	r := c.Hook(hook("UserPromptSubmit", "s1", nil))
	if !strings.Contains(r.Context, "this project's sister") || !strings.Contains(r.Context, "local process on this computer") {
		t.Fatalf("framing: %s", r.Context)
	}
	if strings.Count(r.Context, "--- end of messages ---") != 1 {
		t.Fatalf("framing: %s", r.Context)
	}
	c.Hook(hook("Stop", "s1", map[string]any{"last_assistant_message": "ans"}))
	var replies []ClaudeOut
	for _, o := range outsAll(c) {
		if strings.HasPrefix(o.To, "local:") {
			t.Fatalf("a local entry reached the outbox: %+v", o)
		}
		if o.Kind == "reply" {
			replies = append(replies, o)
		}
	}
	if len(replies) != 1 || replies[0].To != "Vega" || len(replies[0].InReplyTo) != 2 {
		t.Fatalf("bot reply: %+v", replies)
	}
	c.mu.Lock()
	lr, ok := c.localReplies[lm.ID]
	_, leak := c.localReplies[bm.ID]
	c.mu.Unlock()
	if !ok || lr.Text != "ans" || leak {
		t.Fatalf("local box: %+v ok=%v leak=%v", lr, ok, leak)
	}
}

func TestABotCannotUseTheLocalSenderMarker(t *testing.T) {
	c := liveClaude(t, "s1")
	for _, from := range []string{"local:bot-a", "LOCAL:x", "  local: y"} {
		if _, _, err := c.Send("s1", from, "hi"); err == nil {
			t.Errorf("Send accepted from %q", from)
		}
	}
	if _, _, err := c.Send("s1", "Vega", "hi"); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeSendUnknownAndEndedSessions(t *testing.T) {
	c := liveClaude(t, "s1", "gone")
	c.Hook(hook("SessionEnd", "gone", nil))
	sock := serveSocket(t, c)
	// make the registry "ready" so an unknown id is judged unknown
	c.reg.waitScan(time.Second)
	for _, tc := range []struct{ id, want string }{{"gone", "ended"}, {"nope", "unknown session"}} {
		r := sendNow(sock, "hi", "--session", tc.id, "--from", "bot-a")
		if r.code != 1 || !strings.Contains(r.err, tc.want) {
			t.Errorf("%s: %+v", tc.id, r)
		}
	}
	if inboxLen(c, "gone") != 0 || inboxLen(c, "nope") != 0 {
		t.Fatal("a message was queued for a session that can't take it")
	}
}

func TestClaudeSendInboxLimit(t *testing.T) {
	c := liveClaude(t, "s1")
	sock := serveSocket(t, c)
	for i := 0; i < claudeMaxInbox; i++ {
		if r := sendNow(sock, fmt.Sprint("m", i), "--session", "s1", "--from", "bot-a"); r.code != 0 {
			t.Fatalf("send %d: %+v", i, r)
		}
	}
	if r := sendNow(sock, "one more", "--session", "s1", "--from", "bot-a"); r.code != 1 || !strings.Contains(r.err, "too many messages") {
		t.Fatalf("over the limit: %+v", r)
	}
}

func titledClaude(t *testing.T, titles map[string]string) *Claude {
	t.Helper()
	store := t.TempDir()
	cwd := t.TempDir()
	for id, title := range titles {
		b, _ := json.Marshal(map[string]any{"sessionId": "local_" + id, "cliSessionId": id, "cwd": cwd, "originCwd": cwd,
			"title": title, "lastActivityAt": time.Now().UnixMilli()})
		writeFile(t, filepath.Join(store, "acct", "org", "local_"+id+".json"), string(b))
	}
	return &Claude{DesktopStore: store, Transcripts: t.TempDir()}
}

func TestClaudeSendResolvesATitle(t *testing.T) {
	c := titledClaude(t, map[string]string{"cli-a": "Remote control", "cli-b": "Other work", "cli-c": "Twin", "cli-d": "twin"})
	c.Hook(hook("SessionStart", "cli-a", nil))
	sock := serveSocket(t, c)
	r := sendNow(sock, "hello", "--name", "remote CONTROL", "--from", "bot-a")
	if r.code != 0 || !strings.Contains(r.err, "session cli-a") {
		t.Fatalf("by title: %+v", r)
	}
	if inboxLen(c, "cli-a") != 1 {
		t.Fatal("not queued for the titled session")
	}
	if r := sendNow(sock, "hello", "--name", "Nothing", "--from", "bot-a"); r.code != 1 || !strings.Contains(r.err, "no live session") {
		t.Fatalf("no match: %+v", r)
	}
	r = sendNow(sock, "hello", "--name", "Twin", "--from", "bot-a")
	if r.code != 1 || !strings.Contains(r.err, "2 sessions are called") || !strings.Contains(r.err, "cli-c") || !strings.Contains(r.err, "cli-d") {
		t.Fatalf("ambiguous: %+v", r)
	}
	if inboxLen(c, "cli-c")+inboxLen(c, "cli-d") != 0 {
		t.Fatal("an ambiguous name queued a message")
	}
	// An ended session doesn't count.
	c.Hook(hook("SessionEnd", "cli-a", nil))
	if r := sendNow(sock, "hello", "--name", "Remote control", "--from", "bot-a"); r.code != 1 || !strings.Contains(r.err, "no live session") {
		t.Fatalf("ended: %+v", r)
	}
}

func TestClaudeSendTextRules(t *testing.T) {
	c := liveClaude(t, "s1")
	sock := serveSocket(t, c)
	args := []string{"--session", "s1", "--from", "bot-a"}
	for name, text := range map[string]string{
		"zero width":    "ok​hidden",
		"bidi":          "a‮b",
		"tag char":      "a\U000E0041b",
		"control":       "a\x07b",
		"empty":         "  \n ",
		"too long":      strings.Repeat("x", claudeMaxText+1),
		"too many rows": strings.Repeat("a\n", claudeMaxTaskLines+2),
		"invalid utf8":  "a\xffb",
	} {
		if r := sendNow(sock, text, args...); r.code != 1 {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if r := sendNow(sock, strings.Repeat("é", claudeMaxText), args...); r.code != 0 { // runes, not bytes
		t.Errorf("max length: %+v", r)
	}
	if r := sendNow(sock, "line1\r\nline2", args...); r.code != 0 {
		t.Errorf("CRLF: %+v", r)
	}
	if r := sendNow(sock, "x", "--session", "s1"); r.code != 2 { // no --from
		t.Errorf("no from: %+v", r)
	}
	if r := sendNow(sock, "x", "--session", "s1", "--name", "n", "--from", "a"); r.code != 2 {
		t.Errorf("both targets: %+v", r)
	}
	// The helper checks too: a raw line that skips the client's checks.
	for _, line := range []string{
		`{"hook_event_name":"RubiSend","op":"send","target_session_id":"s1","from":"x","text":"a​b"}`,
		`{"hook_event_name":"RubiSend","op":"send","target_session_id":"s1","from":"x","text":"a\rb"}`,
		`{"hook_event_name":"RubiSend","op":"send","target_session_id":"s1","from":"local:x","text":"ok"}`, // queued; the sender is "local:local:x"
	} {
		conn, _ := net.Dial("unix", sock)
		fmt.Fprintln(conn, line)
		resp, _ := readLine(conn, 1<<16)
		conn.Close()
		if strings.Contains(line, `"from":"x"`) && !strings.Contains(string(resp), "error") {
			t.Errorf("helper accepted %s: %s", line, resp)
		}
	}
	if got := inboxLen(c, "s1"); got < 2 {
		t.Fatalf("inbox: %d", got)
	}
	c.mu.Lock()
	for _, m := range c.inbox["s1"] {
		if strings.ContainsAny(m.Text, "​\r") {
			t.Errorf("hidden text queued: %q", m.Text)
		}
	}
	c.mu.Unlock()
}

func TestClaudeSendSlotsAndDeadline(t *testing.T) {
	c := liveClaude(t, "s1")
	// slots: none free -> an error answer
	full := make(chan struct{}, 1)
	full <- struct{}{}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() { _, _ = c.serveSend(t.Context(), a, []byte(`{"op":"send"}`), full, make(chan struct{}, 1)) }()
	line, err := readLine(b, 1<<16)
	if err != nil || !strings.Contains(string(line), "too many claude-send") {
		t.Fatalf("slots: %s %v", line, err)
	}
	// deadline: the inbox lock is held, so the handler can't finish
	old := claudeSendDeadline
	claudeSendDeadline = 100 * time.Millisecond
	defer func() { claudeSendDeadline = old }()
	sock := serveSocket(t, c)
	c.mu.Lock()
	r := sendNow(sock, "hi", "--session", "s1", "--from", "bot-a")
	c.mu.Unlock()
	if r.code != 1 || !strings.Contains(r.err, "took too long") {
		t.Fatalf("deadline: %+v", r)
	}
}

func TestClaudeSendOwnSlotsDontUseApprovalsOrEvents(t *testing.T) {
	c := liveClaude(t, "s1")
	c.Wait = 20 * time.Second
	sock := serveSocket(t, c)
	before := c.reg.stateOf("s1")
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintln(conn, `{"hook_event_name":"RubiSend","op":"send","target_session_id":"s1","from":"x","text":"hi","session_id":"s1","tool_name":"Bash","tool_input":{"command":"rm -rf /"}}`)
	resp, err := readLine(conn, 1<<16)
	if err != nil || !strings.Contains(string(resp), `"queued":true`) {
		t.Fatalf("send: %s %v", resp, err)
	}
	c.mu.Lock()
	pending := len(c.pending)
	c.mu.Unlock()
	if pending != 0 {
		t.Fatalf("%d approvals pending", pending)
	}
	if got := c.reg.stateOf("s1"); got != before {
		t.Fatalf("a send changed the session state: %q -> %q", before, got)
	}
	// An unknown op is an error, not an approval.
	conn2, _ := net.Dial("unix", sock)
	defer conn2.Close()
	fmt.Fprintln(conn2, `{"hook_event_name":"RubiSend","op":"approve","tool_name":"Bash","tool_input":{"command":"x"}}`)
	if resp, _ := readLine(conn2, 1<<16); !strings.Contains(string(resp), "error") {
		t.Fatalf("unknown op: %s", resp)
	}
}

// An older helper answers {} to a line it doesn't know.
func TestClaudeSendOldHelper(t *testing.T) {
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
			_, _ = conn.Write([]byte("{}\n"))
			conn.Close()
		}
	}()
	for _, args := range [][]string{{"--session", "s1", "--from", "x"}, {"--get-reply", "cm_x"}} {
		r := sendNow(sock, "hi", args...)
		if r.code != 1 || !strings.Contains(r.err, "Rubi Home is too old") {
			t.Fatalf("%v: %+v", args, r)
		}
	}
	if r := sendNow(filepath.Join(shortDir(t), "none.sock"), "hi", "--session", "s1", "--from", "x"); r.code != 1 || !strings.Contains(r.err, "isn't running") {
		t.Fatalf("no helper: %+v", r)
	}
}

func TestLocalReplyBoxTTLAndCap(t *testing.T) {
	c := &Claude{}
	put := func(id string) {
		c.mu.Lock()
		c.pushOutLocked(ClaudeOut{Kind: "reply", To: "local:x", InReplyTo: []string{id}, Text: id})
		c.mu.Unlock()
	}
	for i := 0; i < claudeMaxLocalBox+10; i++ {
		put(fmt.Sprint("cm_", i))
	}
	c.mu.Lock()
	n := len(c.localReplies)
	c.mu.Unlock()
	if n != claudeMaxLocalBox {
		t.Fatalf("box holds %d", n)
	}
	old := claudeLocalReplyTTL
	claudeLocalReplyTTL = 20 * time.Millisecond
	defer func() { claudeLocalReplyTTL = old }()
	time.Sleep(40 * time.Millisecond)
	if _, ok := c.localReplyWait(t.Context(), "cm_59", 0); ok {
		t.Fatal("a reply outlived its TTL")
	}
	if outboxLen(c) != 0 {
		t.Fatal("outbox used")
	}
}

func TestExpiredLocalMessageTellsItsSender(t *testing.T) {
	c := liveClaude(t, "s1")
	m, _, err := c.send("s1", "bot-a", "old", true)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.inbox["s1"][0].At = time.Now().Add(-2 * claudeQueueTTL)
	c.sweepLocked(time.Now().UTC())
	c.mu.Unlock()
	r, ok := c.localReplyWait(t.Context(), m.ID, 0)
	if !ok || r.State != "expired" || outboxLen(c) != 0 {
		t.Fatalf("expired: %+v %v outbox=%d", r, ok, outboxLen(c))
	}
	sock := serveSocket(t, c)
	if res := sendNow(sock, "", "--get-reply", m.ID); res.code != 1 || !strings.Contains(res.err, "expired") {
		t.Fatalf("client: %+v", res)
	}
}

func TestWaitReplyGivesUp(t *testing.T) {
	c := liveClaude(t, "s1")
	sock := serveSocket(t, c)
	start := time.Now()
	r := sendNow(sock, "q", "--session", "s1", "--from", "bot-a", "--wait-reply", "1")
	if r.code != 3 || !strings.Contains(r.err, "--get-reply cm_") || time.Since(start) > 5*time.Second {
		t.Fatalf("wait: %+v after %s", r, time.Since(start))
	}
	if r := sendNow(sock, "q", "--session", "s1", "--from", "bot-a", "--wait-reply", "0"); r.code != 2 {
		t.Fatalf("bad wait: %+v", r)
	}
}

// get_reply long-polls have their own slots: filling them leaves sends working.
func TestReplyPollsDontBlockSends(t *testing.T) {
	c := liveClaude(t, "s1")
	full := make(chan struct{}, 1)
	full <- struct{}{}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		_, _ = c.serveSend(t.Context(), a, []byte(`{"op":"get_reply","message_id":"cm_x"}`), make(chan struct{}, 1), full)
	}()
	if line, err := readLine(b, 1<<16); err != nil || !strings.Contains(string(line), "too many claude-send") {
		t.Fatalf("poll slots: %s %v", line, err)
	}
	sock := serveSocket(t, c)
	if r := sendNow(sock, "hi", "--session", "s1", "--from", "bot-a"); r.code != 0 {
		t.Fatalf("send: %+v", r)
	}
}

// A saved outbox entry for a local sender is dropped on load; RubiSend lines don't count as the hook binary.
func TestLoadQueueDropsLocalEntriesAndSendIsNotTheHookBinary(t *testing.T) {
	c := liveClaude(t, "s1")
	c.mu.Lock()
	c.outbox = []ClaudeOut{{ID: "co_1", Kind: "reply", To: "local:x", Text: "a", At: time.Now().UTC()}, {ID: "co_2", Kind: "reply", To: "Vega", Text: "b", At: time.Now().UTC()}}
	q := c.snapshotLocked()
	c.mu.Unlock()
	b, _ := json.Marshal(q)
	qf := filepath.Join(t.TempDir(), "q.json")
	writeFile(t, qf, string(b))
	d := &Claude{QueueFile: qf}
	d.loadQueue()
	if len(d.outbox) != 1 || d.outbox[0].To != "Vega" {
		t.Fatalf("outbox: %+v", d.outbox)
	}
	sock := serveSocket(t, c)
	if r := sendNow(sock, "hi", "--session", "s1", "--from", "x"); r.code != 0 {
		t.Fatal(r)
	}
	if hk := c.hookInfo(); hk != nil {
		t.Fatalf("a RubiSend line was recorded as a hook binary: %v", hk)
	}
}

func TestMixedFramingSaysEveryone(t *testing.T) {
	m := formatMessages([]ClaudeMessage{{ID: "cm_1", From: "Vega", Text: "a"}, {ID: "cm_2", From: "local:bot-a", Text: "b"}}, "x")
	if strings.Count(m, "every") < 2 {
		t.Fatal(m)
	}
}
