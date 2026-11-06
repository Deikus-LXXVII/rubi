package core

import (
	"context"
	"testing"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
)

// Project rule 5 (docs/project-rules.md): the fewest possible user actions. Everything one Bot asks for while a request
// of its own still waits joins that request, so the user approves once, whatever the number of asks. (The merge itself
// is also covered by TestBotRequestsMergeAndPartialApproval; this test counts what the user would have to open.)
func TestSeveralAsksOfOneBotAreOneApproval(t *testing.T) {
	c, hook, _ := botCore(t)
	ctx := context.Background()
	pendingForUser := func() int {
		n := 0
		for _, a := range c.Approvals.Pending() {
			if a.Level.InPanel() {
				n++
			}
		}
		return n
	}

	first := propose(t, c, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: testHookKey}, Reason: "connect"})
	second := propose(t, c, BotRequest{Agent: "Mail", Access: []PluginAccessWant{{Plugin: "demo-mail", Accounts: []string{"ann@example.com"}}}})
	third := propose(t, c, BotRequest{Agent: "Mail", Access: []PluginAccessWant{{Plugin: "demo-mail", Accounts: []string{"bob@example.com"}}}})
	if second["merged"] != first["approval_id"] || third["merged"] == nil {
		t.Fatalf("the later asks did not join the waiting request: %v %v", second, third)
	}
	if n := pendingForUser(); n != 1 {
		t.Fatalf("%d requests wait for the user; want 1 for three asks of one Bot", n)
	}
	id := third["approval_id"].(string)
	det, _ := c.Approvals.Detail(id)
	if len(det.Items) != 3 { // the webhook and the two mailboxes
		t.Fatalf("items: %+v", det.Items)
	}
	// one proof covers the webhook and both mailboxes
	snap, err := c.Approvals.DecidePanel(ctx, id, "apply", true, approvals.Verified)
	if err != nil || snap.State != approvals.Executed {
		t.Fatalf("approve: %+v %v", snap, err)
	}
	if got := assigned(c, "ann@example.com"); len(got) != 1 {
		t.Fatalf("ann: %v", got)
	}
	if got := assigned(c, "bob@example.com"); len(got) != 1 {
		t.Fatalf("bob: %v", got)
	}
	if n := pendingForUser(); n != 0 {
		t.Fatalf("%d requests still wait", n)
	}
	waitGone(t, c, "Mail")
}
