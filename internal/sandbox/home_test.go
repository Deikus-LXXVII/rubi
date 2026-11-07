package sandbox

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/setup"
	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
)

func TestHomeLoginWaitsAndThenReturnsTheFullLink(t *testing.T) {
	h := NewHome("Sandbox Home", "secret", "https://127.0.0.1:18443/oauth/authorize?state="+strings.Repeat("a", 480))
	h.signInFor = 40 * time.Millisecond
	h.selfTestFor = 40 * time.Millisecond
	hello, err := h.Handle(homeproto.Envelope{Op: "pair", Args: mustJSON(t, map[string]string{"secret": "secret", "token": "tok"})})
	if err != nil {
		t.Fatal(err)
	}
	can, _ := hello.(map[string]any)["can"].([]string)
	if len(can) != 1 || can[0] != "t3-setup" {
		t.Fatalf("can: %#v", hello)
	}
	st, err := h.Handle(homeproto.Envelope{Op: "t3-setup.status", Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	login := step(t, st, "login")
	if !strings.Contains(login["reason"].(string), "Waiting for earlier steps") || login["action"] != "" {
		t.Fatalf("login before earlier steps: %#v", login)
	}
	self := step(t, st, "selftest")
	if !strings.Contains(self["reason"].(string), "Waiting for earlier steps") || self["action"] != "" {
		t.Fatalf("self-test before earlier steps: %#v", self)
	}
	for _, id := range []string{"service", "image", "t3", "pair"} {
		if _, err := h.Handle(homeproto.Envelope{Op: "t3-setup.run", Token: "tok", Args: mustJSON(t, map[string]string{"step": id})}); err != nil {
			t.Fatal(id, err)
		}
	}
	if _, err := h.Handle(homeproto.Envelope{Op: "t3-setup.run", Token: "tok", Args: mustJSON(t, map[string]string{"step": "selftest"})}); err == nil {
		t.Fatal("self-test ran before sign-in")
	}
	if _, err := h.Handle(homeproto.Envelope{Op: "t3-setup.run", Token: "tok", Args: mustJSON(t, map[string]string{"step": "login", "method": "claudeai"})}); err != nil {
		t.Fatal(err)
	}
	opened, err := h.Handle(homeproto.Envelope{Op: "t3-setup.login.open", Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	got := opened.(map[string]any)
	if got["opened"] != false || got["url"] != h.SignInURL || len(h.SignInURL) < 500 {
		t.Fatalf("open: %#v len %d", got, len(h.SignInURL))
	}
	if _, err := h.Handle(homeproto.Envelope{Op: "t3-setup.login.code", Token: "tok", Args: mustJSON(t, map[string]string{"code": "has space"})}); err == nil {
		t.Fatal("a code with a space was accepted")
	}
	if _, err := h.Handle(homeproto.Envelope{Op: "t3-setup.login.code", Token: "tok", Args: mustJSON(t, map[string]string{"code": SignInCode})}); err != nil {
		t.Fatal(err)
	}
	st, _ = h.Handle(homeproto.Envelope{Op: "t3-setup.status", Token: "tok"})
	login = step(t, st, "login")
	self = step(t, st, "selftest")
	if login["state"] == "done" || self["state"] == "done" {
		t.Fatalf("code() finished the sign-in itself: login %#v self %#v", login, self)
	}
	var seen []string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(seen) < 3 {
		st, _ = h.Handle(homeproto.Envelope{Op: "t3-setup.status", Token: "tok"})
		login = step(t, st, "login")
		self = step(t, st, "selftest")
		phase := ""
		switch {
		case login["state"] == "working" && login["reason"] == setup.SigningIn && self["state"] != "working" && self["state"] != "done":
			phase = setup.SignInPhases()[0]
		case login["state"] == "done" && self["state"] == "working" && self["reason"] == setup.SelfTestRunning:
			phase = setup.SignInPhases()[1]
		case login["state"] == "done" && self["state"] == "done" && strings.Contains(fmtReason(self), "Ready"):
			phase = setup.SignInPhases()[2]
		}
		if phase != "" && (len(seen) == 0 || seen[len(seen)-1] != phase) {
			seen = append(seen, phase)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if strings.Join(seen, ",") != strings.Join(setup.SignInPhases(), ",") {
		t.Fatalf("phases %v, want %v", seen, setup.SignInPhases())
	}
	if self["action"] != "selftest" {
		t.Fatalf("no rerun: %#v", self)
	}
}

func fmtReason(step map[string]any) string {
	s, _ := step["reason"].(string)
	return s
}

func step(t *testing.T, st any, id string) map[string]any {
	t.Helper()
	for _, raw := range st.(map[string]any)["steps"].([]map[string]any) {
		if raw["id"] == id {
			return raw
		}
	}
	t.Fatalf("no step %s", id)
	return nil
}

func TestSandboxTitlesMatchTheRealHome(t *testing.T) {
	h := NewHome("Sandbox Home", "secret", "https://127.0.0.1:18443/oauth")
	st, err := h.Handle(homeproto.Envelope{Op: "pair", Args: mustJSON(t, map[string]string{"secret": "secret", "token": "tok"})})
	if err != nil {
		t.Fatal(err)
	}
	_ = st
	st, err = h.Handle(homeproto.Envelope{Op: "t3-setup.status", Token: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if step(t, st, "login")["title"] != setup.TitleLogin || step(t, st, "t3")["title"] != setup.TitleT3 ||
		step(t, st, "pair")["title"] != setup.TitlePair || step(t, st, "selftest")["title"] != setup.TitleSelfTest {
		t.Fatalf("titles diverged: login %v t3 %v pair %v self %v", step(t, st, "login")["title"], step(t, st, "t3")["title"], step(t, st, "pair")["title"], step(t, st, "selftest")["title"])
	}
	projects, _ := step(t, st, "selftest")["projects"].([]map[string]any)
	if len(projects) != 1 || projects[0]["id"] != "reviewer" || projects[0]["progress"] == nil {
		t.Fatalf("self-test role row: %#v", projects)
	}
}

func TestWrongCodeFailsTheSignInAfterItWorks(t *testing.T) {
	h := NewHome("Sandbox Home", "secret", "https://127.0.0.1:18443/oauth")
	h.signInFor = 20 * time.Millisecond
	if _, err := h.Handle(homeproto.Envelope{Op: "pair", Args: mustJSON(t, map[string]string{"secret": "secret", "token": "tok"})}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"service", "image", "t3", "pair"} {
		if _, err := h.Handle(homeproto.Envelope{Op: "t3-setup.run", Token: "tok", Args: mustJSON(t, map[string]string{"step": id})}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.Handle(homeproto.Envelope{Op: "t3-setup.run", Token: "tok", Args: mustJSON(t, map[string]string{"step": "login", "method": "claudeai"})}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Handle(homeproto.Envelope{Op: "t3-setup.login.code", Token: "tok", Args: mustJSON(t, map[string]string{"code": "has space"})}); err == nil {
		t.Fatal("a code with a space was accepted")
	}
	if _, err := h.Handle(homeproto.Envelope{Op: "t3-setup.login.code", Token: "tok", Args: mustJSON(t, map[string]string{"code": "not-the-code"})}); err != nil {
		t.Fatal(err)
	}
	st, _ := h.Handle(homeproto.Envelope{Op: "t3-setup.status", Token: "tok"})
	login := step(t, st, "login")
	if login["state"] != "working" || login["action"] != "cancel" || login["action2"] != "check" {
		t.Fatalf("while signing in: %#v", login)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st, _ = h.Handle(homeproto.Envelope{Op: "t3-setup.status", Token: "tok"})
		login = step(t, st, "login")
		if login["state"] == "failed" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	self := step(t, st, "selftest")
	if login["state"] != "failed" || !strings.Contains(fmtReason(login), "that code was not accepted") || login["action"] != "login" || self["state"] == "done" {
		t.Fatalf("wrong code: login %#v self %#v", login, self)
	}
}

func TestNeedsUserSelfTestState(t *testing.T) {
	h := NewHome("Sandbox Home", "secret", "https://127.0.0.1:18443/oauth")
	if _, err := h.Handle(homeproto.Envelope{Op: "pair", Args: mustJSON(t, map[string]string{"secret": "secret", "token": "tok"})}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.state["selftest"] = "needs_user"
	h.mu.Unlock()
	st, _ := h.Handle(homeproto.Envelope{Op: "t3-setup.status", Token: "tok"})
	self := step(t, st, "selftest")
	if self["state"] != "needs_user" || self["action"] != "selftest" || self["reason"] != "Sign in to Claude, then run it again." {
		t.Fatalf("%#v", self)
	}
	h.mu.Lock()
	h.state["selftest"] = "failed"
	h.mu.Unlock()
	st, _ = h.Handle(homeproto.Envelope{Op: "t3-setup.status", Token: "tok"})
	self = step(t, st, "selftest")
	rows, _ := self["projects"].([]map[string]any)
	if self["reason"] != "1 of 1 roles failed." || len(rows) != 1 || rows[0]["reason"] != "The role VM did not start." || rows[0]["state"] != "failed" {
		t.Fatalf("step %#v row %#v", self, rows)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
