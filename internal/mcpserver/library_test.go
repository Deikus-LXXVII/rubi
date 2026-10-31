package mcpserver_test

import (
	"encoding/json"
	"strings"
	"testing"
)

const toolSkill = "---\nname: layout-notes\ndescription: How to lay out notes.\n---\n# Layout\nUse short headings.\n"

// The library tools: any Bot submits, only the curator the user named admits, and nothing is served before.
func TestLibraryTools(t *testing.T) {
	r := newRig(t)
	for _, name := range []string{"rubi_library_list", "rubi_library_get", "rubi_library_submit", "rubi_library_admit",
		"rubi_library_update", "rubi_library_deprecate", "rubi_library_withdraw", "rubi_library_purge"} {
		found := false
		for _, n := range r.ag.tools() {
			found = found || n == name
		}
		if !found {
			t.Fatalf("tool %s is not registered", name)
		}
	}
	bot := r.c.Agents()[0].Name
	auth := func(m map[string]any) map[string]any {
		m["agent"], m["code"] = bot, r.agentCode(bot)
		return m
	}
	if out := r.ag.call("rubi_library_submit", map[string]any{"description": "d", "content": toolSkill}); !strings.Contains(toString(out["tool_error"]), "agent") {
		t.Fatalf("anonymous submit: %v", out)
	}
	sub := r.ag.call("rubi_library_submit", auth(map[string]any{"description": "How to lay out notes", "tags": []string{"notes"}, "content": toolSkill}))
	if sub["proposal_id"] != "layout-notes@1" || sub["state"] != "proposed" || sub["scan"] == nil {
		t.Fatalf("submit: %v", sub)
	}
	if out := r.ag.call("rubi_library_admit", auth(map[string]any{"proposal_id": "layout-notes@1"})); !strings.Contains(toString(out["tool_error"]), "curator") {
		t.Fatalf("admit with no curator: %v", out)
	}
	if out := r.ag.call("rubi_library_get", map[string]any{"id": "layout-notes"}); out["tool_error"] == nil {
		t.Fatalf("a proposal was served: %v", out)
	}
	if out := r.ag.call("rubi_library_list", nil); len(out["skills"].([]any)) != 0 {
		t.Fatalf("a proposal is listed: %v", out)
	}
	// The user names the curator, with the passkey or password in the panel.
	res, err := r.c.RequestLibraryCurator(t.Context(), bot)
	if err != nil {
		t.Fatal(err)
	}
	var plain map[string]any // as the agent would see it
	b, _ := json.Marshal(res)
	_ = json.Unmarshal(b, &plain)
	r.approve(plain, "apply")
	if r.c.LibraryCurator() != bot {
		t.Fatal("curator not set")
	}
	adm := r.ag.call("rubi_library_admit", auth(map[string]any{"proposal_id": "layout-notes@1"}))
	a, _ := adm["admitted"].(map[string]any)
	if a["state"] != "admitted" || a["self_admitted"] != true {
		t.Fatalf("admit: %v", adm)
	}
	got := r.ag.call("rubi_library_get", map[string]any{"id": "layout-notes"})
	sk, _ := got["skill"].(map[string]any)
	if sk["content"] != toolSkill || sk["signature"] == "" || sk["self_admitted"] != true {
		t.Fatalf("get: %v", got)
	}
	if out := r.ag.call("rubi_library_list", map[string]any{"query": "NOTES"}); len(out["skills"].([]any)) != 1 {
		t.Fatalf("list: %v", out)
	}
	if out := r.ag.call("rubi_library_list", map[string]any{"tag": "nothing"}); len(out["skills"].([]any)) != 0 {
		t.Fatalf("list by tag: %v", out)
	}
	up := r.ag.call("rubi_library_update", auth(map[string]any{"id": "layout-notes", "description": "d", "content": toolSkill + "More.\n"}))
	if u, _ := up["admitted"].(map[string]any); u["version"] != float64(2) {
		t.Fatalf("update: %v", up)
	}
	if out := r.ag.call("rubi_library_deprecate", auth(map[string]any{"id": "layout-notes", "version": 2})); out["state"] != "deprecated" {
		t.Fatalf("deprecate: %v", out)
	}
	if out := r.ag.call("rubi_library_withdraw", auth(map[string]any{"id": "layout-notes"})); out["state"] != "withdrawn" {
		t.Fatalf("withdraw: %v", out)
	}
	if out := r.ag.call("rubi_library_get", map[string]any{"id": "layout-notes"}); out["tool_error"] == nil {
		t.Fatalf("withdrawn skill served: %v", out)
	}
}
