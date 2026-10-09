package mcpserver_test

import (
	"context"
	"encoding/json"
	"github.com/Deikus-LXXVII/rubi/internal/home"
	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
	"github.com/Deikus-LXXVII/rubi/internal/panelclient"
	"github.com/Deikus-LXXVII/rubi/internal/relay"
	"github.com/Deikus-LXXVII/rubi/internal/relay/relaytest"
	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
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
	code string
	data map[string]any
}

func hookServer(t *testing.T) (*httptest.Server, chan hookHit) {
	ch := make(chan hookHit, 50)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		d, _ := m["data"].(map[string]any)
		code, _ := m["agent_code"].(string)
		ch <- hookHit{typ: toString(m["type"]), auth: req.Header.Get("Authorization"), code: code, data: d}
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

	// Two Bots share Rubi, so plugin accounts need access codes.
	if out := r.ag.call("demo_ping", nil); out["status"] != "access_needed" {
		t.Fatalf("plugin call without access: %v", out)
	}
	accessOf := func(agent string, ch chan hookHit) string {
		t.Helper()
		out := r.ag.call("rubi_access", map[string]any{"agent": agent, "plugin": "demo", "task": "test the plugin", "minutes": 30})
		if out["status"] == "awaiting_approval" {
			r.approve(out, "allow")
		} else if out["status"] != "sent" {
			t.Fatalf("access for %s: %v", agent, out)
		}
		return toString(waitHit(t, ch, "rubi.access.granted").data["access_code"])
	}
	mainAccess := accessOf("Main", mainCh) // the administrator: nobody is assigned, so it's let in at once
	dcall := func(tool string, args map[string]any) map[string]any {
		if args == nil {
			args = map[string]any{}
		}
		args["rubi_agent"], args["rubi_access"] = "Main", strings.Trim(mainAccess, `"`)
		return r.ag.call(tool, args)
	}
	r.waitFor("plugin", func() bool { return dcall("demo_ping", nil)["started"] == true })
	if out := r.ag.call("demo_ping", map[string]any{"rubi_agent": "mail", "rubi_access": strings.Trim(mainAccess, `"`)}); out["status"] != "access_needed" {
		t.Fatalf("another Bot used the administrator's code: %v", out)
	}
	// The mail Bot isn't assigned: the user approves its access.
	mailAccess := strings.Trim(accessOf("mail", mailCh), `"`)
	if out := r.ag.call("demo_ping", map[string]any{"rubi_agent": "mail", "rubi_access": mailAccess}); out["started"] != true {
		t.Fatalf("approved access: %v", out)
	}
	// Once the user assigns the mail Bot to the account, its requests skip the approval (here it is turned
	// away only because a code went out a moment ago).
	settings := r.panel("settings")
	var asg map[string]any
	if err := settings.Call("account.agents", map[string]any{"id": "demo", "account": "", "agents": []string{"mail"}}, &asg); err != nil {
		t.Fatal(err)
	}
	r.approveChange(settings, asg)
	if out := r.ag.call("rubi_access", map[string]any{"agent": "mail", "plugin": "demo", "task": "again"}); !strings.Contains(toString(out["tool_error"]), "a moment ago") {
		t.Fatalf("assigned Bot: %v", out)
	}
	if out := r.ag.call("rubi_access", map[string]any{"agent": "Main", "plugin": "demo", "task": "x", "minutes": 500}); out["tool_error"] == nil {
		t.Fatalf("more than 2 hours accepted: %v", out)
	}
	// A plugin no Bot subscribed to wakes no one; its events wait for the administrator in the list.
	noHit(t, mainCh, "demo.ping")
	noHit(t, mailCh, "demo.ping")
	// Reading needs a code that only the Bot's own webhook receives.
	if e := r.ag.call("rubi_events", map[string]any{"agent": "Main"}); e["needs_code"] != true || e["events"] != nil {
		t.Fatalf("events without a code: %v", e)
	}
	codeOf := func(agent string, ch chan hookHit) string {
		t.Helper()
		if e := r.ag.call("rubi_verify", map[string]any{"agent": agent}); e["status"] != "sent" {
			t.Fatalf("verify: %v", e)
		}
		return waitHit(t, ch, "rubi.agent.code").code
	}
	mainCode, mailCode := codeOf("Main", mainCh), codeOf("mail", mailCh)
	findPing := func(agent, code string) bool {
		for _, e := range r.ag.call("rubi_events", map[string]any{"agent": agent, "code": code})["events"].([]any) {
			if e.(map[string]any)["type"] == "ping" {
				return true
			}
		}
		return false
	}
	if !findPing("Main", mainCode) || findPing("mail", mailCode) {
		t.Fatal("the administrator should see the unassigned event, the mail Bot not")
	}
	if e := r.ag.call("rubi_events", map[string]any{"agent": "Ghost", "code": "x"}); e["tool_error"] == nil {
		t.Fatalf("an unknown Bot read events: %v", e)
	}
	// Posing as the administrator: its own code doesn't work for another name, and asking for one sends it
	// to the real Bot. Repeated wrong codes block the name and warn the administrator.
	if e := r.ag.call("rubi_events", map[string]any{"agent": "Main", "code": mailCode}); e["tool_error"] == nil {
		t.Fatalf("another Bot's code accepted: %v", e)
	}
	r.ag.call("rubi_events", map[string]any{"agent": "Main", "code": "guess1"})
	if e := r.ag.call("rubi_events", map[string]any{"agent": "Main", "code": "guess2"}); !strings.Contains(toString(e["tool_error"]), "blocked") {
		t.Fatalf("no block after wrong codes: %v", e)
	}
	if h := waitHit(t, mainCh, "rubi.agent.suspicious"); h.data["agent"] != "Main" {
		t.Fatalf("warning: %v", h)
	}
	if e := r.ag.call("rubi_events", map[string]any{"agent": "Main", "code": mainCode}); e["tool_error"] == nil {
		t.Fatalf("a blocked name still reads: %v", e)
	}

	// The mail Bot subscribes itself to the plugin's events (no approval needed: it only routes among
	// webhooks the user approved). Unknown sources and agents are refused.
	if e := r.ag.call("rubi_notifications", map[string]any{"agent": "Mail", "sources": []string{"nope"}}); e["tool_error"] == nil {
		t.Fatalf("unknown source accepted: %v", e)
	}
	if e := r.ag.call("rubi_notifications", map[string]any{"agent": "Ghost"}); e["tool_error"] == nil {
		t.Fatalf("unknown agent accepted: %v", e)
	}
	// Hearing about a plugin gives a Bot its events: the user approves it.
	me := r.ag.call("rubi_notifications", map[string]any{"agent": "mail", "sources": []string{"demo"}})
	if subs := me["you"].(map[string]any)["subscriptions"].([]any); len(subs) != 1 || subs[0] != "rubi" {
		t.Fatalf("subscribed before approval: %v", me)
	}
	r.approve(me, "apply")
	me = r.ag.call("rubi_notifications", map[string]any{"agent": "mail"})
	if subs := me["you"].(map[string]any)["subscriptions"].([]any); len(subs) != 1 || subs[0] != "demo" {
		t.Fatalf("subscribe: %v", me)
	}
	dcall("demo_ping", nil)
	if h := waitHit(t, mailCh, "demo.ping"); h.auth != "Bearer crsr_mail" {
		t.Fatalf("mail Bot auth: %q", h.auth)
	}
	noHit(t, mainCh, "demo.ping")

	// An approval wakes the Bot that asked; unknown names are refused.
	out := dcall("demo_send", map[string]any{"to": "x"})
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
	call := grantAccess(t, r, "Main", mainCh) // two Bots: plugin calls carry an access code
	r.waitFor("plugin", func() bool { return call("demo_ping", nil)["started"] == true })

	if cfg := call("demo_config", nil); cfg["strict"] != true || toString(cfg["hidden"]) != `["code"]` {
		t.Fatalf("defaults: %v", cfg)
	}
	if err := settings.Call("plugin.config.set", map[string]any{"id": "demo", "values": map[string]any{"hidden": "nope"}}, &res); err == nil {
		t.Fatal("bad config value accepted")
	}
	if err := settings.Call("plugin.config.set", map[string]any{"id": "demo", "values": map[string]any{"hidden": []string{"code", "pin"}, "strict": false}}, &res); err != nil {
		t.Fatal(err)
	}
	r.approveChange(settings, res)
	if cfg := call("demo_config", nil); cfg["strict"] != false || toString(cfg["hidden"]) != `["code","pin"]` {
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
	call("demo_notify", map[string]any{"agent": "Mail"})
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

// TestBatchApproval: a Bot bundles several requests; the user approves a subset with one proof, and only
// that subset runs.
func TestBatchApproval(t *testing.T) {
	reg := plugintest.New(t)
	reg.Review("v1.0.0", reg.Publish("v1.0.0", nil, nil), "")
	r := newRig(t)
	r.approve(r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}), "install")
	r.connectDemo()
	r.waitFor("plugin", func() bool { return r.ag.call("demo_ping", nil)["started"] == true })

	out := r.ag.call("demo_batch", map[string]any{"to": []string{"ann", "bob", "cy"}})
	pc := r.panelLink(out["approval_url"].(string))
	info, err := pc.ApprovalInfo(out["approval_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Approval.Preview) == 0 {
		t.Fatal("no preview")
	}
	id := out["approval_id"].(string)
	if _, err := pc.ApproveWithPassword(id, "items:zed", pw); err == nil {
		t.Fatal("an unknown item was approved")
	}
	if _, err := pc.ApproveWithPassword(id, "items:", pw); err == nil {
		t.Fatal("an empty selection was approved")
	}
	res, err := pc.ApproveWithPassword(id, "items:cy,ann", pw)
	if err != nil || res["state"] != "executed" {
		t.Fatalf("approve subset: %v %v", res, err)
	}
	snap := r.ag.call("rubi_approval", map[string]any{"approval_id": id})["approval"].(map[string]any)
	if toString(snap["result"].(map[string]any)["sent_to"]) != `["ann","cy"]` {
		t.Fatalf("executed: %v", snap)
	}
}

// TestSeveralAccounts: a plugin connects any number of accounts, each with its own secrets and per-account
// settings; disconnecting one leaves the others.
func TestSeveralAccounts(t *testing.T) {
	reg := plugintest.New(t)
	reg.Review("v1.0.0", reg.Publish("v1.0.0", nil, nil), "")
	r := newRig(t)
	r.approve(r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}), "install")
	r.connectDemo() // account "ann" (the default)
	setup := r.panel("setup:demo")
	var res map[string]any
	if err := setup.Call("integration.setup", map[string]any{"id": "demo", "fields": map[string]string{"user": "bob"},
		"secrets": map[string]string{"token": "good"}}, &res); err != nil {
		t.Fatal(err)
	}
	r.approveChange(setup, res)
	r.waitFor("plugin", func() bool { return r.ag.call("demo_ping", nil)["started"] == true })

	if out := r.ag.call("demo_who", nil); out["user"] != "ann" || out["accounts"] != float64(2) {
		t.Fatalf("default account: %v", out)
	}
	if out := r.ag.call("demo_who", map[string]any{"account": "BOB"}); out["user"] != "bob" || out["token_ok"] != true {
		t.Fatalf("named account: %v", out)
	}
	if out := r.ag.call("demo_who", map[string]any{"account": "carol"}); !strings.Contains(toString(out["tool_error"]), "no connected account") {
		t.Fatalf("unknown account: %v", out)
	}

	// Per-account settings: bob's folders change, ann's stay.
	settings := r.panel("settings")
	if err := settings.Call("plugin.config.set", map[string]any{"id": "demo", "account": "bob", "values": map[string]any{"folders": []string{"Archive"}}}, &res); err != nil {
		t.Fatal(err)
	}
	r.approveChange(settings, res)
	if out := r.ag.call("demo_who", map[string]any{"account": "bob"}); toString(out["folders"]) != `["Archive"]` {
		t.Fatalf("bob's folders: %v", out)
	}
	if out := r.ag.call("demo_who", nil); toString(out["folders"]) != `["INBOX"]` {
		t.Fatalf("ann's folders changed: %v", out)
	}

	// Disconnect ann (the default): bob remains and becomes the default.
	if err := settings.Call("integration.disconnect", map[string]any{"id": "demo", "account": "ann"}, &res); err != nil {
		t.Fatal(err)
	}
	r.approveChange(settings, res)
	if out := r.ag.call("demo_who", nil); out["user"] != "bob" || out["accounts"] != float64(1) {
		t.Fatalf("after disconnecting ann: %v", out)
	}
}

// TestSetupSteps: a plugin may ask for more during setup (a login code); the panel sends it back with the
// fields entered so far, and only then is the account connected.
func TestSetupSteps(t *testing.T) {
	reg := plugintest.New(t)
	reg.Review("v1.0.0", reg.Publish("v1.0.0", nil, nil), "")
	r := newRig(t)
	r.approve(r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}), "install")
	setup := r.panel("setup:demo")
	var res map[string]any
	if err := setup.Call("integration.setup", map[string]any{"id": "demo", "fields": map[string]string{"user": "dan"},
		"secrets": map[string]string{"token": "2fa"}}, &res); err != nil {
		t.Fatal(err)
	}
	more, _ := res["need_more"].(map[string]any)
	if more == nil || more["step"] != "code-sent" || res["approval_id"] != nil {
		t.Fatalf("first step: %v", res)
	}
	next := map[string]string{"user": "dan", "code": "000", "_step": "code-sent"}
	err := setup.Call("integration.setup", map[string]any{"id": "demo", "fields": next, "secrets": map[string]string{"token": "2fa"}}, &res)
	if err == nil || !strings.Contains(err.Error(), "wrong code") {
		t.Fatalf("wrong code: %v %v", err, res)
	}
	next["code"] = "123"
	res = nil
	if err := setup.Call("integration.setup", map[string]any{"id": "demo", "fields": next, "secrets": map[string]string{"token": "2fa"}}, &res); err != nil {
		t.Fatal(err)
	}
	if res["account"] != "dan" || res["approval_id"] == nil {
		t.Fatalf("second step: %v", res)
	}
	r.approveChange(setup, res)
	r.waitFor("plugin", func() bool { return r.ag.call("demo_ping", nil)["started"] == true })
	if out := r.ag.call("demo_who", nil); out["user"] != "dan" {
		t.Fatalf("connected: %v", out)
	}
}

// TestHooks: a plugin hands out a private web address; a request to it reaches the plugin, which can
// wake the agent. A rotated address replaces the old one.
func TestHooks(t *testing.T) {
	reg := plugintest.New(t)
	reg.Review("v1.0.0", reg.Publish("v1.0.0", nil, nil), "")
	r := newRig(t)
	r.approve(r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}), "install")
	r.connectDemo()
	r.waitFor("plugin", func() bool { return r.ag.call("demo_ping", nil)["started"] == true })

	if out := r.ag.call("demo_hook", map[string]any{"name": "home:arrive"}); !strings.Contains(toString(out["tool_error"]), "Rubi Gateway or Tailscale") {
		t.Fatalf("no hook base: %v", out)
	}
	r.c.HookBase = func() string { return "https://gw.example" }
	u := r.ag.call("demo_hook", map[string]any{"name": "home:arrive"})["url"].(string)
	if again := r.ag.call("demo_hook", map[string]any{"name": "home:arrive"})["url"].(string); again != u {
		t.Fatalf("the address changed: %s %s", u, again)
	}
	parts := strings.Split(strings.TrimPrefix(u, "https://gw.example/h/"), "/")
	// The URL carries an address derived from the route secret, never the secret itself.
	if len(parts) != 2 || relay.HookAddress(r.c.HookRoute()) != parts[0] || strings.Contains(u, r.c.HookRoute()) {
		t.Fatalf("address %s, route %s", u, r.c.HookRoute())
	}
	if r.c.DeliverHookAt("wrong_route_000000000", parts[1], nil) || r.c.DeliverHook("nope", nil) {
		t.Fatal("delivered to a wrong address")
	}
	if !r.c.DeliverHookAt(parts[0], parts[1], []byte(`{"x":1}`)) {
		t.Fatal("not delivered")
	}
	r.waitFor("event", func() bool {
		for _, e := range r.c.Events.List(true) {
			if e.Type == "hooked" && e.Data["name"] == "home:arrive" && e.Data["body"] == `{"x":1}` {
				return true
			}
		}
		return false
	})
	// Rotating: the old address stops working.
	nu := r.ag.call("demo_hook", map[string]any{"name": "home:arrive", "rotate": true})["url"].(string)
	if nu == u || r.c.DeliverHook(parts[1], nil) {
		t.Fatalf("rotate: %s %s", u, nu)
	}
	// Disconnecting the account drops its addresses.
	np := strings.Split(strings.TrimPrefix(nu, "https://gw.example/h/"), "/")
	settings := r.panel("settings")
	var res map[string]any
	if err := settings.Call("integration.disconnect", map[string]any{"id": "demo", "account": ""}, &res); err != nil {
		t.Fatal(err)
	}
	r.approveChange(settings, res)
	if r.c.DeliverHook(np[1], nil) {
		t.Fatal("a hook of a disconnected plugin still works")
	}
}

// TestRubiHome: the user pairs a Rubi Home helper by pasting its code into the panel; a plugin then
// reaches it, but only for what its manifest declares.
func TestRubiHome(t *testing.T) {
	reg := plugintest.New(t)
	reg.Review("v1.0.0", reg.Publish("v1.0.0", nil, nil), "")
	r := newRig(t)
	r.approve(r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}), "install")
	r.connectDemo()
	r.waitFor("plugin", func() bool { return r.ag.call("demo_ping", nil)["started"] == true })

	rl := relaytest.New()
	defer rl.Close()
	h, err := home.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = h.Update(func(c *home.Config) { c.Relays = []string{rl.URL()} })
	h.Shortcuts = func(ctx context.Context, args ...string) ([]byte, error) {
		return []byte("Lights off (11111111-2222-3333-4444-555555555555)\n"), nil
	}
	key, _ := h.RelayKey()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan int, 4)
	rpc := &homeproto.Server{Key: h.ID.Box, Handle: h.Handle}
	go (&relay.Server{Key: key, Relays: h.Relays(), Handle: rpc.HandleRPC, Ready: func(n int) { ready <- n }}).Run(ctx)
	<-ready

	if out := r.ag.call("demo_home", map[string]any{"op": "shortcuts.list"}); out["code"] != float64(rubiplugin.CodeHomeNotPaired) {
		t.Fatalf("before pairing: %v", out)
	}
	code, _ := h.StartPairing()
	settings := r.panel("settings")
	var res map[string]any
	if err := settings.Call("device.pair", map[string]any{"code": code.String()}, &res); err != nil {
		t.Fatal(err)
	}
	r.approveChange(settings, res)
	var devs struct{ Devices []map[string]any }
	_ = settings.Call("devices.get", nil, &devs)
	if len(devs.Devices) != 1 {
		t.Fatalf("devices: %v", devs)
	}
	out := r.ag.call("demo_home", map[string]any{"op": "shortcuts.list"})
	if !strings.Contains(toString(out["out"]), "Lights off") {
		t.Fatalf("shortcuts.list: %v", out)
	}
	if out := r.ag.call("demo_home", map[string]any{"op": "hue.discover"}); !strings.Contains(toString(out["error"]), "doesn't allow") {
		t.Fatalf("an undeclared capability: %v", out)
	}
	var check map[string]any
	if err := settings.Call("device.check", map[string]any{"id": devs.Devices[0]["id"]}, &check); err != nil || check["online"] != true {
		t.Fatalf("check: %v %v", check, err)
	}
	// Removing it also unpairs it on the helper.
	if err := settings.Call("device.remove", map[string]any{"id": devs.Devices[0]["id"]}, &res); err != nil {
		t.Fatal(err)
	}
	r.approveChange(settings, res)
	r.waitFor("unpaired", func() bool { return len(h.Config().Paired) == 0 })
	if out := r.ag.call("demo_home", map[string]any{"op": "shortcuts.list"}); out["code"] != float64(rubiplugin.CodeHomeNotPaired) {
		t.Fatalf("after removing: %v", out)
	}
}

// TestInternalSecrets: a plugin may replace its own internal secrets, never what the user entered.
func TestInternalSecrets(t *testing.T) {
	reg := plugintest.New(t)
	reg.Review("v1.0.0", reg.Publish("v1.0.0", nil, nil), "")
	r := newRig(t)
	r.approve(r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}), "install")
	r.connectDemo()
	r.waitFor("plugin", func() bool { return r.ag.call("demo_ping", nil)["started"] == true })
	out := r.ag.call("demo_session", map[string]any{"value": "s2"})
	if out["before"] != "" || out["after"] != "s2" || out["token_refused"] != true || out["token"] != "good" {
		t.Fatalf("secrets: %v", out)
	}
}

// TestSettingsFromAnyLink: a link made for another purpose (here unlocking) can open the settings.
func TestSettingsFromAnyLink(t *testing.T) {
	r := newRig(t)
	unlock := r.panel("unlock")
	var res struct{ Ticket string }
	if err := unlock.Call("session.settings", nil, &res); err != nil || res.Ticket == "" {
		t.Fatalf("session.settings: %v %v", err, res)
	}
	if err := unlock.Call("store.list", nil, nil); err == nil {
		t.Fatal("an unlock link reached the settings directly")
	}
	link := unlock.Link
	link.Ticket = res.Ticket
	settings, err := panelclient.New(link)
	if err != nil {
		t.Fatal(err)
	}
	settings.HTTP = unlock.HTTP
	if err := settings.Call("store.list", nil, nil); err != nil {
		t.Fatalf("settings with the new ticket: %v", err)
	}

	// From a narrower link, settings open for viewing (and changes that ask for approval), but the few
	// changes that need no approval stay with settings links.
	narrow := r.panel("agent:Mail")
	var vres struct {
		Ticket   string
		ViewOnly bool `json:"view_only"`
	}
	if err := narrow.Call("session.settings", nil, &vres); err != nil || !vres.ViewOnly {
		t.Fatalf("narrow session.settings: %v %+v", err, vres)
	}
	vlink := narrow.Link
	vlink.Ticket = vres.Ticket
	view, _ := panelclient.New(vlink)
	view.HTTP = narrow.HTTP
	if err := view.Call("store.list", nil, nil); err != nil {
		t.Fatalf("view-only settings can't list the store: %v", err)
	}
	for _, op := range []string{"agent.subscribe", "updates.notify"} {
		if err := view.Call(op, map[string]any{"name": "Mail", "id": "rubi", "notify": false, "sources": []string{}}, nil); err == nil {
			t.Fatalf("%s allowed from a view-only session", op)
		}
	}
}

// grantAccess asks for an access code for agent (approving if needed), as a Bot would, and returns a
// function that calls plugin tools with it.
func grantAccess(t *testing.T, r *rig, agent string, ch chan hookHit) func(string, map[string]any) map[string]any {
	t.Helper()
	out := r.ag.call("rubi_access", map[string]any{"agent": agent, "plugin": "demo", "task": "test", "minutes": 60})
	if out["status"] == "awaiting_approval" {
		r.approve(out, "allow")
	} else if out["status"] != "sent" {
		t.Fatalf("access for %s: %v", agent, out)
	}
	code := strings.Trim(toString(waitHit(t, ch, "rubi.access.granted").data["access_code"]), `"`)
	return func(tool string, args map[string]any) map[string]any {
		if args == nil {
			args = map[string]any{}
		}
		args["rubi_agent"], args["rubi_access"] = agent, code
		return r.ag.call(tool, args)
	}
}
