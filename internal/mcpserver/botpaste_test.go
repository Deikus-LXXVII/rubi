package mcpserver_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// TestBotConnectsWithoutItsKey: the stock Grok Bot case. The Bot asks without a key, the request waits in
// the panel's "Waiting for you" list, the user pastes address and key on the approval screen, and the
// password/passkey proof covers what was pasted: values swapped after signing are refused.
func TestBotConnectsWithoutItsKey(t *testing.T) {
	r := newRig(t)
	srv, ch := hookServer(t)
	out := r.ag.call("rubi_agent_connect", map[string]any{"name": "Mail", "url": srv.URL, "reason": "to hear about mail"})
	if out["status"] != "awaiting_approval" || out["user_pastes_webhook"] != true {
		t.Fatalf("propose: %v", out)
	}
	id := out["approval_id"].(string)

	var list map[string]any
	if err := r.panel("settings").Call("approval.list", nil, &list); err != nil {
		t.Fatal(err)
	}
	var waiting []string
	for _, a := range list["approvals"].([]any) {
		waiting = append(waiting, a.(map[string]any)["approval_id"].(string))
	}
	if !slices.Contains(waiting, id) {
		t.Fatalf("the Bot request isn't in Waiting for you: %v", waiting)
	}

	pc := r.panelLink(out["approval_url"].(string))
	info, err := pc.ApprovalInfo(id)
	if err != nil {
		t.Fatal(err)
	}
	if in := info.Approval.Inputs; len(in) != 2 || in[0].Value != srv.URL || !in[1].Secret {
		t.Fatalf("inputs: %+v", in)
	}
	if _, err := pc.ApproveWithPassword(id, "apply", pw); err == nil {
		t.Fatal("approved without pasting the key")
	}
	pasted := map[string]string{"webhook_url": srv.URL, "webhook_key": "Authorization: Bearer " + botKey + "\n"}
	swapped := map[string]string{"webhook_url": srv.URL, "webhook_key": "crsr_someone_else_entirely_99"}
	if _, err := pc.ApproveSigningInputs(id, "apply", pw, pasted, swapped); err == nil {
		t.Fatal("inputs changed after signing were accepted")
	}
	if hasAgent(r, "Mail") {
		t.Fatal("connected without a valid approval")
	}
	res, err := pc.ApproveWithPasswordInputs(id, "apply", pw, pasted)
	if err != nil || res["state"] != "executed" {
		t.Fatalf("approve: %v %v", res, err)
	}
	if h := waitHit(t, ch, "rubi.webhook.test"); h.auth != "Bearer "+botKey {
		t.Fatal("the test event doesn't carry the pasted key")
	}
	after, _ := pc.ApprovalInfo(id)
	for _, v := range []any{res, after, out} {
		if b, _ := json.Marshal(v); strings.Contains(string(b), botKey) {
			t.Fatal("the pasted key came back")
		}
	}
}
