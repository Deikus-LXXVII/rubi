package core

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
)

// TestBotConnectWithoutKey: a stock Grok Bot can't read its routine's key. It starts the request anyway;
// the user pastes address and key on the approval screen, and they apply only with the user's proof.
func TestBotConnectWithoutKey(t *testing.T) {
	c, hook, ch := botCore(t)
	out := propose(t, c, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: hook},
		Access: nil, Reason: "to hear about mail"})
	if out["user_pastes_webhook"] != true || !strings.Contains(fmt.Sprint(out["instructions"]), "webhook field") {
		t.Fatalf("propose: %v", out)
	}
	// Folded with an access request into one approval: the paste belongs to the webhook item.
	out2, err := c.ProposeBot(context.Background(), BotRequest{Agent: "Mail",
		Access: []PluginAccessWant{{Plugin: "demo-mail", Accounts: []string{"ann@example.com"}}}})
	if err != nil || out2["merged"] != out["approval_id"] {
		t.Fatalf("merge: %v %v", out2, err)
	}
	id := out2["approval_id"].(string)
	det, _ := c.Approvals.Detail(id)
	if len(det.Inputs) != 2 || det.Inputs[0].Value != hook || det.Inputs[0].Item != webhookItem ||
		!det.Inputs[1].Secret || det.Inputs[1].Value != "" {
		t.Fatalf("inputs: %+v", det.Inputs)
	}
	if p := det.Items[0].Preview.(map[string]any); !strings.Contains(fmt.Sprint(p["webhook"]), "Suggested by the Bot") {
		t.Fatalf("webhook item preview: %v", p)
	}

	ctx := context.Background()
	for _, bad := range []map[string]string{nil, {inputWebhookURL: hook}, {inputWebhookURL: hook, inputWebhookKey: "crsr_a\r\nX: 1"},
		{inputWebhookURL: "http://evil.example/x", inputWebhookKey: testHookKey}} {
		if _, err := c.Approvals.DecidePanelWith(ctx, id, "apply", true, approvals.Verified, bad); err == nil {
			t.Fatalf("approved with bad inputs %v", bad != nil)
		}
	}
	if agentOf(c, "Mail") != nil {
		t.Fatal("applied without valid inputs")
	}
	// The user pastes the key with a trailing newline (as a file or a copy would carry it).
	in := map[string]string{inputWebhookURL: hook, inputWebhookKey: "Bearer " + testHookKey + "\n"}
	snap, err := c.Approvals.DecidePanelWith(ctx, id, "apply", true, approvals.Verified, in)
	if err != nil || snap.State != approvals.Executed {
		t.Fatalf("approve: %+v %v", snap, err)
	}
	if a := agentOf(c, "Mail"); a == nil || a.URL != hook || a.Key != testHookKey {
		t.Fatal("pasted webhook not applied")
	}
	if got := <-ch; got.auth != "Bearer "+testHookKey {
		t.Fatal("the test event doesn't carry the pasted key")
	}
	if a := assigned(c, "ann@example.com"); len(a) != 1 || a[0] != "Mail" {
		t.Fatalf("access: %v", a)
	}
	noKeyIn(t, c, snap, det, out, out2)
	waitGone(t, c, "Mail")
}

// TestBotConnectPasteOnlyWhenChosen: unticking the webhook needs no paste; accounts alone still need a Bot.
func TestBotConnectPasteOnlyWhenChosen(t *testing.T) {
	c, hook, _ := botCore(t)
	// Mail is connected already; now it asks for a new webhook (pasted) and an account.
	first := propose(t, c, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: testHookKey}})
	if s, err := c.Approvals.DecidePanel(context.Background(), first["approval_id"].(string), "apply", true, approvals.Verified); err != nil || s.State != approvals.Executed {
		t.Fatal(err)
	}
	out := propose(t, c, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{},
		Access: []PluginAccessWant{{Plugin: "demo-mail", Accounts: []string{"bob@example.com"}}}})
	id := out["approval_id"].(string)
	det, _ := c.Approvals.Detail(id)
	p := det.Preview.(map[string]any)
	if !strings.Contains(fmt.Sprint(p["replaces"]), "REPLACES") || out["replaces_existing_webhook"] != true {
		t.Fatalf("replacement not marked: %v", p)
	}
	snap, err := c.Approvals.DecidePanelWith(context.Background(), id, "items:access:demo-mail|bob@example.com", true, approvals.Verified, nil)
	if err != nil || snap.State != approvals.Executed {
		t.Fatalf("access only: %+v %v", snap, err)
	}
	if a := agentOf(c, "Mail"); a.URL != hook || a.Key != testHookKey {
		t.Fatal("the webhook changed although it was left out")
	}
}

func TestBotKeyFileReferenceRefused(t *testing.T) {
	c, hook, _ := botCore(t)
	_, err := c.ProposeBot(context.Background(), BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: "$file:/workspace/.rubi-hook-key"}})
	if err == nil || strings.Contains(err.Error(), "$file:/workspace") || !strings.Contains(err.Error(), "key_file") {
		t.Fatalf("a $file: reference was taken as a key: %v", err)
	}
	// A key read from a file ends with a newline: trimmed.
	out := propose(t, c, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: testHookKey + "\n"}})
	if _, err := c.Approvals.DecidePanel(context.Background(), out["approval_id"].(string), "apply", true, approvals.Verified); err != nil {
		t.Fatal(err)
	}
	if a := agentOf(c, "Mail"); a.Key != testHookKey {
		t.Fatal("the trailing newline was kept")
	}
}

// localReceiver lets the tests' httptest receiver stand in for a routine.
func localReceiver(u string) bool { return strings.HasPrefix(u, "http://127.0.0.1:") }

// TestBotMayProposeOnlyRoutineWebhooks: what a Bot proposes (or the user pastes on its approval screen)
// must be a Grok Bot routine webhook; other receivers only through the user's own settings.
func TestBotMayProposeOnlyRoutineWebhooks(t *testing.T) {
	c, local, _ := botCore(t)
	c.BotWebhookOK = nil // as in the daemon
	routine := "https://api2.cursor.sh/automations/webhook/6076a0ff-5b26-405e-82ef-d6173cd96066"
	for _, bad := range []string{local, "https://evil.example/automations/webhook/x",
		"https://api2.cursor.sh/other/path", "https://api2.cursor.sh@evil.example/automations/webhook/x",
		"http://api2.cursor.sh/automations/webhook/x"} {
		for _, key := range []string{testHookKey, ""} {
			if _, err := c.ProposeBot(context.Background(), BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: bad, Key: key}}); err == nil {
				t.Fatalf("a Bot proposed a non-routine webhook (key given: %v)", key != "")
			}
		}
	}
	if agentOf(c, "Mail") != nil || c.pendingBotRequest("Mail") != "" {
		t.Fatal("a refused proposal left something behind")
	}
	out := propose(t, c, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: routine, Key: testHookKey}})
	det, _ := c.Approvals.Detail(out["approval_id"].(string))
	if p := det.Preview.(map[string]any); p["warning"] != nil {
		t.Fatalf("a routine webhook got a warning: %v", p["warning"])
	}
	// Pasted on the approval screen: the same rule.
	out = propose(t, c, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{}})
	id := out["approval_id"].(string)
	if _, err := c.Approvals.CheckInputs(id, "apply", map[string]string{inputWebhookURL: local, inputWebhookKey: testHookKey}); err == nil {
		t.Fatal("a non-routine address was accepted as the pasted webhook")
	}
	if _, err := c.Approvals.CheckInputs(id, "apply", map[string]string{inputWebhookURL: routine, inputWebhookKey: testHookKey}); err != nil {
		t.Fatalf("the routine's own address was refused: %v", err)
	}
}
