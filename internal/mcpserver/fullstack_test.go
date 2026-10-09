package mcpserver_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Deikus-LXXVII/rubi/internal/core"
	"github.com/Deikus-LXXVII/rubi/internal/mcpserver"
	"github.com/Deikus-LXXVII/rubi/internal/panelapi"
	"github.com/Deikus-LXXVII/rubi/internal/panelclient"
	"github.com/Deikus-LXXVII/rubi/internal/paths"
	"github.com/Deikus-LXXVII/rubi/internal/plugins/plugintest"
)

const pw = "correct horse battery"

type agent struct {
	t  *testing.T
	ss *mcp.ClientSession
}

func (a agent) call(name string, args map[string]any) map[string]any {
	a.t.Helper()
	res, err := a.ss.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		a.t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		return map[string]any{"tool_error": res.Content[0].(*mcp.TextContent).Text}
	}
	var out map[string]any
	b, _ := json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(b, &out)
	return out
}

func (a agent) tools() []string {
	a.t.Helper()
	res, err := a.ss.ListTools(context.Background(), nil)
	if err != nil {
		a.t.Fatal(err)
	}
	var names []string
	for _, t := range res.Tools {
		names = append(names, t.Name)
	}
	return names
}

type rig struct {
	t  *testing.T
	c  *core.Core
	ag agent
}

func newRig(t *testing.T) *rig {
	t.Helper()
	c, err := core.Open(paths.Layout{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Lock)
	api := httptest.NewServer(panelapi.New(c).Handler())
	t.Cleanup(api.Close)
	c.SetEndpoint(api.URL)
	ct, st := mcp.NewInMemoryTransports()
	srv := mcpserver.New(c)
	go func() { _ = srv.MCP().Run(context.Background(), st) }()
	ss, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "test"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, c: c, ag: agent{t: t, ss: ss}}
	if _, err := r.panel("pair").PairWithPassword(pw); err != nil {
		t.Fatal(err)
	}
	// Setup isn't finished without the agent webhook.
	if out := r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}); !strings.Contains(toString(out["tool_error"]), "agent webhook") {
		t.Fatalf("install before the webhook: %v", out)
	}
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {}))
	t.Cleanup(sink.Close)
	r.setWebhook(sink.URL)
	return r
}

func (r *rig) setWebhook(url string) {
	r.t.Helper()
	settings := r.panel("settings")
	var res map[string]any
	if err := settings.Call("webhook.set", map[string]any{"url": url, "key": "crsr_test"}, &res); err != nil {
		r.t.Fatal(err)
	}
	if out := r.approveChange(settings, res); out["state"] != "executed" {
		r.t.Fatalf("webhook: %v", out)
	}
}

func (r *rig) panel(purpose string) *panelclient.Client {
	r.t.Helper()
	link, err := r.c.Link(purpose)
	if err != nil {
		r.t.Fatal(err)
	}
	return r.panelLink(link)
}

func (r *rig) panelLink(link string) *panelclient.Client {
	r.t.Helper()
	l, err := panelclient.ParseLink(link)
	if err != nil {
		r.t.Fatal(err)
	}
	pc, err := panelclient.New(l)
	if err != nil {
		r.t.Fatal(err)
	}
	return pc
}

// approve approves an awaiting_approval tool result in the panel and returns the approval's result.
func (r *rig) approve(out map[string]any, option string) map[string]any {
	r.t.Helper()
	if out["status"] != "awaiting_approval" || out["level"] != "strong" {
		r.t.Fatalf("expected a strong approval, got %v", out)
	}
	id := out["approval_id"].(string)
	res, err := r.panelLink(out["approval_url"].(string)).ApproveWithPassword(id, option, pw)
	if err != nil || res["state"] != "executed" {
		r.t.Fatalf("approve %s: %v %v", id, res, err)
	}
	snap := r.ag.call("rubi_approval", map[string]any{"approval_id": id})["approval"].(map[string]any)
	result, _ := snap["result"].(map[string]any)
	return result
}

// approveChange approves a settings change returned by a panel operation.
func (r *rig) approveChange(base *panelclient.Client, res map[string]any) map[string]any {
	r.t.Helper()
	pc := *base
	pc.Link.Ticket = res["ticket"].(string)
	out, err := pc.ApproveWithPassword(res["approval_id"].(string), firstOption(r.t, &pc, res["approval_id"].(string)), pw)
	if err != nil {
		r.t.Fatal(err)
	}
	return out
}

func firstOption(t *testing.T, pc *panelclient.Client, id string) string {
	info, err := pc.ApprovalInfo(id)
	if err != nil {
		t.Fatal(err)
	}
	return info.Approval.Options[0].Key
}

func (r *rig) connectDemo() {
	r.t.Helper()
	setup := r.panel("setup:demo")
	var res map[string]any
	if err := setup.Call("integration.setup", map[string]any{"id": "demo", "fields": map[string]string{"user": "ann"},
		"secrets": map[string]string{"token": "bad"}}, &res); err == nil || !strings.Contains(err.Error(), "wrong token") {
		r.t.Fatalf("setup with a bad token: %v", err)
	}
	if err := setup.Call("integration.setup", map[string]any{"id": "demo", "fields": map[string]string{"user": "ann"},
		"secrets": map[string]string{"token": "good", "other": "x"}}, &res); err != nil {
		r.t.Fatal(err)
	}
	if out := r.approveChange(setup, res); out["state"] != "executed" {
		r.t.Fatalf("connect: %v", out)
	}
}

func (r *rig) waitFor(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (r *rig) events() []map[string]any {
	var out []map[string]any
	for _, e := range r.ag.call("rubi_events", nil)["events"].([]any) {
		out = append(out, e.(map[string]any))
	}
	return out
}

func TestMarketplaceFullStack(t *testing.T) {
	reg := plugintest.New(t)
	sums := reg.Publish("v1.0.0", nil, nil)
	reg.Review("v1.0.0", sums, "")
	r := newRig(t)
	ag := r.ag

	// A bare Rubi: no plugin tools; the store lists the reviewed plugin.
	if slices.Contains(ag.tools(), "demo_echo") {
		t.Fatal("plugin tool listed before install")
	}
	store := ag.call("rubi_store", nil)
	list := store["plugins"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != "demo" || list[0].(map[string]any)["installed"] != false {
		t.Fatalf("store: %v", store)
	}

	// Install: verified first, then a strong approval with the plugin's permissions.
	out := ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"})
	info, err := r.panelLink(out["approval_url"].(string)).ApprovalInfo(out["approval_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	preview := previewOf(t, info)
	if preview["review"] != "Reviewed by Rubi-Project" || preview["will_ask_for"] != "User, Token" ||
		!strings.Contains(preview["can"].(string), "Send (Passkey / password)") || preview["connects_to"] != "example.com:443" {
		t.Fatalf("install preview: %v", preview)
	}
	if res := ag.call("rubi_confirm", map[string]any{"approval_id": out["approval_id"], "user_response": "Install"}); res["tool_error"] == nil {
		t.Fatal("the agent approved an install itself")
	}
	res := r.approve(out, "install")
	if res["version"] != "v1.0.0" || !strings.Contains(res["next_step"].(string), "setup:demo") {
		t.Fatalf("install result: %v", res)
	}
	r.waitFor("tools list update", func() bool { return slices.Contains(ag.tools(), "demo_echo") })

	// Installed but not connected: the agent gets the setup link.
	if out := ag.call("demo_echo", map[string]any{"text": "hi"}); out["status"] != "not_connected" || out["needs"] != "A token." {
		t.Fatalf("before setup: %v", out)
	}
	r.connectDemo()

	// Free actions run right away; secrets are confined to what the manifest declares.
	r.waitFor("plugin start", func() bool { return ag.call("demo_ping", nil)["started"] == true })
	if out := ag.call("demo_echo", map[string]any{"text": "hi"}); out["echo"] != "hi" || out["user"] != "ann" {
		t.Fatalf("echo: %v", out)
	}
	if out := ag.call("rubi_call", map[string]any{"tool": "demo_echo", "arguments": map[string]any{"text": "via call"}}); out["echo"] != "via call" {
		t.Fatalf("rubi_call: %v", out)
	}
	if out := ag.call("demo_secret", nil); out["token_ok"] != true || out["undeclared_refused"] != true {
		t.Fatalf("secrets: %v", out)
	}

	// Gated actions execute in the plugin with the submitted payload, after the user approves.
	out = ag.call("demo_send", map[string]any{"to": "bob"})
	if res := r.approve(out, "send"); res["sent_to"] != "bob" || res["total"] != float64(1) {
		t.Fatalf("send: %v", res)
	}

	// Events carry the manifest's untrusted fields.
	ag.call("demo_ping", nil)
	found := false
	for _, e := range r.events() {
		if e["type"] == "ping" && e["integration"] == "demo" {
			found = slices.Contains(toStrings(e["untrusted_fields"]), "from")
		}
	}
	if !found {
		t.Fatalf("ping event: %v", r.events())
	}

	// A crashed plugin is restarted.
	ag.call("demo_crash", nil)
	r.waitFor("restart after crash", func() bool { return ag.call("demo_echo", map[string]any{"text": "back"})["echo"] == "back" })

	// A new reviewed version: announced once, updated with an approval that lists new permissions.
	sums2 := reg.Publish("v1.1.0", nil, func(m map[string]any) { m["egress"] = []string{"example.com:443", "api.example.net:443"} })
	reg.Review("v1.1.0", sums2, "")
	r.c.CheckPluginUpdates(context.Background())
	r.c.CheckPluginUpdates(context.Background())
	n := 0
	for _, e := range r.events() {
		if e["type"] == "plugin.update_available" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("update announcements: %d", n)
	}
	out = ag.call("rubi_plugin_update", map[string]any{"id": "demo"})
	info, _ = r.panelLink(out["approval_url"].(string)).ApprovalInfo(out["approval_id"].(string))
	if np, _ := previewOf(t, info)["new_permissions"].(string); np != "connects to api.example.net:443" {
		t.Fatalf("update preview: %s", info.Approval.Preview)
	}
	if res := r.approve(out, "install"); res["version"] != "v1.1.0" {
		t.Fatalf("update result: %v", res)
	}
	// Settings and state survive the update.
	r.waitFor("plugin after update", func() bool { return ag.call("demo_echo", map[string]any{"text": "x"})["user"] == "ann" })
	out = ag.call("demo_send", map[string]any{"to": "carol"})
	if res := r.approve(out, "send"); res["total"] != float64(2) {
		t.Fatalf("state after update: %v", res)
	}

	// Rollback: back to v1.0.0 (its tools and files), then forward again; state stays.
	out = ag.call("rubi_plugin_rollback", map[string]any{"id": "demo"})
	if res := r.approve(out, "rollback"); res["version"] != "v1.0.0" {
		t.Fatalf("rollback: %v", res)
	}
	r.waitFor("plugin after rollback", func() bool { return ag.call("demo_echo", map[string]any{"text": "x"})["user"] == "ann" })
	if st := ag.call("rubi_status", nil)["plugins"].([]any)[0].(map[string]any); st["version"] != "v1.0.0" {
		t.Fatalf("status after rollback: %v", st)
	}
	out = ag.call("rubi_plugin_rollback", map[string]any{"id": "demo"})
	if res := r.approve(out, "rollback"); res["version"] != "v1.1.0" {
		t.Fatalf("roll forward: %v", res)
	}
	r.waitFor("plugin after roll forward", func() bool { return ag.call("demo_echo", map[string]any{"text": "x"})["user"] == "ann" })

	// Files changed on disk: the plugin doesn't start after the next unlock.
	ag.call("rubi_lock", nil)
	bin := filepath.Join(r.c.Layout.Plugins(), "demo", "v1.1.0", "demo")
	f, _ := os.OpenFile(bin, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.Write([]byte{0})
	f.Close()
	if _, err := r.panel("unlock").UnlockWithPassword(pw, 0); err != nil {
		t.Fatal(err)
	}
	r.waitFor("tamper event", func() bool {
		for _, e := range r.events() {
			if e["type"] == "plugin.failed" && strings.Contains(toString(e["data"]), "modified") {
				return true
			}
		}
		return false
	})
	if out := ag.call("demo_echo", map[string]any{"text": "x"}); out["status"] != "plugin_not_running" {
		t.Fatalf("tampered plugin ran: %v", out)
	}

	// Remove erases everything; the tools disappear.
	out = ag.call("rubi_plugin_remove", map[string]any{"id": "demo"})
	r.approve(out, "remove")
	r.waitFor("tools removed", func() bool { return !slices.Contains(ag.tools(), "demo_echo") })
	if _, err := os.Stat(filepath.Join(r.c.Layout.Plugins(), "demo")); !os.IsNotExist(err) {
		t.Fatalf("plugin files left: %v", err)
	}
	if st := ag.call("rubi_status", nil); st["plugins"] != nil {
		t.Fatalf("status after remove: %v", st)
	}

	// The settings panel can install from the store as well.
	settings := r.panel("settings")
	var pres map[string]any
	if err := settings.Call("plugin.install", map[string]any{"plugin": "demo"}, &pres); err != nil {
		t.Fatal(err)
	}
	if out := r.approveChange(settings, pres); out["state"] != "executed" {
		t.Fatalf("panel install: %v", out)
	}
}

func TestMarketplaceRefusesUnreviewedOrForged(t *testing.T) {
	reg := plugintest.New(t)
	reg.Publish("v1.0.0", nil, nil)
	reg.Review("v1.0.0", strings.Repeat("0", 64), "") // the catalog vouches for different bytes
	r := newRig(t)
	if out := r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}); !strings.Contains(toString(out["tool_error"]), "differs from the reviewed") {
		t.Fatalf("install of an unreviewed build: %v", out)
	}
	// A plugin in the store can't be sideloaded around the review.
	if out := r.ag.call("rubi_plugin_install", map[string]any{"plugin": reg.Source()}); !strings.Contains(toString(out["tool_error"]), "is in the Rubi store") {
		t.Fatalf("sideload of a store plugin: %v", out)
	}
}

func TestSideload(t *testing.T) {
	reg := plugintest.New(t) // empty catalog
	reg.Publish("v1.0.0", nil, nil)
	r := newRig(t)

	out := r.ag.call("rubi_plugin_install", map[string]any{"plugin": reg.Source()})
	info, err := r.panelLink(out["approval_url"].(string)).ApprovalInfo(out["approval_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	p := previewOf(t, info)
	if p["review"] != "Not reviewed by Rubi-Project" || p["warning"] == nil || p["source"] != reg.Source() {
		t.Fatalf("sideload preview: %v", p)
	}
	r.approve(out, "install_quiet") // the user turned update notifications off on the install screen
	if items := r.ag.call("rubi_updates", nil)["updates"].([]any); items[len(items)-1].(map[string]any)["notify"] != false {
		t.Fatalf("notify choice ignored: %v", items)
	}

	// The publisher key is pinned: an update signed by someone else is refused.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	reg.Publish("v1.1.0", other, nil)
	if out := r.ag.call("rubi_plugin_update", map[string]any{"id": "demo"}); !strings.Contains(toString(out["tool_error"]), "different publisher key") {
		t.Fatalf("update with another key: %v", out)
	}
	// A tampered archive is refused.
	reg.Publish("v1.2.0", nil, nil)
	reg.Tamper("v1.2.0")
	if out := r.ag.call("rubi_plugin_update", map[string]any{"id": "demo"}); !strings.Contains(toString(out["tool_error"]), "checksum mismatch") {
		t.Fatalf("tampered update: %v", out)
	}
	reg.Publish("v1.3.0", nil, nil)
	out = r.ag.call("rubi_plugin_update", map[string]any{"id": "demo"})
	if res := r.approve(out, "install"); res["version"] != "v1.3.0" {
		t.Fatalf("sideload update: %v", res)
	}

	// Several at once: what can be updated goes to one approval; the rest is reported.
	reg.Publish("v1.4.0", nil, nil)
	out = r.ag.call("rubi_plugin_update", map[string]any{"ids": []string{"demo", "nope"}})
	if failed, _ := out["failed"].(map[string]any); failed["nope"] == nil || out["approval_url"] == nil {
		t.Fatalf("batch update: %v", out)
	}
	if res := r.approve(out, "install"); res["version"] != "v1.4.0" {
		t.Fatalf("batch update result: %v", res)
	}
	if out := r.ag.call("rubi_plugin_update", map[string]any{"all": true}); out["status"] != "nothing_to_update" {
		t.Fatalf("all, nothing left: %v", out)
	}
}

func previewOf(t *testing.T, info *panelclient.ApprovalInfo) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(info.Approval.Preview, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func toString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}
