package sandbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
)

func TestHomeLoginWaitsAndThenReturnsTheFullLink(t *testing.T) {
	h := NewHome("Sandbox Home", "secret", "https://127.0.0.1:18443/oauth/authorize?state="+strings.Repeat("a", 480))
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
	if _, err := h.Handle(homeproto.Envelope{Op: "t3-setup.login.code", Token: "tok", Args: mustJSON(t, map[string]string{"code": "nope"})}); err == nil {
		t.Fatal("wrong code accepted")
	}
	if _, err := h.Handle(homeproto.Envelope{Op: "t3-setup.login.code", Token: "tok", Args: mustJSON(t, map[string]string{"code": SignInCode})}); err != nil {
		t.Fatal(err)
	}
	st, _ = h.Handle(homeproto.Envelope{Op: "t3-setup.status", Token: "tok"})
	login = step(t, st, "login")
	if login["state"] != "done" {
		t.Fatalf("after code: %#v", login)
	}
	self = step(t, st, "selftest")
	projects, _ := self["projects"].([]map[string]any)
	if self["state"] != "done" || !strings.Contains(fmtReason(self), "Ready") {
		t.Fatalf("self-test did not follow sign-in: %#v", self)
	}
	if self["action"] != "selftest" || len(projects) != 1 || projects[0]["action"] != "selftest" {
		t.Fatalf("no rerun after the automatic self-test: %#v", self)
	}
	if _, err := h.Handle(homeproto.Envelope{Op: "t3-setup.run", Token: "tok", Args: mustJSON(t, map[string]string{"step": "selftest", "project": "reviewer"})}); err != nil {
		t.Fatal(err)
	}
	st, _ = h.Handle(homeproto.Envelope{Op: "t3-setup.status", Token: "tok"})
	self = step(t, st, "selftest")
	if self["state"] != "done" || self["action"] != "selftest" {
		t.Fatalf("after rerun: %#v", self)
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

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
