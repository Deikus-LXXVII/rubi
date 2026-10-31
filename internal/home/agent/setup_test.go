package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSelfTestDeniesItselfAndKeepsItsEvents(t *testing.T) {
	e := newEnv(t)
	e.fake.Setup = func(fr *FakeRunner) {
		fr.Handle = func(method string, params json.RawMessage) (any, error, bool) {
			if method != "session.start" || len(fr.CallsOf("session.start")) > 1 {
				return nil, nil, false
			}
			go selfTestRunner(t, fr)
			return map[string]any{"id": "sess-1"}, nil, true
		}
	}
	pl := startPoller(t, e.m) // a plugin is connected; it must see nothing of the self-test
	st, err := e.m.BeginSelfTest("my-project")
	if err != nil {
		t.Fatal(err)
	}
	defer st.End()
	if _, err := e.m.BeginSelfTest("my-project"); err == nil {
		t.Error("a second self-test (or any session) must not start while one runs")
	}
	if _, err := e.op("start", map[string]any{"project": "my-project", "prompt": "hi"}); err != nil {
		t.Fatal(err) // queued behind the self-test, not delivered
	}
	if e.fake.Starts() != 0 {
		t.Error("a session started during the self-test")
	}
	res, err := st.Turn(e.ctx, "run echo", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Result != "DENIED" || res.IsError || len(res.Denied) != 1 || res.Denied[0] != "Bash" {
		t.Errorf("turn: %+v", res)
	}
	if got := e.fake.Runner().CallsOf("session.close"); len(got) != 1 {
		t.Errorf("the session wasn't closed: %v", got)
	}
	for done := false; !done; { // only the test's own queued start may show
		select {
		case it := <-pl.items:
			if it.Type != "queue" {
				t.Errorf("the plugin saw %+v", it)
			}
		case <-time.After(150 * time.Millisecond):
			done = true
		}
	}
	st.End()
	// The slot is free again: the queued start goes out.
	eventually(t, "queued start delivered", func() bool { return len(e.fake.Runner().CallsOf("session.start")) == 2 })
}

func TestSelfTestDeniesNetAndCredAsksOfItsProject(t *testing.T) {
	e := newEnv(t)
	st, err := e.m.BeginSelfTest("my-project")
	if err != nil {
		t.Fatal(err)
	}
	defer st.End()
	for _, typ := range []string{"net.ask", "cred.ask", "perm.ask"} {
		if r := e.m.waitDecision(e.ctx, "my-project", Item{Type: typ}); r.Behavior != "deny" || r.Message != DenySelfTest {
			t.Errorf("%s: %+v", typ, r)
		}
	}
	if len(e.m.out) != 0 {
		t.Errorf("an ask was queued for the plugin: %v", e.m.out)
	}
	st.End()
	if r := e.m.waitDecision(e.ctx, "my-project", Item{Type: "perm.ask"}); r.Message != DenyLocked {
		t.Errorf("after the self-test the normal path applies: %+v", r)
	}
}

// selfTestRunner plays the runner of the self-test's session: it asks for Bash and ends with a result.
func selfTestRunner(t *testing.T, fr *FakeRunner) {
	r, err := fr.Ask(context.Background(), AskParams{SessionID: "sess-1", Tool: "Bash", Input: json.RawMessage(`{"command":"echo hi"}`)})
	if err != nil || r.Behavior != "deny" || !strings.Contains(r.Message, "self-test") {
		t.Errorf("ask answered %+v %v", r, err)
	}
	fr.Emit(map[string]any{"session_id": "sess-1", "kind": "result", "result": "DENIED", "is_error": false})
}
