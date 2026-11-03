package t3vm

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

type fakeAsker struct {
	mu    sync.Mutex
	calls []askCall
	res   agent.AskResult
	hold  chan struct{} // when set, AskTool waits for it or the context
}

type askCall struct {
	Project, Role string
	P             agent.AskParams
}

func (f *fakeAsker) AskTool(ctx context.Context, project, role string, a agent.AskParams) agent.AskResult {
	f.mu.Lock()
	f.calls = append(f.calls, askCall{project, role, a})
	f.mu.Unlock()
	if f.hold != nil {
		select {
		case <-f.hold:
		case <-ctx.Done():
			return agent.AskResult{Behavior: "deny", Message: agent.DenyGone}
		}
	}
	return f.res
}

func (f *fakeAsker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func testGate(a Asker) *Gate {
	g := NewGate(a, Target{Project: "proj", Role: "role-1", Cwd: "/w/proj"})
	g.Logf = func(string, ...any) {}
	return g
}

const goodInput = `{"hook_event_name":"PreToolUse","session_id":"sess-1","cwd":"/w/proj","permission_mode":"bypassPermissions",` +
	`"tool_name":"Bash","tool_input":{"command":"ls"},"tool_use_id":"toolu_1"}`

func post(g http.Handler, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	return w
}

type hookOut struct {
	HookSpecificOutput *struct {
		HookEventName            string          `json:"hookEventName"`
		PermissionDecision       string          `json:"permissionDecision"`
		PermissionDecisionReason string          `json:"permissionDecisionReason"`
		UpdatedInput             json.RawMessage `json:"updatedInput"`
	} `json:"hookSpecificOutput"`
}

func decode(t *testing.T, w *httptest.ResponseRecorder) hookOut {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var o hookOut
	if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil {
		t.Fatalf("answer %q: %v", w.Body, err)
	}
	return o
}

func wantDeny(t *testing.T, w *httptest.ResponseRecorder, reason string) {
	t.Helper()
	o := decode(t, w)
	h := o.HookSpecificOutput
	if h == nil || h.HookEventName != "PreToolUse" || h.PermissionDecision != "deny" || !strings.Contains(h.PermissionDecisionReason, reason) {
		t.Fatalf("not a deny with %q: %s", reason, w.Body)
	}
}

func TestGateAsksTheManagersPathWithTheHookInput(t *testing.T) {
	f := &fakeAsker{res: agent.AskResult{Behavior: "allow"}}
	w := post(testGate(f), "/ask", goodInput)
	if o := decode(t, w); o.HookSpecificOutput != nil {
		t.Fatalf("an allow must carry no decision: %s", w.Body)
	}
	if f.count() != 1 {
		t.Fatalf("asked %d times", f.count())
	}
	c := f.calls[0]
	if c.Project != "proj" || c.Role != "role-1" || c.P.SessionID != "sess-1" || c.P.Tool != "Bash" || c.P.ToolUseID != "toolu_1" ||
		c.P.Via != "hook" || c.P.Cwd != "/w/proj" || c.P.Mode != "bypassPermissions" || string(c.P.Input) != `{"command":"ls"}` {
		t.Fatalf("ask: %+v", c)
	}
}

func TestGateDeniesWithTheDecidersReason(t *testing.T) {
	f := &fakeAsker{res: agent.AskResult{Behavior: "deny", Message: "nope, not that"}}
	wantDeny(t, post(testGate(f), "/ask", goodInput), "nope, not that")
	// a deny without a message still says why
	f.res = agent.AskResult{Behavior: "deny"}
	wantDeny(t, post(testGate(f), "/ask", goodInput), "Denied by Rubi")
}

func TestGateDeniesAnythingButAnExplicitAllow(t *testing.T) {
	for _, b := range []string{"", "maybe", "ALLOW", "allowed"} {
		f := &fakeAsker{res: agent.AskResult{Behavior: b}}
		wantDeny(t, post(testGate(f), "/ask", goodInput), "")
	}
}

func TestGateReturnsAChangedInputWithoutADecision(t *testing.T) {
	f := &fakeAsker{res: agent.AskResult{Behavior: "allow", UpdatedInput: json.RawMessage(`{"command":"ls -la"}`)}}
	o := decode(t, post(testGate(f), "/ask", goodInput))
	h := o.HookSpecificOutput
	if h == nil || h.PermissionDecision != "" || string(h.UpdatedInput) != `{"command":"ls -la"}` || h.HookEventName != "PreToolUse" {
		t.Fatalf("%+v", h)
	}
}

func TestGateDeniesMalformedIncompleteAndUnknownInputWithoutAsking(t *testing.T) {
	f := &fakeAsker{res: agent.AskResult{Behavior: "allow"}}
	g := testGate(f)
	for name, body := range map[string]string{
		"empty":            ``,
		"not json":         `{"tool_name":`,
		"array":            `[1,2]`,
		"wrong event":      strings.Replace(goodInput, "PreToolUse", "PostToolUse", 1),
		"no event":         strings.Replace(goodInput, `"hook_event_name":"PreToolUse",`, "", 1),
		"no tool":          strings.Replace(goodInput, `"tool_name":"Bash",`, "", 1),
		"no input":         strings.Replace(goodInput, `"tool_input":{"command":"ls"},`, "", 1),
		"input not object": strings.Replace(goodInput, `{"command":"ls"}`, `"ls"`, 1),
		"input null":       strings.Replace(goodInput, `{"command":"ls"}`, `null`, 1),
		"no session":       strings.Replace(goodInput, `"session_id":"sess-1",`, "", 1),
	} {
		wantDeny(t, post(g, "/ask", body), "Rubi")
		if f.count() != 0 {
			t.Fatalf("%s: the asker was asked", name)
		}
	}
}

func TestGateDeniesWhenItHasNoWayToAsk(t *testing.T) {
	nowhere := NewGate(nil, Target{Project: "proj"})
	nowhere.Logf = func(string, ...any) {}
	wantDeny(t, post(nowhere, "/ask", goodInput), "no approval path")
	noTarget := NewGate(&fakeAsker{res: agent.AskResult{Behavior: "allow"}}, Target{})
	noTarget.Logf = func(string, ...any) {}
	wantDeny(t, post(noTarget, "/ask", goodInput), "no approval path")
}

func TestGateCapsTheBody(t *testing.T) {
	f := &fakeAsker{res: agent.AskResult{Behavior: "allow"}}
	big := `{"hook_event_name":"PreToolUse","session_id":"sess-1","tool_name":"Write","tool_input":{"x":"` + strings.Repeat("a", MaxHookBody) + `"}}`
	wantDeny(t, post(testGate(f), "/ask", big), "too large")
	if f.count() != 0 {
		t.Fatal("an oversized input was asked")
	}
}

func TestGateDeniesWhenTheDecisionTakesTooLong(t *testing.T) {
	f := &fakeAsker{hold: make(chan struct{}), res: agent.AskResult{Behavior: "allow"}}
	g := testGate(f)
	g.Wait = 50 * time.Millisecond
	start := time.Now()
	wantDeny(t, post(g, "/ask", goodInput), "")
	if time.Since(start) > 3*time.Second {
		t.Fatal("the gate waited too long")
	}
}

func TestGateDropsTheAskWhenTheHookGoesAway(t *testing.T) {
	f := &fakeAsker{hold: make(chan struct{}), res: agent.AskResult{Behavior: "allow"}}
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/ask", strings.NewReader(goodInput)).WithContext(ctx)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		testGate(f).ServeHTTP(w, r)
		done <- w
	}()
	for f.count() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case w := <-done:
		wantDeny(t, w, "")
	case <-time.After(3 * time.Second):
		t.Fatal("the gate kept waiting after the request was cancelled")
	}
}

func TestGateLetsAskUserQuestionThroughToT3(t *testing.T) {
	f := &fakeAsker{res: agent.AskResult{Behavior: "deny"}}
	in := strings.Replace(goodInput, `"Bash"`, `"AskUserQuestion"`, 1)
	if o := decode(t, post(testGate(f), "/ask", in)); o.HookSpecificOutput != nil || f.count() != 0 {
		t.Fatalf("AskUserQuestion was gated: %d asks", f.count())
	}
}

func TestGateServesAskOnlyByPost(t *testing.T) {
	g := testGate(&fakeAsker{res: agent.AskResult{Behavior: "allow"}})
	if w := post(g, "/other", goodInput); w.Code != 404 {
		t.Errorf("other path: %d", w.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "/ask", bytes.NewReader(nil))
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	if w.Code != 405 {
		t.Errorf("GET: %d", w.Code)
	}
}

// The gate against the real manager, with the plugin played by a poller that decides.
func TestGateThroughTheManagerAllowsDeniesAndTimesOut(t *testing.T) {
	m := agent.New(agent.Options{Dir: t.TempDir(), Wait: 400 * time.Millisecond, NewVM: func(agent.Project) agent.VM { return &agent.Fake{} }})
	t.Cleanup(func() { m.Close() })
	var decide decider
	decide.set("allow", "")
	ctx, stop := context.WithCancel(context.Background())
	if _, err := m.Next(ctx, 10*time.Millisecond); err != nil { // a poll makes the plugin count as connected
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	asked := make(chan agent.Item, 16)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			items, err := m.Next(ctx, 20*time.Millisecond)
			if err != nil {
				return
			}
			for _, it := range items {
				if it.Type != "perm.ask" {
					continue
				}
				asked <- it
				if b, msg := decide.get(); b != "" {
					_ = m.Decide(it.AskID, b, msg, nil)
				}
			}
		}
	}()
	t.Cleanup(func() { stop(); wg.Wait() })

	g := testGate(m)
	if o := decode(t, post(g, "/ask", goodInput)); o.HookSpecificOutput != nil {
		t.Fatalf("allowed call got a decision: %+v", o.HookSpecificOutput)
	}
	it := <-asked
	if it.Project != "proj" || it.Role != "role-1" || it.Tool != "Bash" || it.Via != "hook" || it.SessionID != "sess-1" || it.Mode != "bypassPermissions" {
		t.Fatalf("item the plugin got: %+v", it)
	}

	decide.set("deny", "not this one")
	wantDeny(t, post(g, "/ask", goodInput), "not this one")

	decide.set("", "") // nobody answers: the manager's wait ends and the call is denied
	wantDeny(t, post(g, "/ask", goodInput), "no answer in time")

}

// Each VM has its own gate: a call that claims another VM's session or cwd is still filed under its own VM's target.
func TestGateFilesEveryAskUnderItsOwnVMsTarget(t *testing.T) {
	fa, fb := &fakeAsker{res: agent.AskResult{Behavior: "allow"}}, &fakeAsker{res: agent.AskResult{Behavior: "allow"}}
	var logged []string
	ga := NewGate(fa, Target{Project: "proj-a", Role: "role-a", Cwd: "/w/a"})
	ga.Logf = func(f string, args ...any) { logged = append(logged, f) }
	gb := NewGate(fb, Target{Project: "proj-b", Role: "role-b", Cwd: "/w/b"})
	gb.Logf = func(string, ...any) {}
	// VM A's code claims the session and the folder of VM B
	claim := strings.Replace(strings.Replace(goodInput, "sess-1", "sess-of-b", 1), "/w/proj", "/w/b", 1)
	post(ga, "/ask", claim)
	if fa.count() != 1 || fb.count() != 0 {
		t.Fatalf("asks: a %d, b %d", fa.count(), fb.count())
	}
	if c := fa.calls[0]; c.Project != "proj-a" || c.Role != "role-a" {
		t.Fatalf("filed under %s/%s", c.Project, c.Role)
	}
	if len(logged) != 1 {
		t.Errorf("a cwd that is not the VM's was not logged: %v", logged)
	}
	post(gb, "/ask", claim)
	if c := fb.calls[0]; c.Project != "proj-b" || c.Role != "role-b" {
		t.Fatalf("filed under %s/%s", c.Project, c.Role)
	}
}

type decider struct {
	mu     sync.Mutex
	b, msg string
}

func (d *decider) set(b, msg string) { d.mu.Lock(); d.b, d.msg = b, msg; d.mu.Unlock() }
func (d *decider) get() (string, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.b, d.msg
}

// With the T3 thread known, asks are filed under it, not under claude's own session id.
func TestGateFilesAsksUnderTheT3Thread(t *testing.T) {
	f := &fakeAsker{res: agent.AskResult{Behavior: "allow"}}
	g := NewGate(f, Target{Project: "proj", Role: "role-1", Cwd: "/w/proj", Thread: "thread-42"})
	g.Logf = func(string, ...any) {}
	post(g, "/ask", goodInput)
	if len(f.calls) != 1 || f.calls[0].P.SessionID != "thread-42" {
		t.Fatalf("calls: %+v", f.calls)
	}
	// Without it, claude's id stays.
	f2 := &fakeAsker{res: agent.AskResult{Behavior: "allow"}}
	post(testGate(f2), "/ask", goodInput)
	if f2.calls[0].P.SessionID != "sess-1" {
		t.Errorf("session %q", f2.calls[0].P.SessionID)
	}
}

// T3_AGENT_THREAD is a claim in the shim's request: only something shaped like an id becomes the thread.
func TestServerGateTakesOnlyAnIdAsTheThread(t *testing.T) {
	s := &Server{opt: Options{Backend: &fakeBackend{}}}
	role := &agent.T3Role{ID: "r_1", Project: "p"}
	for in, want := range map[string]string{"thr-123": "thr-123", "a:b.c_d-e": "a:b.c_d-e", "": "", "bad id": "", "x\ny": "", "../x": "", strings.Repeat("a", 129): ""} {
		g := s.gate(role, "/w", in).(*Gate)
		if g.target.Thread != want {
			t.Errorf("thread %q became %q, want %q", in, g.target.Thread, want)
		}
	}
}

func TestThreadOfPrefersT3sOwnVariable(t *testing.T) {
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"T3CODE_THREAD_ID": "mcp:launch:abc", "T3_AGENT_THREAD": "old"}, "mcp:launch:abc"},
		{map[string]string{"T3_AGENT_THREAD": "old"}, "old"},
		{map[string]string{}, ""},
	} {
		if got := threadOf(c.env); got != c.want {
			t.Errorf("threadOf(%v) = %q", c.env, got)
		}
	}
}
