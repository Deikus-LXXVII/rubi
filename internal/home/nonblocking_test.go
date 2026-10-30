package home

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// hookEvents are every event the helper handles, each for one session.
func hookEvents() []string {
	var out []string
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "Stop", "PreToolUse", "SessionEnd"} {
		out = append(out, `{"hook_event_name":"`+ev+`","session_id":"s-1","cwd":"/work","transcript_path":"/x/t.jsonl"}`)
	}
	return out
}

// A scan stuck while it holds the registry's scan lock must not hold up any hook.
func TestHooksDontWaitForTheScanLock(t *testing.T) {
	c := &Claude{DesktopStore: t.TempDir(), Transcripts: t.TempDir()}
	c.reg.mu.Lock()
	defer c.reg.mu.Unlock()
	for _, raw := range hookEvents() {
		start := time.Now()
		c.Hook([]byte(raw))
		if took := time.Since(start); took > time.Second {
			t.Fatalf("%s took %s", hookEvent([]byte(raw)), took)
		}
	}
}

// With the desktop store's read blocked for good, hooks still answer and an approval still reaches Rubi.
func TestStuckStore(t *testing.T) {
	c := &Claude{DesktopStore: t.TempDir(), Wait: 20 * time.Second}
	c.reg.readStore = stuckStore(t)
	go c.Sessions("", false) // starts the scan that never ends
	waitFor(t, "the scan to start", func() bool {
		c.reg.mu.Lock()
		defer c.reg.mu.Unlock()
		return c.reg.scanning
	})

	for _, raw := range hookEvents() {
		start := time.Now()
		c.Hook([]byte(raw))
		if took := time.Since(start); took > time.Second {
			t.Fatalf("%s took %s", hookEvent([]byte(raw)), took)
		}
	}
	sock := serveSocket(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reqs := poller(ctx, c)
	time.Sleep(20 * time.Millisecond)
	hook := runHook(t, hookJSON, sock, 30*time.Second)
	select {
	case r := <-reqs:
		c.mu.Lock()
		_, pending := c.pending[r.ID]
		c.mu.Unlock()
		if !pending {
			t.Fatal("the request isn't pending")
		}
		if err := c.Decide(answer(r, "allow", "")); err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the approval didn't reach Rubi")
	}
	if d := <-hook; d["behavior"] != "allow" {
		t.Fatalf("hook: %v", d)
	}
	// An event through the socket answers too.
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte(hookEvents()[0] + "\n"))
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := readLine(conn, 64<<10); err != nil {
		t.Fatalf("event over the socket: %v", err)
	}
}

// waitFor polls cond for up to 5 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// settled is the goroutine count once it stops changing.
func settled() int {
	n := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		time.Sleep(25 * time.Millisecond)
		m := runtime.NumGoroutine()
		if m == n {
			return n
		}
		n = m
	}
	return n
}

// Calls of claude-sessions.list that time out don't pile up goroutines: they share one in-flight call.
func TestTimedOutListsDontLeakGoroutines(t *testing.T) {
	old := sessionsListTimeout
	sessionsListTimeout = 5 * time.Millisecond
	defer func() { sessionsListTimeout = old }()

	t.Run("registry lock held", func(t *testing.T) {
		c := &Claude{}
		c.reg.mu.Lock() // a list can't get past the scan lock
		defer c.reg.mu.Unlock()
		before := settled()
		for i := 0; i < 100; i++ {
			if _, err := c.handle(context.Background(), "claude-sessions.list", nil); err == nil {
				t.Fatal("no error")
			}
		}
		if after := settled(); after > before+1 {
			t.Fatalf("goroutines %d -> %d", before, after)
		}
	})
	t.Run("store read blocked", func(t *testing.T) {
		c := &Claude{DesktopStore: t.TempDir()}
		c.reg.readStore, c.reg.firstScanWait = stuckStore(t), 50*time.Millisecond
		c.reg.scanBudget = 20 * time.Millisecond // scans end quickly and reuse the one stuck read
		before := settled()
		for i := 0; i < 100; i++ {
			// Most time out; one that joins the flight just before its end may get the hook-only list.
			_, _ = c.handle(context.Background(), "claude-sessions.list", nil)
		}
		// The one blocked store read is all that is left (scans and list calls all end).
		time.Sleep(200 * time.Millisecond)
		if after := settled(); after > before+1 {
			t.Fatalf("goroutines %d -> %d", before, after)
		}
	})
}

// Event hooks that fill their slots (here: stuck on the live-sessions lock) don't make an approval be denied.
func TestFullEventSlots(t *testing.T) {
	c := &Claude{Wait: 20 * time.Second}
	sock := serveSocket(t, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reqs := poller(ctx, c)

	c.reg.liveMu.Lock() // every SessionStart now waits
	unlocked := false
	unlock := func() {
		if !unlocked {
			unlocked = true
			c.reg.liveMu.Unlock()
		}
	}
	defer unlock()
	var conns []net.Conn
	defer func() {
		for _, cn := range conns {
			cn.Close()
		}
	}()
	// Send events until one is answered empty at once: that is when every slot is taken. Each one before
	// that waits for the lock, so exactly claudeMaxEvents of them hold a slot.
	held := 0
	for i := 0; ; i++ {
		if i > claudeMaxEvents+2 {
			t.Fatalf("still no empty reply after %d events", i)
		}
		cn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, cn)
		_, _ = fmt.Fprintf(cn, `{"hook_event_name":"SessionStart","session_id":"s-%d"}`+"\n", i)
		_ = cn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		line, err := readLine(cn, 64<<10)
		if err != nil {
			held++ // no answer: it is waiting for the lock, in a slot
			continue
		}
		var reply map[string]any
		if json.Unmarshal(line, &reply) != nil || len(reply) != 0 {
			t.Fatalf("event with full slots got %s", line)
		}
		break
	}
	if held != claudeMaxEvents {
		t.Fatalf("%d events held a slot, want %d", held, claudeMaxEvents)
	}

	hook := runHook(t, hookJSON, sock, 30*time.Second)
	select {
	case r := <-reqs:
		if err := c.Decide(answer(r, "allow", "")); err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the approval didn't reach Rubi while event slots were full")
	}
	if d := <-hook; d["behavior"] != "allow" {
		t.Fatalf("hook: %v", d)
	}
	unlock()
}

// fifos makes n named pipes named *.jsonl in dir; opening one for reading blocks until someone opens it
// for writing (a stand-in for a hung disk). The cleanup lets the blocked reads end.
func fifos(t *testing.T, dir string, n int) []string {
	t.Helper()
	var out []string
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%d.jsonl", i))
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Skip("no named pipes here:", err)
		}
		out = append(out, p)
	}
	t.Cleanup(func() {
		for _, p := range out {
			// A writer that opens without blocking succeeds only while a reader is waiting in open(2).
			if f, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				f.Close()
			}
		}
	})
	return out
}

// Stop goes on without the answer when the transcript can't be read in time, reads at most
// claudeAnswerReads at a time, and keeps the reply it owes for a later Stop.
func TestStopDoesNotWaitForAStuckTranscript(t *testing.T) {
	old := claudeAnswerWait
	claudeAnswerWait = 50 * time.Millisecond
	defer func() { claudeAnswerWait = old }()
	dir := t.TempDir()
	c := &Claude{Transcripts: dir}
	paths := fifos(t, dir, claudeAnswerReads+2)
	c.mu.Lock()
	c.turnLocked("s-1").from = []string{"sister"} // the turn carried a message from a sister
	c.mu.Unlock()
	for i, p := range paths {
		start := time.Now()
		r := c.Hook([]byte(`{"hook_event_name":"Stop","session_id":"s-1","transcript_path":"` + p + `"}`))
		if r.Block || time.Since(start) > time.Second {
			t.Fatalf("stop %d: %+v after %s", i, r, time.Since(start))
		}
	}
	if n := len(c.answerSlots); n != claudeAnswerReads { // the 2 extra Stops didn't start a read
		t.Fatalf("%d reads running, want %d", n, claudeAnswerReads)
	}
	c.mu.Lock()
	owed := append([]string(nil), c.turns["s-1"].from...)
	queued := 0
	for _, o := range c.outbox {
		if o.Kind != "status" {
			queued++
		}
	}
	c.mu.Unlock()
	if len(owed) != 1 || queued != 0 {
		t.Fatalf("the owed reply was dropped or sent without an answer: from %v, outbox %d", owed, queued)
	}
	// The next Stop has the answer: the reply goes out.
	c.Hook([]byte(`{"hook_event_name":"Stop","session_id":"s-1","last_assistant_message":"done: all green"}`))
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.outbox) != 1 || c.outbox[0].Kind != "reply" || c.outbox[0].To != "sister" || c.outbox[0].Text != "done: all green" {
		t.Fatalf("outbox %+v", c.outbox)
	}
}

// An event handler that is late (here: held up by c.mu) and was answered empty must not take messages out
// of the inbox afterwards: they stay for the next prompt or stop.
func TestLateHookLeavesMessages(t *testing.T) {
	old := claudeEventDeadline
	claudeEventDeadline = 50 * time.Millisecond
	defer func() { claudeEventDeadline = old }()
	for _, ev := range []string{"UserPromptSubmit", "Stop"} {
		t.Run(ev, func(t *testing.T) {
			c := &Claude{}
			c.inbox = map[string][]ClaudeMessage{"s-1": {{ID: "cm_1", From: "sister", Text: "hello", At: time.Now()}}}
			slots := make(chan struct{}, 1)
			line := []byte(`{"hook_event_name":"` + ev + `","session_id":"s-1","last_assistant_message":"ok"}`)
			c.mu.Lock()
			reply, _ := c.event(line, slots) // the handler waits for c.mu; the deadline passes
			c.mu.Unlock()
			if reply.Context != "" || reply.Block {
				t.Fatalf("reply %+v", reply)
			}
			waitFor(t, "the late handler to end", func() bool { return len(slots) == 0 })
			c.mu.Lock()
			left := len(c.inbox["s-1"])
			c.mu.Unlock()
			if left != 1 {
				t.Fatalf("%d messages left, want 1", left)
			}
			r := c.Hook(line) // the next one gets them
			if ev == "Stop" && !r.Block || ev == "UserPromptSubmit" && !strings.Contains(r.Context, "hello") {
				t.Fatalf("next reply %+v", r)
			}
		})
	}
}

// A handler that has already taken the messages is waited for: its reply is the answer.
func TestEventTicketStates(t *testing.T) {
	old := claudeEventDeadline
	claudeEventDeadline = 50 * time.Millisecond
	defer func() { claudeEventDeadline = old }()
	tk := &eventTicket{}
	if !tk.commit() || tk.abandon() || !tk.commit() {
		t.Fatal("a committed ticket must stay committed and can't be abandoned")
	}
	tk = &eventTicket{}
	if !tk.abandon() || tk.commit() {
		t.Fatal("an abandoned ticket must not commit")
	}
}

// More connections than the read gate allows get a denial that says why; an event hook reads the same
// line as an empty reply.
func TestReadGate(t *testing.T) {
	c := &Claude{}
	sock := serveSocket(t, c)
	var conns []net.Conn
	defer func() {
		for _, cn := range conns {
			cn.Close()
		}
	}()
	for i := 0; i < claudeMaxReading; i++ { // clients that connect and say nothing
		cn, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, cn)
	}
	var line []byte
	var err error
	for try := 0; try < 50; try++ { // the server may not have accepted all of them yet
		cn, derr := net.Dial("unix", sock)
		if derr != nil {
			t.Fatal(derr)
		}
		conns = append(conns, cn)
		_ = cn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if line, err = readLine(cn, 64<<10); err == nil {
			break
		}
	}
	var d ClaudeDecision
	if err != nil || json.Unmarshal(line, &d) != nil || d.Behavior != "deny" || !strings.Contains(d.Message, "too many") {
		t.Fatalf("overflow got %q (%v)", line, err)
	}
	var r HookReply
	if json.Unmarshal(line, &r) != nil || r != (HookReply{}) {
		t.Fatalf("as an event reply: %+v", r)
	}
}

// A desktop store whose read never returns doesn't hide the transcript sessions, and later scans reuse
// the stuck read instead of starting another.
func TestStuckStoreDoesNotHideTranscripts(t *testing.T) {
	tr := filepath.Join(t.TempDir(), "projects")
	writeFile(t, filepath.Join(tr, "p", "s-tx.jsonl"), `{"cwd":"/x"}`+"\n")
	var storeReads atomic.Int32
	block := make(chan struct{})
	defer close(block)
	r := &sessionRegistry{firstScanWait: 2 * time.Second, scanBudget: 300 * time.Millisecond, readGit: func(string) gitInfo { return gitInfo{} }}
	r.readStore = func(string, time.Time) []ClaudeSession {
		storeReads.Add(1)
		<-block
		return nil
	}
	for round := 0; round < 3; round++ {
		now := time.Now().Add(time.Duration(round) * time.Minute) // each round's cache is old
		start := time.Now()
		list := r.list("store", tr, now)
		if round > 0 { // the round's scan runs in the background; the next list gets it
			waitFor(t, "the scan to end", func() bool {
				r.mu.Lock()
				defer r.mu.Unlock()
				return !r.scanning
			})
			list = r.list("store", tr, now)
		}
		if len(list) != 1 || list[0].SessionID != "s-tx" || time.Since(start) > time.Second {
			t.Fatalf("round %d: %+v after %s", round, list, time.Since(start))
		}
		if n := storeReads.Load(); n != 1 {
			t.Fatalf("round %d: the store was read %d times", round, n)
		}
	}
}

// claude-sessions.send answers within sendScanWait while the first scan is stuck: a session the hooks reported takes
// the message, any other id gets "not ready" (and "unknown session" once a scan has finished).
func TestSendDoesNotWaitForTheScan(t *testing.T) {
	c := &Claude{DesktopStore: t.TempDir()}
	block := make(chan struct{})
	c.reg.readStore = func(string, time.Time) []ClaudeSession {
		<-block
		return []ClaudeSession{{SessionID: "s-desk", Source: "desktop"}}
	}
	c.Hook([]byte(`{"hook_event_name":"SessionStart","session_id":"s-hook","cwd":"/w"}`))
	start := time.Now()
	if _, _, err := c.Send("s-hook", "sister", "hi"); err != nil {
		t.Fatalf("hook-reported session: %v", err)
	}
	if _, _, err := c.Send("s-desk", "sister", "hi"); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("before the first scan: %v", err)
	}
	if took := time.Since(start); took > sendScanWait+500*time.Millisecond {
		t.Fatalf("send took %s", took)
	}
	close(block)
	waitFor(t, "the scan to end", func() bool {
		c.reg.mu.Lock()
		defer c.reg.mu.Unlock()
		return c.reg.hasScan
	})
	if _, _, err := c.Send("s-desk", "sister", "hi"); err != nil {
		t.Fatalf("after the scan: %v", err)
	}
	if _, _, err := c.Send("nope", "sister", "hi"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown id after the scan: %v", err)
	}
}

// A list served from an old scan says so in sources, with the time of that scan.
func TestListSaysWhenItIsStale(t *testing.T) {
	c := &Claude{DesktopStore: t.TempDir()}
	c.reg.readStore = stuckStore(t)
	at := time.Now().Add(-time.Hour).UTC()
	c.reg.scanned, c.reg.scannedAt, c.reg.hasScan = []ClaudeSession{}, at, true
	v, err := c.handle(context.Background(), "claude-sessions.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	src := v.(map[string]any)["sources"].(map[string]any)
	if src["stale"] != true || src["scanned_at"] != at {
		t.Fatalf("sources %v", src)
	}
	c = &Claude{}
	c.reg.scanned, c.reg.scannedAt, c.reg.hasScan = []ClaudeSession{}, time.Now().UTC(), true
	v, _ = c.handle(context.Background(), "claude-sessions.list", nil)
	if src := v.(map[string]any)["sources"].(map[string]any); src["stale"] != nil || src["scanned_at"] == nil {
		t.Fatalf("fresh sources %v", src)
	}
}

// A panic in a read ends the scan cleanly, and one in a flight ends the flight.
func TestPanicsDontWedge(t *testing.T) {
	r := &sessionRegistry{firstScanWait: time.Second}
	r.readStore = func(string, time.Time) []ClaudeSession { panic("boom") }
	r.touch("hooked", "/w", "", "", "idle", time.Now())
	if list := r.list("x", "", time.Now()); len(list) != 1 {
		t.Fatalf("list %+v", list)
	}
	r.mu.Lock()
	scanning := r.scanning
	r.mu.Unlock()
	if scanning {
		t.Fatal("the scan is still marked running")
	}

	c := &Claude{}
	if _, err := c.within(context.Background(), "k", time.Second, func() (any, error) { panic("boom") }); err == nil {
		t.Fatal("no error from a panicking call")
	}
	c.flightMu.Lock()
	left := len(c.flights)
	c.flightMu.Unlock()
	if left != 0 {
		t.Fatalf("%d flights left", left)
	}
	if v, err := c.within(context.Background(), "k", time.Second, func() (any, error) { return 1, nil }); err != nil || v != 1 {
		t.Fatalf("next call: %v %v", v, err)
	}
}

// Lists with different repo filters share one flight, so they can't use up the flights.
func TestListFiltersShareOneFlight(t *testing.T) {
	old := sessionsListTimeout
	sessionsListTimeout = 20 * time.Millisecond
	defer func() { sessionsListTimeout = old }()
	c := &Claude{DesktopStore: t.TempDir()}
	c.reg.readStore, c.reg.firstScanWait = stuckStore(t), time.Second
	for i := 0; i < 30; i++ {
		args := json.RawMessage(fmt.Sprintf(`{"repo":"r%d"}`, i))
		_, err := c.handle(context.Background(), "claude-sessions.list", args)
		if err == nil || !strings.Contains(err.Error(), "too long") {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	c.flightMu.Lock()
	n := len(c.flights)
	c.flightMu.Unlock()
	if n != 1 {
		t.Fatalf("%d flights", n)
	}
}

// A store that doesn't come in time is listed as its last full read, and the scan counts as partial: the
// list says stale and an id it doesn't find isn't called unknown.
func TestPartialScanKeepsTheLastFullRead(t *testing.T) {
	var reads atomic.Int32
	block := make(chan struct{})
	defer close(block)
	r := &sessionRegistry{firstScanWait: 2 * time.Second, scanBudget: 100 * time.Millisecond}
	r.readStore = func(string, time.Time) []ClaudeSession {
		if reads.Add(1) > 1 {
			<-block // the disk hangs from the second read on
		}
		return []ClaudeSession{{SessionID: "s-desk", Source: "desktop"}}
	}
	if list, meta := r.listMeta("store", "", time.Now()); len(list) != 1 || meta.stale {
		t.Fatalf("first scan: %+v %+v", list, meta)
	}
	later := time.Now().Add(time.Minute)
	r.list("store", "", later) // starts the second scan, whose store read hangs
	waitFor(t, "the second scan to end", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return !r.scanning && r.scannedAt.Equal(later)
	})
	list, meta := r.listMeta("store", "", later)
	if len(list) != 1 || list[0].SessionID != "s-desk" || !meta.stale {
		t.Fatalf("partial scan: %+v %+v", list, meta)
	}
	if found, _, ready := r.lookup("store", "", "s-other", later); found || ready {
		t.Fatalf("lookup of an id a partial scan doesn't have: found %v ready %v", found, ready)
	}
}
