package mcpserver_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/plugins/plugintest"
)

// TestApprovalWakesAgentWithPlan: after the user approves in the panel, the agent's routine webhook gets
// the decision together with the plan the agent left, so it continues without the user writing to it.
func TestApprovalWakesAgentWithPlan(t *testing.T) {
	reg := plugintest.New(t)
	reg.Review("v1.0.0", reg.Publish("v1.0.0", nil, nil), "")
	r := newRig(t)
	r.approve(r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}), "install")
	r.connectDemo()
	r.waitFor("plugin", func() bool { return r.ag.call("demo_ping", nil)["started"] == true })

	// Without a webhook the agent is told it won't be woken.
	r.setWebhook("")
	if wh := r.ag.call("rubi_status", nil)["webhook"].(map[string]any); wh["configured"] != false || wh["how"] == nil {
		t.Fatalf("webhook hint: %v", wh)
	}
	out := r.ag.call("demo_send", map[string]any{"to": "bob"})
	if !strings.Contains(out["after_sending_the_link"].(string), "can't wake you") {
		t.Fatalf("hint without webhook: %v", out)
	}
	r.ag.call("rubi_cancel", map[string]any{"approval_id": out["approval_id"]})

	bodies := make(chan map[string]any, 20)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		bodies <- m
	}))
	defer hook.Close()
	r.setWebhook(hook.URL)

	out = r.ag.call("demo_send", map[string]any{"to": "carol"})
	if !strings.Contains(out["after_sending_the_link"].(string), "rubi_continue_after") {
		t.Fatalf("hint with webhook: %v", out)
	}
	plan := "Tell the user the message to carol went out, then summarize the thread."
	if res := r.ag.call("rubi_continue_after", map[string]any{"approval_id": out["approval_id"], "plan": plan}); res["saved"] != true {
		t.Fatalf("plan: %v", res)
	}
	// The user approves after the agent's turn ended (the agent isn't polling rubi_approval).
	if res, err := r.panelLink(out["approval_url"].(string)).ApproveWithPassword(out["approval_id"].(string), "send", pw); err != nil || res["state"] != "executed" {
		t.Fatalf("approve: %v %v", res, err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case b := <-bodies:
			d, _ := b["data"].(map[string]any)
			if b["type"] != "rubi.approval.decided" || d["approval_id"] != out["approval_id"] {
				continue
			}
			if d["your_plan"] != plan || d["state"] != "executed" || d["result"].(map[string]any)["sent_to"] != "carol" ||
				!strings.Contains(b["next_step"].(string), "your_plan") {
				t.Fatalf("decision event: %v", b)
			}
			return
		case <-deadline:
			t.Fatal("the agent's webhook never got the decision")
		}
	}
}

// TestNoWakeWhenNotNeeded: the agent isn't woken again when it already saw the outcome itself; without a
// plan it is still told the outcome.
func TestNoWakeWhenNotNeeded(t *testing.T) {
	reg := plugintest.New(t)
	reg.Review("v1.0.0", reg.Publish("v1.0.0", nil, nil), "")
	r := newRig(t)
	r.approve(r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}), "install")
	r.connectDemo()
	r.waitFor("plugin", func() bool { return r.ag.call("demo_ping", nil)["started"] == true })
	hits := make(chan string, 20)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if d, ok := m["data"].(map[string]any); ok {
			hits <- toString(d["approval_id"])
		}
	}))
	defer hook.Close()
	r.setWebhook(hook.URL)
	time.Sleep(4 * time.Second)
	for len(hits) > 0 {
		<-hits
	}

	// 1. With a plan, but the agent saw the outcome while waiting: no wake.
	seen := r.ag.call("demo_send", map[string]any{"to": "a"})
	r.ag.call("rubi_continue_after", map[string]any{"approval_id": seen["approval_id"], "plan": "p"})
	r.approve(seen, "send") // calls rubi_approval afterwards, like a waiting agent
	// 2. No plan, turn over: the outcome still reaches the agent.
	quiet := r.ag.call("demo_send", map[string]any{"to": "b"})
	if res, err := r.panelLink(quiet["approval_url"].(string)).ApproveWithPassword(quiet["approval_id"].(string), "send", pw); err != nil || res["state"] != "executed" {
		t.Fatalf("approve: %v %v", res, err)
	}
	time.Sleep(5 * time.Second)
	var woke []string
	for len(hits) > 0 {
		woke = append(woke, <-hits)
	}
	if len(woke) != 1 || woke[0] != toString(quiet["approval_id"]) {
		t.Fatalf("wakes: %v (want only %v)", woke, quiet["approval_id"])
	}
}

// TestPluginWaitsForItsApproval: a plugin learns how its own approvals end (approval.wait), denials
// included, but not anyone else's; an outcome it saw doesn't wake the agent.
func TestPluginWaitsForItsApproval(t *testing.T) {
	reg := plugintest.New(t)
	reg.Review("v1.0.0", reg.Publish("v1.0.0", nil, nil), "")
	r := newRig(t)
	r.approve(r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}), "install")
	r.connectDemo()
	r.waitFor("plugin", func() bool { return r.ag.call("demo_ping", nil)["started"] == true })
	hits := make(chan string, 20)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if d, ok := m["data"].(map[string]any); ok {
			hits <- toString(d["approval_id"])
		}
	}))
	defer hook.Close()
	r.setWebhook(hook.URL)

	denied := r.ag.call("demo_send", map[string]any{"to": "a"})
	id := denied["approval_id"]
	if st := r.ag.call("demo_wait", map[string]any{"approval_id": id}); st["state"] != "pending" {
		t.Fatalf("before deciding: %v", st)
	}
	if _, err := r.panelLink(denied["approval_url"].(string)).Deny(id.(string)); err != nil {
		t.Fatal(err)
	}
	if st := r.ag.call("demo_wait", map[string]any{"approval_id": id, "wait_seconds": 5}); st["state"] != "denied" {
		t.Fatalf("after denying: %v", st)
	}

	approved := r.ag.call("demo_send", map[string]any{"to": "b"})
	if res, err := r.panelLink(approved["approval_url"].(string)).ApproveWithPassword(approved["approval_id"].(string), "send", pw); err != nil || res["state"] != "executed" {
		t.Fatalf("approve: %v %v", res, err)
	}
	if st := r.ag.call("demo_wait", map[string]any{"approval_id": approved["approval_id"], "wait_seconds": 5}); st["state"] != "executed" || st["chosen_option"] != "send" {
		t.Fatalf("after approving: %v", st)
	}

	// Rubi's own approval (a settings change) isn't the plugin's to see.
	var res map[string]any
	if err := r.panel("settings").Call("webhook.set", map[string]any{"url": hook.URL, "key": "crsr_other"}, &res); err != nil {
		t.Fatal(err)
	}
	if st := r.ag.call("demo_wait", map[string]any{"approval_id": res["approval_id"]}); !strings.Contains(toString(st["error"]), "unknown approval") {
		t.Fatalf("another integration's approval: %v", st)
	}

	time.Sleep(5 * time.Second) // past the grace before a wake
	for len(hits) > 0 {
		if h := <-hits; h == toString(approved["approval_id"]) {
			t.Fatal("the agent was woken for an outcome the plugin already handled")
		}
	}
}
