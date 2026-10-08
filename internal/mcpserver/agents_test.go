package mcpserver_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/plugins/plugintest"
)

type hookHit struct {
	typ  string
	auth string
	data map[string]any
}

func hookServer(t *testing.T) (*httptest.Server, chan hookHit) {
	ch := make(chan hookHit, 50)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		d, _ := m["data"].(map[string]any)
		ch <- hookHit{typ: toString(m["type"]), auth: req.Header.Get("Authorization"), data: d}
	}))
	t.Cleanup(s.Close)
	return s, ch
}

func waitHit(t *testing.T, ch chan hookHit, typ string) hookHit {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case h := <-ch:
			if h.typ == `"`+typ+`"` {
				return h
			}
		case <-deadline:
			t.Fatalf("no %s event", typ)
		}
	}
}

func noHit(t *testing.T, ch chan hookHit, typ string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case h := <-ch:
			if h.typ == `"`+typ+`"` {
				t.Fatalf("unexpected %s event: %v", typ, h)
			}
		case <-deadline:
			return
		}
	}
}

// TestSeveralAgents: Bots sharing one Rubi each get their own events.
func TestSeveralAgents(t *testing.T) {
	reg := plugintest.New(t)
	reg.Review("v1.0.0", reg.Publish("v1.0.0", nil, nil), "")
	r := newRig(t) // registers "Main" (default) through the older single-webhook setting
	mainSrv, mainCh := hookServer(t)
	mailSrv, mailCh := hookServer(t)
	r.setWebhook(mainSrv.URL)

	// The mail Bot registers itself through its own link; the paste may include the whole header.
	link := r.panel("agent:Mail")
	var res map[string]any
	if err := link.Call("agent.add", map[string]any{"name": "Other", "url": mailSrv.URL, "key": "x"}, &res); err == nil {
		t.Fatal("an agent link registered a different agent")
	}
	if err := link.Call("policy.set", map[string]any{"levels": map[string]string{}}, &res); err == nil {
		t.Fatal("an agent link changed other settings")
	}
	if err := link.Call("agent.add", map[string]any{"name": "mail", "url": mailSrv.URL, "key": "x", "sources": []string{"nope"}}, &res); err == nil {
		t.Fatal("unknown source accepted")
	}
	if err := link.Call("agent.add", map[string]any{"name": "mail", "url": mailSrv.URL, "key": "Authorization: Bearer crsr_mail", "sources": []string{"rubi"}}, &res); err != nil {
		t.Fatal(err)
	}
	r.approveChange(link, res)
	for _, a := range r.c.Agents() {
		if a.Name == "mail" && (len(a.Subscriptions) != 1 || a.Subscriptions[0] != "rubi") {
			t.Fatalf("subscriptions chosen at connect: %+v", a)
		}
	}
	agents := r.ag.call("rubi_status", nil)["webhook"].(map[string]any)["agents"].([]any)
	if len(agents) != 2 {
		t.Fatalf("agents: %v", agents)
	}

	r.approve(r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}), "install")
	r.connectDemo()
	r.waitFor("plugin", func() bool { return r.ag.call("demo_ping", nil)["started"] == true })
	waitHit(t, mainCh, "demo.ping") // unassigned plugin: the default agent

	// The mail Bot subscribes itself to the plugin's events (no approval needed: it only routes among
	// webhooks the user approved). Unknown sources and agents are refused.
	if e := r.ag.call("rubi_notifications", map[string]any{"agent": "Mail", "sources": []string{"nope"}}); e["tool_error"] == nil {
		t.Fatalf("unknown source accepted: %v", e)
	}
	if e := r.ag.call("rubi_notifications", map[string]any{"agent": "Ghost"}); e["tool_error"] == nil {
		t.Fatalf("unknown agent accepted: %v", e)
	}
	me := r.ag.call("rubi_notifications", map[string]any{"agent": "mail", "sources": []string{"demo"}})
	if subs := me["you"].(map[string]any)["subscriptions"].([]any); len(subs) != 1 || subs[0] != "demo" {
		t.Fatalf("subscribe: %v", me)
	}
	r.ag.call("demo_ping", nil)
	if h := waitHit(t, mailCh, "demo.ping"); h.auth != "Bearer crsr_mail" {
		t.Fatalf("mail Bot auth: %q", h.auth)
	}
	noHit(t, mainCh, "demo.ping")

	// An approval wakes the Bot that asked; unknown names are refused.
	out := r.ag.call("demo_send", map[string]any{"to": "x"})
	if e := r.ag.call("rubi_continue_after", map[string]any{"approval_id": out["approval_id"], "plan": "p", "agent": "Nobody"}); !strings.Contains(toString(e["tool_error"]), "no agent") {
		t.Fatalf("unknown agent: %v", e)
	}
	r.ag.call("rubi_continue_after", map[string]any{"approval_id": out["approval_id"], "plan": "tell the user", "agent": "Mail"})
	if r2, err := r.panelLink(out["approval_url"].(string)).ApproveWithPassword(out["approval_id"].(string), "send", pw); err != nil || r2["state"] != "executed" {
		t.Fatalf("approve: %v %v", r2, err)
	}
	if h := waitHit(t, mailCh, "rubi.approval.decided"); h.data["your_plan"] != "tell the user" {
		t.Fatalf("decision: %v", h)
	}
	noHit(t, mainCh, "rubi.approval.decided")
}

// TestPluginConfigAndTargets: user-only settings change only through the panel with approval, locked
// levels can't be lowered, and a plugin can address one Bot.
func TestPluginConfigAndTargets(t *testing.T) {
	reg := plugintest.New(t)
	reg.Review("v1.0.0", reg.Publish("v1.0.0", nil, nil), "")
	r := newRig(t)
	mainSrv, mainCh := hookServer(t)
	mailSrv, mailCh := hookServer(t)
	r.setWebhook(mainSrv.URL)
	settings := r.panel("settings")
	var res map[string]any
	if err := settings.Call("agent.add", map[string]any{"name": "Mail", "url": mailSrv.URL, "key": "k"}, &res); err != nil {
		t.Fatal(err)
	}
	r.approveChange(settings, res)
	r.approve(r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}), "install")
	r.connectDemo()
	r.waitFor("plugin", func() bool { return r.ag.call("demo_ping", nil)["started"] == true })

	if cfg := r.ag.call("demo_config", nil); cfg["strict"] != true || toString(cfg["hidden"]) != `["code"]` {
		t.Fatalf("defaults: %v", cfg)
	}
	if err := settings.Call("plugin.config.set", map[string]any{"id": "demo", "values": map[string]any{"hidden": "nope"}}, &res); err == nil {
		t.Fatal("bad config value accepted")
	}
	if err := settings.Call("plugin.config.set", map[string]any{"id": "demo", "values": map[string]any{"hidden": []string{"code", "pin"}, "strict": false}}, &res); err != nil {
		t.Fatal(err)
	}
	r.approveChange(settings, res)
	if cfg := r.ag.call("demo_config", nil); cfg["strict"] != false || toString(cfg["hidden"]) != `["code","pin"]` {
		t.Fatalf("after change: %v", cfg)
	}
	// Dynamic choices come from the plugin; a choice must be one of the offered keys.
	var cfgView map[string]any
	if err := settings.Call("plugin.config.get", map[string]any{"id": "demo"}, &cfgView); err != nil ||
		!strings.Contains(toString(cfgView["fields"]), `"Receipts"`) {
		t.Fatalf("config view: %v %v", cfgView, err)
	}
	if err := settings.Call("plugin.config.set", map[string]any{"id": "demo", "values": map[string]any{"mode": "some"}}, &res); err == nil {
		t.Fatal("an unknown choice was accepted")
	}

	if err := settings.Call("policy.set", map[string]any{"levels": map[string]string{"demo.secret": "none"}}, &res); err == nil {
		t.Fatal("a locked level was lowered")
	}

	time.Sleep(time.Second) // earlier pings went to the default Bot; drop them
	for len(mainCh) > 0 {
		<-mainCh
	}
	r.ag.call("demo_notify", map[string]any{"agent": "Mail"})
	waitHit(t, mailCh, "demo.ping")
	noHit(t, mainCh, "demo.ping")
}

// TestQuietUpdates: with notifications off for a plugin, its update doesn't wake the agent but shows in
// rubi_updates and on the panel's Updates page, which any link can open.
func TestQuietUpdates(t *testing.T) {
	reg := plugintest.New(t)
	reg.Review("v1.0.0", reg.Publish("v1.0.0", nil, nil), "")
	r := newRig(t)
	r.approve(r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}), "install")
	r.connectDemo()
	r.waitFor("plugin", func() bool { return r.ag.call("demo_ping", nil)["started"] == true })
	if e := r.ag.call("rubi_update_notifications", map[string]any{"id": "nope", "notify": false}); e["tool_error"] == nil {
		t.Fatal("unknown id accepted")
	}
	r.ag.call("rubi_update_notifications", map[string]any{"id": "demo", "notify": false})
	reg.Review("v1.1.0", reg.Publish("v1.1.0", nil, nil), "")
	r.c.CheckPluginUpdates(context.Background())
	for _, e := range r.events() {
		if e["type"] == "plugin.update_available" {
			t.Fatalf("a quiet plugin woke the agent: %v", e)
		}
	}
	items := r.ag.call("rubi_updates", nil)["updates"].([]any)
	demo := items[len(items)-1].(map[string]any)
	if demo["id"] != "demo" || demo["available"] != true || demo["latest"] != "v1.1.0" || demo["notify"] != false {
		t.Fatalf("rubi_updates: %v", items)
	}

	// The Updates page works from any link, e.g. an approval link.
	out := r.ag.call("demo_send", map[string]any{"to": "x"})
	var list map[string]any
	if err := r.panelLink(out["approval_url"].(string)).Call("updates.list", nil, &list); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(toString(list), `"latest":"v1.1.0"`) {
		t.Fatalf("updates.list: %v", list)
	}

	// Back on: the next check tells the agent.
	r.ag.call("rubi_update_notifications", map[string]any{"id": "demo", "notify": true})
	r.c.CheckPluginUpdates(context.Background())
	found := false
	for _, e := range r.events() {
		found = found || e["type"] == "plugin.update_available"
	}
	if !found {
		t.Fatal("no update event after turning notifications back on")
	}
}
