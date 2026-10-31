package mcpserver_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/plugins/plugintest"
)

// TestPluginApprovalAnnounced: an approval a plugin asks for on its own (not in a tool call) reaches the
// user. Rubi wakes the administrator with a fresh approval link, and the request waits in the panel's
// list. Rubi stays quiet when an agent already has the link: from the tool call's result, or from the
// plugin's own event to a Bot that follows the plugin.
func TestPluginApprovalAnnounced(t *testing.T) {
	reg := plugintest.New(t)
	reg.Review("v1.0.0", reg.Publish("v1.0.0", nil, nil), "")
	r := newRig(t)
	r.approve(r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}), "install")
	r.connectDemo()
	r.waitFor("plugin", func() bool { return r.ag.call("demo_ping", nil)["started"] == true })
	r.c.HookBase = func() string { return "https://gw.example" }
	hook := func(name, body string) {
		t.Helper()
		u := r.ag.call("demo_hook", map[string]any{"name": name})["url"].(string)
		parts := strings.Split(strings.TrimPrefix(u, "https://gw.example/h/"), "/")
		if !r.c.DeliverHookAt(parts[0], parts[1], []byte(body)) {
			t.Fatal("hook not delivered")
		}
	}
	announced := func(id string) map[string]any {
		for _, b := range r.webhooks("rubi.approval.requested") {
			if b["data"].(map[string]any)["approval_id"] == id {
				return b
			}
		}
		return nil
	}
	pending := func() []string {
		var res map[string]any
		if err := r.panel("settings").Call("approval.list", nil, &res); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, a := range res["approvals"].([]any) {
			ids = append(ids, a.(map[string]any)["approval_id"].(string))
		}
		return ids
	}

	// 1. A background approval (the claude-code case: its own event follows, but no Bot follows demo).
	hook("ask-tell", "bob@example.com")
	var body map[string]any
	r.waitFor("announcement", func() bool {
		bs := r.webhooks("rubi.approval.requested")
		if len(bs) > 0 {
			body = bs[0]
		}
		return body != nil
	})
	data := body["data"].(map[string]any)
	id := data["approval_id"].(string)
	if data["integration"] != "demo" || data["kind"] != "demo.send" || data["summary"] != "Send to bob@example.com" || data["level"] != "strong" {
		t.Fatalf("announcement data: %v", data)
	}
	if ns, _ := body["next_step"].(string); !strings.Contains(ns, "approval_url") || !strings.Contains(ns, "untrusted") {
		t.Fatalf("next_step: %q", ns)
	}
	if uf, _ := body["untrusted_fields"].([]any); len(uf) != 1 || uf[0] != "summary" {
		t.Fatalf("untrusted fields: %v", body["untrusted_fields"])
	}
	// The stored event carries no link; the webhook's link opens exactly this request.
	for _, e := range r.c.Events.List(true) {
		if e.Integration == "rubi" && e.Type == "approval.requested" && e.Data["approval_url"] != nil {
			t.Fatal("the stored event carries a link")
		}
	}
	pc := r.panelLink(body["approval_url"].(string))
	if info, err := pc.ApprovalInfo(id); err != nil || info.Approval.Summary != "Send to bob@example.com" {
		t.Fatalf("the link: %v %v", info, err)
	}
	if ids := pending(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("pending list: %v (want %s)", ids, id)
	}
	if res, err := pc.ApproveWithPassword(id, "send", pw); err != nil || res["state"] != "executed" {
		t.Fatalf("approve: %v %v", res, err)
	}
	if ids := pending(); len(ids) != 0 {
		t.Fatalf("still pending: %v", ids)
	}

	// 2. An approval asked for in a tool call: the agent has the link in the result.
	out := r.ag.call("demo_send", map[string]any{"to": "carol@example.com"})
	tid, _ := out["approval_id"].(string)
	if tid == "" || out["approval_url"] == nil {
		t.Fatalf("tool call: %v", out)
	}

	// 3. A Bot follows demo: the plugin's own event brings it the link.
	def := ""
	for _, a := range r.c.Agents() {
		if a.Default {
			def = a.Name
		}
	}
	if _, err := r.c.SetSubscriptions(def, []string{"demo"}); err != nil {
		t.Fatal(err)
	}
	hook("ask-tell", "dan@example.com")
	var did string
	r.waitFor("the plugin's event", func() bool {
		for _, b := range r.webhooks("demo.ping") {
			if d, _ := b["data"].(map[string]any); d["from"] == "hook" {
				did, _ = d["approval_id"].(string)
			}
		}
		return did != ""
	})
	time.Sleep(pastGrace) // past Rubi's grace period
	if announced(tid) != nil || announced(did) != nil {
		t.Fatalf("announced although an agent has the link: %v", r.webhooks("rubi.approval.requested"))
	}
	if ids := pending(); len(ids) != 2 {
		t.Fatalf("both still wait in the list: %v", ids)
	}

	// 4. A plain background approval with a follower but no event of its own is still announced.
	hook("ask", "erin@example.com")
	r.waitFor("announcement", func() bool { return len(r.webhooks("rubi.approval.requested")) == 2 })
}
