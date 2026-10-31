package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type env struct {
	t      *testing.T
	dir    string // helper dir
	proj   string
	fake   *Fake
	m      *Manager
	opt    Options
	cancel context.CancelFunc
	ctx    context.Context
}

func newEnv(t *testing.T, mods ...func(*Options)) *env {
	t.Helper()
	root := t.TempDir()
	e := &env{t: t, dir: filepath.Join(root, "helper"), proj: filepath.Join(root, "my-project"), fake: &Fake{}}
	for _, d := range []string{e.dir, e.proj} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	e.ctx, e.cancel = context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(e.cancel)
	e.opt = Options{Dir: e.dir, Projects: func() []string { return []string{e.proj} },
		GitHosts: func() []string { return []string{"git.example.com"} },
		NewVM:    func(Project) VM { return e.fake }, Wait: 5 * time.Second}
	for _, f := range mods {
		f(&e.opt)
	}
	e.m = New(e.opt)
	t.Cleanup(func() { e.m.Close() })
	return e
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (e *env) op(op string, args any) (map[string]any, error) {
	e.t.Helper()
	b, _ := json.Marshal(args)
	v, err := e.m.Handle(e.ctx, "claude-agent."+op, b)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

func (e *env) busy() bool {
	e.m.mu.Lock()
	defer e.m.mu.Unlock()
	return e.m.slot.busy
}

// poller plays the plugin polling claude-agent.next.
type poller struct {
	items chan Item
	stop  context.CancelFunc
}

func startPoller(t *testing.T, m *Manager) *poller {
	ctx, cancel := context.WithCancel(context.Background())
	p := &poller{items: make(chan Item, 256), stop: cancel}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			items, err := m.Next(ctx, 50*time.Millisecond)
			if err != nil {
				return
			}
			for _, it := range items {
				p.items <- it
			}
		}
	}()
	t.Cleanup(func() { cancel(); wg.Wait() })
	return p
}

func (p *poller) wait(t *testing.T, typ string) Item {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case it := <-p.items:
			if it.Type == typ {
				return it
			}
		case <-timeout:
			t.Fatalf("no %s item", typ)
		}
	}
}

func statusEv(session, status string) map[string]any {
	return map[string]any{"session_id": session, "kind": "status", "status": status}
}

func TestHandshakeBadTokenRefused(t *testing.T) {
	e := newEnv(t)
	e.fake.Setup = func(fr *FakeRunner) { fr.BadToken = true }
	if _, err := e.op("sessions", map[string]any{"project": "my-project"}); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a runner with a wrong token: %v", err)
	}
	fr := e.fake.Runner()
	select {
	case <-fr.Exited():
	case <-time.After(5 * time.Second):
		t.Fatal("the refused runner was not ended")
	}
	if fr.HelloOK() {
		t.Fatal("hello was accepted")
	}
	// The token is per start: a good start gets a different one, and it never contains the old one.
	first := e.fake.Env()["RUBI_RUN_TOKEN"]
	e.fake.Setup = nil
	if _, err := e.op("sessions", map[string]any{"project": "my-project"}); err != nil {
		t.Fatal(err)
	}
	if second := e.fake.Env()["RUBI_RUN_TOKEN"]; len(second) != 64 || second == first {
		t.Fatalf("tokens %q %q", first, second)
	}
}

func TestHandshakeSilentRunnerTimesOut(t *testing.T) {
	old := HelloTimeout
	HelloTimeout = 100 * time.Millisecond
	defer func() { HelloTimeout = old }()
	e := newEnv(t)
	e.fake.Setup = func(fr *FakeRunner) { fr.NoHello = true }
	if _, err := e.op("sessions", map[string]any{"project": "my-project"}); err == nil || !strings.Contains(err.Error(), "hello") {
		t.Fatalf("a silent runner: %v", err)
	}
}

func TestSlotExclusivityQueueOrderAndPersistence(t *testing.T) {
	e := newEnv(t)
	res, err := e.op("start", map[string]any{"project": "my-project", "prompt": "first", "mode": "default"})
	if err != nil || res["id"] != "sess-1" {
		t.Fatalf("start: %v %v", res, err)
	}
	eventually(t, "slot busy", e.busy)
	for i, p := range []string{"second", "third"} {
		res, err := e.op("start", map[string]any{"project": "my-project", "prompt": p})
		if err != nil || res["queued"] != true || int(res["position"].(float64)) != i+1 {
			t.Fatalf("queued start %d: %v %v", i, res, err)
		}
	}
	// A send to another session waits too; a send to the running one goes straight in.
	if res, err := e.op("send", map[string]any{"project": "my-project", "id": "other", "text": "hi"}); err != nil || res["delivered"] != false {
		t.Fatalf("send to another session: %v %v", res, err)
	}
	if res, err := e.op("send", map[string]any{"project": "my-project", "id": "sess-1", "text": "more", "steer": true}); err != nil || res["delivered"] != true {
		t.Fatalf("send to the owner: %v %v", res, err)
	}
	if n := len(e.fake.Runner().CallsOf("session.start")); n != 1 {
		t.Fatalf("%d starts reached the runner while busy", n)
	}
	// The queue file: 0600, three items in order.
	qf := filepath.Join(e.dir, QueueFile)
	st, err := os.Stat(qf)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("queue file: %v %v", st, err)
	}
	var d queueDisk
	b, _ := os.ReadFile(qf)
	if json.Unmarshal(b, &d) != nil || len(d.Items) != 3 || d.Items[0].Text != "second" || d.Items[1].Text != "third" || d.Items[2].Op != "send" {
		t.Fatalf("queue file content: %s", b)
	}
	status, _ := e.op("status", nil)
	if len(status["queue"].([]any)) != 3 || status["slot"].(map[string]any)["busy"] != true {
		t.Fatalf("status: %v", status)
	}

	// Restart: a new Manager on the same directory still has the queue and delivers it in order.
	e.m.Close()
	fake2 := &Fake{}
	e.fake = fake2
	e.m = New(e.opt)
	t.Cleanup(e.m.Close)
	if e.m.q.len() != 3 {
		t.Fatalf("queue after restart: %d", e.m.q.len())
	}
	e.m.Resume()
	eventually(t, "second delivered", func() bool { return fake2.Runner() != nil && len(fake2.Runner().CallsOf("session.start")) == 1 })
	var p struct{ Prompt string }
	_ = json.Unmarshal(fake2.Runner().CallsOf("session.start")[0].Params, &p)
	if p.Prompt != "second" {
		t.Fatalf("first delivered: %q", p.Prompt)
	}
	time.Sleep(50 * time.Millisecond)
	if len(fake2.Runner().CallsOf("session.start")) != 1 || e.m.q.len() != 2 {
		t.Fatal("the next item was delivered while the slot was busy")
	}
	fake2.Runner().Emit(statusEv("sess-1", "idle"))
	eventually(t, "third delivered", func() bool { return len(fake2.Runner().CallsOf("session.start")) == 2 })
	_ = json.Unmarshal(fake2.Runner().CallsOf("session.start")[1].Params, &p)
	if p.Prompt != "third" {
		t.Fatalf("second delivered: %q", p.Prompt)
	}
	fake2.Runner().Emit(statusEv("sess-2", "idle"))
	eventually(t, "send delivered", func() bool { return len(fake2.Runner().CallsOf("session.send")) == 1 })
	eventually(t, "queue empty", func() bool { return e.m.q.len() == 0 })
}

func TestQueueExpiresAndCaps(t *testing.T) {
	var nmu sync.Mutex
	now := time.Now()
	e := newEnv(t, func(o *Options) {
		o.Now = func() time.Time { nmu.Lock(); defer nmu.Unlock(); return now }
	})
	e.m.mu.Lock()
	e.m.slot = slotState{busy: true, project: "my-project", session: "x", turn: true}
	e.m.mu.Unlock()
	if _, err := e.op("start", map[string]any{"project": "my-project", "prompt": "old"}); err != nil {
		t.Fatal(err)
	}
	nmu.Lock()
	now = now.Add(QueueTTL + time.Minute)
	nmu.Unlock()
	if gone := e.m.q.sweep(e.m.opt.Now()); len(gone) != 1 || e.m.q.len() != 0 {
		t.Fatalf("expired: %v", gone)
	}
	for i := 0; i < maxQueueItems; i++ {
		if _, err := e.op("start", map[string]any{"project": "my-project", "prompt": "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.op("start", map[string]any{"project": "my-project", "prompt": "x"}); err == nil {
		t.Fatal("the queue has no cap")
	}
	if _, err := e.op("start", map[string]any{"project": "my-project", "prompt": strings.Repeat("a", maxQueueText+1)}); err == nil {
		t.Fatal("an oversized prompt was accepted")
	}
}

func TestCorruptQueueFileIsMovedAside(t *testing.T) {
	dir := t.TempDir()
	qf := filepath.Join(dir, QueueFile)
	os.WriteFile(qf, []byte("{nope"), 0o600)
	m := New(Options{Dir: dir})
	defer m.Close()
	if m.q.len() != 0 {
		t.Fatal("items from a corrupt file")
	}
	if _, err := os.Stat(qf + ".bad"); err != nil {
		t.Fatal("the corrupt file was not moved aside")
	}
}

func TestBackgroundSubagentKeepsSlot(t *testing.T) {
	e := newEnv(t)
	if _, err := e.op("start", map[string]any{"project": "my-project", "prompt": "go"}); err != nil {
		t.Fatal(err)
	}
	fr := e.fake.Runner()
	fr.Emit(map[string]any{"session_id": "sess-1", "kind": "background", "agent_id": "a1"})
	fr.Emit(statusEv("sess-1", "idle"))
	if res, _ := e.op("start", map[string]any{"project": "my-project", "prompt": "next"}); res["queued"] != true {
		t.Fatalf("a start while a subagent runs: %v", res)
	}
	time.Sleep(50 * time.Millisecond)
	if !e.busy() {
		t.Fatal("the slot was freed with a subagent running")
	}
	fr.Emit(map[string]any{"session_id": "sess-1", "kind": "result", "agent_id": "a1"})
	eventually(t, "queued start delivered", func() bool { return len(fr.CallsOf("session.start")) == 2 })
}

func TestReplayFixtures(t *testing.T) {
	e := newEnv(t)
	pl := startPoller(t, e.m)
	if _, err := e.op("start", map[string]any{"project": "my-project", "prompt": "go"}); err != nil {
		t.Fatal(err)
	}
	fr := e.fake.Runner()
	basic, err := ReadFixture("testdata/session-basic.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- fr.Replay(e.ctx, basic) }()
	ask := pl.wait(t, "perm.ask")
	if ask.Tool != "Bash" || ask.Project != "my-project" || !strings.Contains(string(ask.Input), "go test") || ask.Mode != "bypassPermissions" {
		t.Fatalf("ask item: %+v", ask)
	}
	if err := e.m.Decide(ask.AskID, "deny", "not now", nil); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var r AskResult
	if json.Unmarshal(fr.Replies()[0], &r) != nil || r.Behavior != "deny" || !strings.Contains(r.Message, "not now") {
		t.Fatalf("replayed ask answered %s", fr.Replies()[0])
	}
	eventually(t, "slot freed by idle", func() bool { return !e.busy() })
	st, _ := e.op("status", nil)
	if u := st["usage_today"].(map[string]any); u["input_tokens"].(float64) < 120 || u["output_tokens"].(float64) < 30 {
		t.Fatalf("usage: %v", u)
	}

	// The background transcript: idle with a subagent out keeps the slot; its result frees it.
	bg, _ := ReadFixture("testdata/background.ndjson")
	if err := fr.Replay(e.ctx, bg); err != nil {
		t.Fatal(err)
	}
	eventually(t, "slot busy through background", func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		return e.m.slot.busy && len(e.m.slot.bg) == 1 && !e.m.slot.turn
	})
	done2, _ := ReadFixture("testdata/background-done.ndjson")
	if err := fr.Replay(e.ctx, done2); err != nil {
		t.Fatal(err)
	}
	eventually(t, "slot freed", func() bool { return !e.busy() })
}

func TestPermAskRoundTrip(t *testing.T) {
	e := newEnv(t)
	pl := startPoller(t, e.m)
	if _, err := e.op("sessions", map[string]any{"project": "my-project"}); err != nil {
		t.Fatal(err)
	}
	fr := e.fake.Runner()
	type out struct {
		r   AskResult
		err error
	}
	ask := func(cmd string) chan out {
		ch := make(chan out, 1)
		go func() {
			r, err := fr.Ask(e.ctx, AskParams{AskID: "r1", SessionID: "s1", AgentID: "ag", AgentType: "pusher", Tool: "Bash",
				Input: json.RawMessage(`{"command":"` + cmd + `"}`), Cwd: "/work", Mode: "bypassPermissions"})
			ch <- out{r, err}
		}()
		return ch
	}
	ch := ask("git push")
	it := pl.wait(t, "perm.ask")
	if !strings.HasPrefix(it.AskID, "aa_") || it.AgentType != "pusher" || it.SessionID != "s1" {
		t.Fatalf("item: %+v", it)
	}
	if err := e.m.Decide(it.AskID, "allow", "", json.RawMessage(`{"command":"git status"}`)); err != nil {
		t.Fatal(err)
	}
	o := <-ch
	if o.err != nil || o.r.Behavior != "allow" || string(o.r.UpdatedInput) != `{"command":"git status"}` {
		t.Fatalf("allow: %+v", o)
	}
	if err := e.m.Decide(it.AskID, "allow", "", nil); err == nil {
		t.Fatal("a decision counted twice")
	}
	if err := e.m.Decide("aa_unknown", "allow", "", nil); err == nil {
		t.Fatal("an unknown ask was decided")
	}
	ch = ask("ls")
	it = pl.wait(t, "perm.ask")
	if err := e.m.Decide(it.AskID, "maybe", "", nil); err == nil {
		t.Fatal("bad behavior accepted")
	}
	if err := e.m.Decide(it.AskID, "deny", "no\nway", nil); err != nil {
		t.Fatal(err)
	}
	if o := <-ch; o.r.Behavior != "deny" || o.r.Message != "Rubi: no way" {
		t.Fatalf("deny: %+v", o)
	}
	// Through the op, too.
	ch = ask("pwd")
	it = pl.wait(t, "perm.ask")
	if res, err := e.op("decide", map[string]any{"ask_id": it.AskID, "behavior": "allow"}); err != nil || res["ok"] != true {
		t.Fatalf("decide op: %v %v", res, err)
	}
	if o := <-ch; o.r.Behavior != "allow" {
		t.Fatalf("op allow: %+v", o)
	}
}

func TestPermAskTimeoutDenies(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Wait = 200 * time.Millisecond })
	pl := startPoller(t, e.m)
	e.op("sessions", map[string]any{"project": "my-project"})
	r, err := e.fake.Runner().Ask(e.ctx, AskParams{SessionID: "s1", Tool: "Bash", Input: json.RawMessage(`{}`)})
	if err != nil || r.Behavior != "deny" || r.Message != DenyNoAnswer {
		t.Fatalf("%+v %v", r, err)
	}
	it := pl.wait(t, "perm.ask")
	if err := e.m.Decide(it.AskID, "allow", "", nil); err == nil {
		t.Fatal("a late allow was accepted")
	}
}

func TestPermAskLockedDenies(t *testing.T) {
	e := newEnv(t)
	e.op("sessions", map[string]any{"project": "my-project"})
	// Never polled: denied at once.
	r, err := e.fake.Runner().Ask(e.ctx, AskParams{SessionID: "s1", Tool: "Bash", Input: json.RawMessage(`{}`)})
	if err != nil || r.Behavior != "deny" || r.Message != DenyLocked {
		t.Fatalf("never polled: %+v %v", r, err)
	}
	if len(e.m.out) != 0 && e.m.out[len(e.m.out)-1].Type == "perm.ask" {
		t.Fatal("a denied ask was left in the outbox")
	}
	// Polled once, then silence: denied when the grace is over.
	old := lockedAfter
	lockedAfter = 100 * time.Millisecond
	defer func() { lockedAfter = old }()
	e.m.Next(e.ctx, 10*time.Millisecond)
	start := time.Now()
	r, err = e.fake.Runner().Ask(e.ctx, AskParams{SessionID: "s1", Tool: "Bash", Input: json.RawMessage(`{}`)})
	if err != nil || r.Behavior != "deny" {
		t.Fatalf("locked: %+v %v", r, err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("not denied promptly")
	}
}

func TestRunnerEndDeniesPendingAndFreesSlot(t *testing.T) {
	e := newEnv(t)
	pl := startPoller(t, e.m)
	e.op("start", map[string]any{"project": "my-project", "prompt": "x"})
	fr := e.fake.Runner()
	ch := make(chan AskResult, 1)
	go func() {
		r, _ := fr.Ask(e.ctx, AskParams{SessionID: "s", Tool: "Bash", Input: json.RawMessage(`{}`)})
		ch <- r
	}()
	pl.wait(t, "perm.ask")
	fr.Exit()
	eventually(t, "slot freed", func() bool { return !e.busy() })
	e.m.mu.Lock()
	n := len(e.m.asks)
	e.m.mu.Unlock()
	eventually(t, "ask dropped", func() bool {
		e.m.mu.Lock()
		defer e.m.mu.Unlock()
		return len(e.m.asks) == 0
	})
	_ = n
}

type fakeCreds struct{}

func (fakeCreds) Read(h string) (Cred, bool) {
	if h == "git.example.com" {
		return Cred{"alice", "read-token"}, true
	}
	return Cred{}, false
}
func (fakeCreds) Push(h string) (Cred, bool) {
	if h == "git.example.com" {
		return Cred{"alice", "push-token"}, true
	}
	return Cred{}, false
}

func TestGitCredentialReadVersusPush(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.Cred = fakeCreds{} })
	e.op("sessions", map[string]any{"project": "my-project"})
	fr := e.fake.Runner()
	in := "protocol=https\nhost=git.example.com\npath=alice/repo.git\n\n"
	// Read: served, no approval asked, even with Rubi absent.
	r, err := fr.Credential(e.ctx, CredParams{Op: "get", Input: in})
	if err != nil || !strings.Contains(r.Output, "password=read-token") || strings.Contains(r.Output, "push-token") {
		t.Fatalf("read: %+v %v", r, err)
	}
	// store and erase are accepted and do nothing.
	for _, op := range []string{"store", "erase"} {
		if r, err := fr.Credential(e.ctx, CredParams{Op: op, Input: in + "username=alice\npassword=x\n"}); err != nil || r.Output != "" {
			t.Fatalf("%s: %+v %v", op, r, err)
		}
	}
	// A host that isn't configured, a non-https protocol, and bad input are refused.
	for _, bad := range []string{"protocol=https\nhost=evil.example.org\n\n", "protocol=http\nhost=git.example.com\n\n", "garbage\n\n"} {
		if _, err := fr.Credential(e.ctx, CredParams{Op: "get", Input: bad}); err == nil {
			t.Fatalf("served %q", bad)
		}
	}
	// Push with Rubi absent: refused.
	if _, err := fr.Credential(e.ctx, CredParams{Op: "get", Input: in, Purpose: "push"}); err == nil || strings.Contains(err.Error(), "push-token") {
		t.Fatalf("push while locked: %v", err)
	}
	pl := startPoller(t, e.m)
	push := func() (CredResult, error) {
		return fr.Credential(e.ctx, CredParams{Op: "get", Input: in, Purpose: "push"})
	}
	type out struct {
		r   CredResult
		err error
	}
	ch := make(chan out, 1)
	go func() { r, err := push(); ch <- out{r, err} }()
	it := pl.wait(t, "cred.ask")
	if it.Host != "git.example.com" || it.Op != "push" || it.Path != "alice/repo.git" || it.Project != "my-project" {
		t.Fatalf("cred.ask item: %+v", it)
	}
	e.m.Decide(it.AskID, "deny", "no mandate", nil)
	if o := <-ch; o.err == nil || !strings.Contains(o.err.Error(), "no mandate") || strings.Contains(o.r.Output, "push-token") {
		t.Fatalf("denied push: %+v", o)
	}
	go func() { r, err := push(); ch <- out{r, err} }()
	it = pl.wait(t, "cred.ask")
	e.m.Decide(it.AskID, "allow", "", nil)
	if o := <-ch; o.err != nil || !strings.Contains(o.r.Output, "password=push-token") {
		t.Fatalf("approved push: %+v", o)
	}
}

func TestFileCredSource(t *testing.T) {
	p := filepath.Join(t.TempDir(), "creds.json")
	os.WriteFile(p, []byte(`{"git.example.com":{"read":{"username":"alice","password":"r"}}}`), 0o644)
	s := FileCredSource{Path: p}
	if _, ok := s.Read("git.example.com"); ok {
		t.Fatal("a world-readable credential file was used")
	}
	os.Chmod(p, 0o600)
	if c, ok := s.Read("git.example.com"); !ok || c.Password != "r" {
		t.Fatal("read credential missing")
	}
	if _, ok := s.Push("git.example.com"); ok {
		t.Fatal("a push credential from nowhere")
	}
	if _, ok := s.Read("other.example.com"); ok {
		t.Fatal("another host")
	}
}

func TestProjectsValidation(t *testing.T) {
	e := newEnv(t)
	root := filepath.Dir(e.dir)
	file := filepath.Join(root, "afile")
	os.WriteFile(file, nil, 0o600)
	other := filepath.Join(root, "other", "my-project")
	os.MkdirAll(other, 0o700)
	e.m.opt.Projects = func() []string {
		return []string{e.proj, "relative/dir", file, root /* contains the helper dir */, e.dir, "/", other, e.proj}
	}
	ps := e.m.projects()
	if len(ps) != 2 || ps[0].ID != "my-project" || ps[1].ID != "my-project-2" {
		t.Fatalf("projects: %+v", ps)
	}
	res, err := e.op("projects", nil)
	if err != nil || len(res["projects"].([]any)) != 2 {
		t.Fatalf("%v %v", res, err)
	}
	if _, err := e.op("sessions", map[string]any{"project": "nope"}); err == nil {
		t.Fatal("unknown project accepted")
	}
}

func TestOpsForwardAndValidate(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		op   string
		args map[string]any
		want string
	}{
		{"sessions", map[string]any{}, "session.list"},
		{"messages", map[string]any{"id": "s1", "limit": 5}, "session.messages"},
		{"rename", map[string]any{"id": "s1", "name": "Docs"}, "session.rename"},
		{"fork", map[string]any{"id": "s1"}, "session.fork"},
		{"interrupt", map[string]any{"id": "s1"}, "session.interrupt"},
		{"mode", map[string]any{"id": "s1", "mode": "plan"}, "session.mode"},
		{"close", map[string]any{"id": "s1"}, "session.close"},
	} {
		c.args["project"] = "my-project"
		if _, err := e.op(c.op, c.args); err != nil {
			t.Fatalf("%s: %v", c.op, err)
		}
		if len(e.fake.Runner().CallsOf(c.want)) != 1 {
			t.Fatalf("%s didn't reach %s", c.op, c.want)
		}
	}
	for _, bad := range []struct {
		op   string
		args map[string]any
	}{
		{"mode", map[string]any{"id": "s1", "mode": "yolo"}},
		{"mode", map[string]any{"id": "s1"}},
		{"send", map[string]any{"id": "../x", "text": "t"}},
		{"start", map[string]any{"prompt": "  "}},
		{"start", map[string]any{"prompt": "x", "mode": "yolo"}},
		{"messages", map[string]any{"id": "a b"}},
		{"nonsense", map[string]any{}},
	} {
		bad.args["project"] = "my-project"
		if _, err := e.op(bad.op, bad.args); err == nil {
			t.Fatalf("%s %v was accepted", bad.op, bad.args)
		}
	}
}

func TestEnvAndProxyGivenToRunner(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.ProxyVMHost = "gateway.example.com" })
	e.op("sessions", map[string]any{"project": "my-project"})
	env := e.fake.Env()
	if !strings.HasPrefix(env["HTTPS_PROXY"], "http://gateway.example.com:") || len(env["RUBI_RUN_TOKEN"]) != 64 {
		t.Fatalf("env: %v", env)
	}
}
