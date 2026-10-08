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
	settings := r.panel("settings")
	var res map[string]any
	if err := settings.Call("webhook.set", map[string]any{"url": hook.URL, "key": "crsr_test"}, &res); err != nil {
		t.Fatal(err)
	}
	r.approveChange(settings, res)

	out = r.ag.call("demo_send", map[string]any{"to": "carol"})
	if !strings.Contains(out["after_sending_the_link"].(string), "rubi_continue_after") {
		t.Fatalf("hint with webhook: %v", out)
	}
	plan := "Tell the user the message to carol went out, then summarize the thread."
	if res := r.ag.call("rubi_continue_after", map[string]any{"approval_id": out["approval_id"], "plan": plan}); res["saved"] != true {
		t.Fatalf("plan: %v", res)
	}
	r.approve(out, "send")

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
