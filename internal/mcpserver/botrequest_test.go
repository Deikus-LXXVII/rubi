package mcpserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/plugins/plugintest"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const botKey = "crsr_fullstack_never_shown_42"

// TestBotProposesItsOwnConnection: a Bot proposes its webhook and plugin access over MCP; nothing changes
// without the user's passkey, both requests become one approval, the user can leave items out, the key
// never comes back, and the store can be searched.
func TestBotProposesItsOwnConnection(t *testing.T) {
	reg := plugintest.New(t)
	reg.Review("v1.0.0", reg.Publish("v1.0.0", nil, nil), "")
	r := newRig(t)
	r.approve(r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}), "install")
	r.connectDemo()

	// Store search: read-only, signed catalog, says what the plugin needs and what to do next.
	found := r.ag.call("rubi_store", map[string]any{"query": "DEMO"})
	list, _ := found["plugins"].([]any)
	if len(list) != 1 {
		t.Fatalf("search: %v", found)
	}
	p := list[0].(map[string]any)
	if p["id"] != "demo" || p["installed"] != true || p["latest"] != "v1.0.0" || p["needs"] == nil ||
		!strings.Contains(fmt.Sprint(p["next_step"]), "rubi_access_request") {
		t.Fatalf("search result: %v", p)
	}
	if none := r.ag.call("rubi_store", map[string]any{"query": "no-such-plugin-anywhere"}); len(none["plugins"].([]any)) != 0 {
		t.Fatalf("search without matches: %v", none)
	}

	srv, ch := hookServer(t)
	first := r.ag.call("rubi_agent_connect", map[string]any{"name": "Mail", "url": srv.URL, "key": botKey, "reason": "to hear about mail"})
	second := r.ag.call("rubi_access_request", map[string]any{"agent": "Mail", "plugins": []map[string]any{{"id": "demo"}}, "reason": "to read it"})
	for _, out := range []map[string]any{first, second} {
		if b, _ := json.Marshal(out); strings.Contains(string(b), botKey) {
			t.Fatal("a tool result carries the key")
		}
	}
	if first["status"] != "awaiting_approval" || first["level"] != "strong" || second["merged"] != first["approval_id"] {
		t.Fatalf("propose: %v / %v", first, second)
	}
	id := second["approval_id"].(string)
	pc := r.panelLink(second["approval_url"].(string))
	info, err := pc.ApprovalInfo(id)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(info); strings.Contains(string(b), botKey) {
		t.Fatal("the panel shows the key")
	}
	var keys []string
	for _, it := range info.Approval.Items {
		keys = append(keys, it.Key)
	}
	if len(keys) != 2 || keys[0] != "webhook" || !strings.HasPrefix(keys[1], "access:demo|") {
		t.Fatalf("items: %v", keys)
	}
	if _, err := pc.ApproveWithPassword(id, "items:webhook", "wrong password"); err == nil {
		t.Fatal("approved without the password")
	}
	if hasAgent(r, "Mail") {
		t.Fatal("connected without approval")
	}

	// The user leaves the access out.
	res, err := pc.ApproveWithPassword(id, "items:webhook", pw)
	if err != nil || res["state"] != "executed" {
		t.Fatalf("approve: %v %v", res, err)
	}
	if h := waitHit(t, ch, "rubi.webhook.test"); h.auth != "Bearer "+botKey {
		t.Fatal("the test event doesn't carry the proposed key")
	}
	if !hasAgent(r, "Mail") || slices.Contains(demoAgents(r), "Mail") {
		t.Fatalf("after a partial approval: agent %v, demo agents %v", hasAgent(r, "Mail"), demoAgents(r))
	}

	// Asked again later, access alone: a separate approval, and it applies only when approved.
	again := r.ag.call("rubi_access_request", map[string]any{"agent": "Mail", "plugins": []map[string]any{{"id": "demo"}}, "reason": "now"})
	if again["merged"] != nil {
		t.Fatalf("merged into a finished request: %v", again)
	}
	r.approve(again, "apply")
	if !slices.Contains(demoAgents(r), "Mail") {
		t.Fatalf("access not given: %v", demoAgents(r))
	}
}

func hasAgent(r *rig, name string) bool {
	for _, a := range r.c.Agents() {
		if a.Name == name {
			return true
		}
	}
	return false
}

func demoAgents(r *rig) []string {
	var out []string
	_ = r.c.Vault.View(func(d *vault.Data) error {
		if i := d.Integrations["demo"]; i != nil {
			for _, a := range i.Accounts {
				out = append(out, a.Agents...)
			}
		}
		return nil
	})
	return out
}

// TestPluginsAsStringsOverMCP: plugins may be bare id strings, objects or a mix; other types are refused
// by the tool's schema.
func TestPluginsAsStringsOverMCP(t *testing.T) {
	reg := plugintest.New(t)
	reg.Review("v1.0.0", reg.Publish("v1.0.0", nil, nil), "")
	r := newRig(t)
	r.approve(r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}), "install")
	r.connectDemo()
	srv, _ := hookServer(t)
	for _, plugins := range [][]any{{"demo"}, {map[string]any{"id": "demo"}}, {"demo", map[string]any{"id": "demo"}}} {
		out := r.ag.call("rubi_agent_connect", map[string]any{"name": "Mail" + fmt.Sprint(len(plugins)), "url": srv.URL, "key": botKey, "plugins": plugins})
		if out["status"] != "awaiting_approval" {
			t.Fatalf("%v: %v", plugins, out)
		}
	}
	for _, bad := range [][]any{{5}, {true}, {[]any{"demo"}}, {map[string]any{"id": 3}}} {
		res, err := r.ag.ss.CallTool(context.Background(), &mcp.CallToolParams{Name: "rubi_agent_connect",
			Arguments: map[string]any{"name": "Bad", "url": srv.URL, "key": botKey, "plugins": bad}})
		if err == nil && !res.IsError {
			t.Fatalf("%v accepted", bad)
		}
	}
}
