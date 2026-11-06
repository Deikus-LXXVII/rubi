package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// Project rule 5, Bot access: a Bot's request for its webhook or for accounts for good (rubi.bot.request) and its
// one-off access (rubi.access: a duration the user picks) are one card and one passkey when both wait. They are not
// merged in substance: the duration the user picks belongs to the one-off items only, and what the Bot asked for to
// keep stays for good.

const (
	permItem = "access:demo-mail|ann@example.com"
	onceItem = "once:demo-mail|bob@example.com"
)

// combinedCard has Mail asking for ann for good and then, while that waits, for bob for a task.
func combinedCard(t *testing.T) (*Core, string) {
	t.Helper()
	c := targetCore(t)
	first := propose(t, c, BotRequest{Agent: "Mail", Access: []PluginAccessWant{{Plugin: "demo-mail", Accounts: []string{"ann@example.com"}}}})
	out, err := c.RequestAccess(context.Background(), "Mail", []AccessResource{{Plugin: "demo-mail", Account: "bob@example.com"}}, "check bob's mail", 60)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := out["approval_id"].(string)
	if id == "" || out["merged"] != first["approval_id"] {
		t.Fatalf("the one-off access did not join the waiting request: %v", out)
	}
	return c, id
}

func approveCard(t *testing.T, c *Core, id, option string) {
	t.Helper()
	snap, err := c.Approvals.DecidePanel(context.Background(), id, option, true, approvals.Verified)
	if err != nil || snap.State != approvals.Executed {
		t.Fatalf("approve %s: %+v %v", option, snap, err)
	}
}

// accountState is what the vault says about a Bot and an account: assigned for good, and until when for a while.
func accountState(c *Core, agent, account string) (permanent bool, until time.Time) {
	_ = c.Vault.View(func(d *vault.Data) error {
		a, _ := accountOf(d, "demo-mail", account)
		for _, n := range a.Agents {
			permanent = permanent || strings.EqualFold(n, agent)
		}
		until = a.TempAgents[agent]
		return nil
	})
	return
}

func grantOf(c *Core, agent, account string) (accessGrant, bool) {
	c.access.mu.Lock()
	defer c.access.mu.Unlock()
	for _, g := range c.access.grants {
		if g.agent == agent && g.resources["demo-mail|"+account] {
			return g, true
		}
	}
	return accessGrant{}, false
}

func near(t time.Time, d time.Duration) bool {
	return time.Until(t) > d-time.Minute && time.Until(t) <= d+time.Minute
}

func TestOneOffAccessInTheCombinedCardGetsExactlyTheChosenDuration(t *testing.T) {
	c, id := combinedCard(t)
	det, _ := c.Approvals.Detail(id)
	if det.Kind != botRequestKind || len(det.Items) != 2 || det.Items[0].Key != permItem || det.Items[1].Key != onceItem || len(det.Options) != 4 {
		t.Fatalf("the card: %s items %+v options %+v", det.Kind, det.Items, det.Options)
	}
	if !strings.Contains(det.Items[1].Label, "for the time you choose") || strings.Contains(det.Items[0].Label, "for the time you choose") {
		t.Fatalf("labels do not tell the two apart: %+v", det.Items)
	}
	approveCard(t, c, id, "day|items:"+permItem+","+onceItem)

	// bob: a day, then gone. Never assigned for good.
	perm, until := accountState(c, "Mail", "bob@example.com")
	if perm || !near(until, 24*time.Hour) {
		t.Fatalf("bob: permanent %v, until %v; want a day and not for good", perm, until)
	}
	g, ok := grantOf(c, "Mail", "bob@example.com")
	if !ok || !near(g.expires, 24*time.Hour) {
		t.Fatalf("the code for bob: %v %+v; it must end with the day", ok, g)
	}
	// the day over: neither the code nor the assignment holds
	later := time.Now().Add(25 * time.Hour)
	_ = c.Vault.View(func(d *vault.Data) error {
		a, _ := accountOf(d, "demo-mail", "bob@example.com")
		if a.AgentAllowed("Mail", later) {
			t.Error("bob is still allowed after the day")
		}
		return nil
	})
	if later.Before(g.expires) {
		t.Error("the code outlives the day")
	}
}

func TestOneOffAccessForThisTaskLastsTheTaskOnly(t *testing.T) {
	c, id := combinedCard(t)
	approveCard(t, c, id, "allow|items:"+permItem+","+onceItem)
	perm, until := accountState(c, "Mail", "bob@example.com")
	g, ok := grantOf(c, "Mail", "bob@example.com")
	if perm || !until.IsZero() || !ok || !near(g.expires, time.Hour) {
		t.Fatalf("bob for this task: permanent %v, until %v, code %v %+v", perm, until, ok, g)
	}
}

func TestPermanentAssignmentInTheCombinedCardStaysPermanent(t *testing.T) {
	c, id := combinedCard(t)
	// whichever duration the user picks for the one-off item, ann is for good, with no expiry
	approveCard(t, c, id, "allow|items:"+permItem+","+onceItem)
	perm, until := accountState(c, "Mail", "ann@example.com")
	if !perm || !until.IsZero() {
		t.Fatalf("ann: permanent %v, until %v; want for good and no expiry", perm, until)
	}
}

func TestUntickingOneItemLeavesTheOthersMeaningAlone(t *testing.T) {
	// the one-off item unticked: ann for good, bob nothing at all, whatever duration is picked
	c, id := combinedCard(t)
	approveCard(t, c, id, "week|items:"+permItem)
	perm, until := accountState(c, "Mail", "ann@example.com")
	bobPerm, bobUntil := accountState(c, "Mail", "bob@example.com")
	if _, ok := grantOf(c, "Mail", "bob@example.com"); !perm || !until.IsZero() || bobPerm || !bobUntil.IsZero() || ok {
		t.Fatalf("ann %v %v, bob %v %v code %v", perm, until, bobPerm, bobUntil, ok)
	}
	// the permanent item unticked: bob for a week, ann nothing
	c, id = combinedCard(t)
	approveCard(t, c, id, "week|items:"+onceItem)
	perm, until = accountState(c, "Mail", "ann@example.com")
	bobPerm, bobUntil = accountState(c, "Mail", "bob@example.com")
	if perm || !until.IsZero() || bobPerm || !near(bobUntil, 7*24*time.Hour) {
		t.Fatalf("ann %v %v, bob %v %v", perm, until, bobPerm, bobUntil)
	}
}

func TestOneOffAccessFirstThenTheBotRequestJoinsIt(t *testing.T) {
	c := targetCore(t)
	ctx := context.Background()
	out, err := c.RequestAccess(ctx, "Mail", []AccessResource{{Plugin: "demo-mail", Account: "bob@example.com"}}, "check bob's mail", 60)
	if err != nil {
		t.Fatal(err)
	}
	first := out["approval_id"].(string)
	if det, _ := c.Approvals.Detail(first); det.Kind != "rubi.access" || len(det.Options) != 4 {
		t.Fatalf("a one-off access on its own changed: %+v", det)
	}
	res := propose(t, c, BotRequest{Agent: "Mail", Access: []PluginAccessWant{{Plugin: "demo-mail", Accounts: []string{"ann@example.com"}}}})
	if res["merged_access"] != first {
		t.Fatalf("the Bot request did not take in the waiting access: %v", res)
	}
	if s, _ := c.Approvals.Get(first); s.State != approvals.Cancelled {
		t.Fatalf("the access approval still waits beside the card: %+v", s)
	}
	det, _ := c.Approvals.Detail(res["approval_id"].(string))
	if len(det.Items) != 2 || det.Items[1].Key != onceItem {
		t.Fatalf("items: %+v", det.Items)
	}
	approveCard(t, c, res["approval_id"].(string), "day|items:"+permItem+","+onceItem)
	if perm, _ := accountState(c, "Mail", "ann@example.com"); !perm {
		t.Fatal("ann is not assigned")
	}
	if perm, until := accountState(c, "Mail", "bob@example.com"); perm || !near(until, 24*time.Hour) {
		t.Fatalf("bob %v %v", perm, until)
	}
}

func TestAsksOfDifferentBotsDoNotMerge(t *testing.T) {
	c := targetCore(t)
	ctx := context.Background()
	mail := propose(t, c, BotRequest{Agent: "Mail", Access: []PluginAccessWant{{Plugin: "demo-mail", Accounts: []string{"ann@example.com"}}}})
	half, err := c.RequestAccess(ctx, "Half", []AccessResource{{Plugin: "demo-mail", Account: "bob@example.com"}}, "check bob's mail", 60)
	if err != nil {
		t.Fatal(err)
	}
	if half["merged"] != nil || half["approval_id"] == mail["approval_id"] {
		t.Fatalf("another Bot's access joined Mail's request: %v", half)
	}
	if det, _ := c.Approvals.Detail(mail["approval_id"].(string)); len(det.Items) != 0 || det.State != approvals.Pending {
		t.Fatalf("Mail's request changed: %+v", det)
	}
	if det, _ := c.Approvals.Detail(half["approval_id"].(string)); det.Kind != "rubi.access" {
		t.Fatalf("Half's access: %+v", det)
	}
	// and the other way: Half's permanent request takes Half's own access, never Mail's
	res := propose(t, c, BotRequest{Agent: "Half", Access: []PluginAccessWant{{Plugin: "demo-mail", Accounts: []string{"ann@example.com"}}}})
	if res["merged_access"] != half["approval_id"] {
		t.Fatalf("%v", res)
	}
	if s, _ := c.Approvals.Get(mail["approval_id"].(string)); s.State != approvals.Pending {
		t.Fatalf("Mail's request was touched by Half's: %+v", s)
	}
}

func TestOneOffAccessForAnotherTaskStaysSeparate(t *testing.T) {
	c, id := combinedCard(t)
	out, err := c.RequestAccess(context.Background(), "Mail", []AccessResource{{Plugin: "demo-mail", Account: "bob@example.com"}}, "a different task", 30)
	if err != nil {
		t.Fatal(err)
	}
	if out["merged"] != nil || out["approval_id"] == id {
		t.Fatalf("a second task joined the card: %v", out)
	}
	if s, _ := c.Approvals.Get(id); s.State != approvals.Pending {
		t.Fatalf("the card was cancelled: %+v", s)
	}
}

// webhookCard is Mail's request for its webhook, with its one-off access for bob joined. With assignAnn Mail is
// assigned to ann already, so ann is part of the task code without a question.
func webhookCard(t *testing.T, assignAnn bool) (*Core, string, string) {
	t.Helper()
	c := targetCore(t)
	dek, _ := c.Vault.Key()
	c.Audit.SetVaultKey(dek)
	hook := ""
	_ = c.Vault.View(func(d *vault.Data) error { hook = findAgent(d, "Mail").URL; return nil })
	if assignAnn {
		_ = c.Vault.Update(func(d *vault.Data) error {
			a, _ := accountOf(d, "demo-mail", "ann@example.com")
			a.Agents = []string{"Mail"}
			return nil
		})
	}
	propose(t, c, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: testHookKey}})
	asks := []AccessResource{{Plugin: "demo-mail", Account: "bob@example.com"}}
	if assignAnn {
		asks = append([]AccessResource{{Plugin: "demo-mail", Account: "ann@example.com"}}, asks...)
	}
	out, err := c.RequestAccess(context.Background(), "Mail", asks, "check bob's mail", 60)
	if err != nil || out["merged"] == nil {
		t.Fatalf("the access did not join the webhook request: %v %v", out, err)
	}
	return c, out["approval_id"].(string), hook
}

func bobOnly(c *Core) []string {
	var out []string
	_ = c.Vault.View(func(d *vault.Data) error {
		a, _ := accountOf(d, "demo-mail", "bob@example.com")
		out = append(out, a.Agents...)
		return nil
	})
	return out
}

// 2. The one-off part dropped as a duplicate of an account kept for good must not lose the task code.
func TestDroppedDuplicateKeepsTheTaskCodeForAssignedAccounts(t *testing.T) {
	c := targetCore(t)
	_ = c.Vault.Update(func(d *vault.Data) error {
		a, _ := accountOf(d, "demo-mail", "ann@example.com")
		a.Agents = []string{"Mail"}
		return nil
	})
	propose(t, c, BotRequest{Agent: "Mail", Access: []PluginAccessWant{{Plugin: "demo-mail", Accounts: []string{"bob@example.com"}}}})
	out, err := c.RequestAccess(context.Background(), "Mail",
		[]AccessResource{{Plugin: "demo-mail", Account: "ann@example.com"}, {Plugin: "demo-mail", Account: "bob@example.com"}}, "check mail", 60)
	if err != nil {
		t.Fatal(err)
	}
	note, _ := out["access_note"].(string)
	if !strings.Contains(note, "bob@example.com") || !strings.Contains(note, "for good") || strings.Contains(note, "Nothing is left") {
		t.Fatalf("the response does not say what was dropped: %v", out)
	}
	approveCard(t, c, out["approval_id"].(string), "apply")
	if perm, until := accountState(c, "Mail", "bob@example.com"); !perm || !until.IsZero() {
		t.Fatalf("bob %v %v", perm, until)
	}
	if g, ok := grantOf(c, "Mail", "ann@example.com"); !ok || !near(g.expires, time.Hour) {
		t.Fatalf("the task code for ann (already assigned) was lost with the duplicate: %v %+v", ok, g)
	}
}

func TestDroppedDuplicateWithNothingLeftIsSaid(t *testing.T) {
	c := targetCore(t)
	dek, _ := c.Vault.Key()
	c.Audit.SetVaultKey(dek)
	propose(t, c, BotRequest{Agent: "Mail", Access: []PluginAccessWant{{Plugin: "demo-mail", Accounts: []string{"bob@example.com"}}}})
	out, err := c.RequestAccess(context.Background(), "Mail", []AccessResource{{Plugin: "demo-mail", Account: "bob@example.com"}}, "check mail", 60)
	if err != nil {
		t.Fatal(err)
	}
	note, _ := out["access_note"].(string)
	if !strings.Contains(note, "dropped") || !strings.Contains(note, "Nothing is left of the one-off request") || out["instructions_access"] != nil {
		t.Fatalf("response: %v", out)
	}
	approveCard(t, c, out["approval_id"].(string), "apply")
	if perm, until := accountState(c, "Mail", "bob@example.com"); !perm || !until.IsZero() {
		t.Fatalf("bob %v %v", perm, until)
	}
	if ev := auditFor(t, c, "bot.request.once"); len(ev) != 0 {
		t.Fatalf("a one-off grant was recorded though nothing was left of it: %v", ev)
	}
}

// Unticking every one-off item: the accounts the Bot already has still get their task code (they need no approval),
// the unticked ones get nothing, and the receipt says so.
func TestUntickingEveryOneOffItemStillGivesTheAssignedAccountsTheirCode(t *testing.T) {
	c, id, _ := webhookCard(t, true)
	snap, err := c.Approvals.DecidePanel(context.Background(), id, "week|items:webhook", true, approvals.Verified)
	if err != nil || snap.State != approvals.Executed {
		t.Fatalf("%+v %v", snap, err)
	}
	if g, ok := grantOf(c, "Mail", "ann@example.com"); !ok || !near(g.expires, time.Hour) {
		t.Fatalf("ann (assigned) has no task code, or one that is not the task's: %v %+v", ok, g)
	}
	if perm, until := accountState(c, "Mail", "bob@example.com"); perm || !until.IsZero() {
		t.Fatalf("bob was allowed though unticked: %v %v", perm, until)
	}
	if _, ok := grantOf(c, "Mail", "bob@example.com"); ok {
		t.Fatal("bob got a code though unticked")
	}
	res, _ := snap.Result.(map[string]any)
	if r, _ := res["receipt"].(string); !strings.Contains(r, "already has") || !strings.Contains(r, "nothing new was allowed") {
		t.Fatalf("receipt: %v", res)
	}
}

// 3. Labels: with one-off items on the card, the others say "for good".
func TestCardLabelsSeparateForGoodFromForAWhile(t *testing.T) {
	c, id := combinedCard(t)
	check := func(id string) {
		det, _ := c.Approvals.Detail(id)
		if !strings.HasSuffix(det.Items[0].Label, " for good") || strings.HasSuffix(det.Items[0].Label, "for good for good") ||
			strings.Contains(det.Items[1].Label, "for good") || !strings.HasSuffix(det.Items[1].Label, "for the time you choose") {
			t.Fatalf("labels: %+v", det.Items)
		}
	}
	check(id)
	// joined again (same task): the card is rebuilt, the label is not suffixed twice
	out, err := c.RequestAccess(context.Background(), "Mail", []AccessResource{{Plugin: "demo-mail", Account: "bob@example.com"}}, "check bob's mail", 60)
	if err != nil || out["merged"] == nil {
		t.Fatalf("%v %v", out, err)
	}
	check(out["approval_id"].(string))

	cw, wid, _ := webhookCard(t, false)
	det, _ := cw.Approvals.Detail(wid)
	if !strings.HasSuffix(det.Items[0].Label, " for good") || det.Items[0].Key != webhookItem {
		t.Fatalf("the webhook label: %+v", det.Items)
	}
	// without one-off items the labels are as before
	c2 := targetCore(t)
	r := propose(t, c2, BotRequest{Agent: "Mail", Access: []PluginAccessWant{{Plugin: "demo-mail"}}})
	d2, _ := c2.Approvals.Detail(r["approval_id"].(string))
	for _, it := range d2.Items {
		if strings.Contains(it.Label, "for good") {
			t.Fatalf("a card with no one-off item changed its labels: %+v", d2.Items)
		}
	}
}

// 4. The request to join was decided between the check and the proposal: the ask is an access request again.
func TestJoinRaceFallsBackToTheAccessApproval(t *testing.T) {
	c, _, _ := func() (*Core, string, string) {
		c := targetCore(t)
		return c, "", ""
	}()
	first := propose(t, c, BotRequest{Agent: "Mail", Access: []PluginAccessWant{{Plugin: "demo-mail", Accounts: []string{"ann@example.com"}}}})
	once := &onceAsk{task: "check bob's mail", d: time.Hour}
	r := resolved{plugin: "demo-mail", name: "Demo Mail", account: "bob@example.com", label: "bob@example.com"}
	once.items = []*botItem{c.onceItem("Mail", once.task, r)}
	if !c.canJoinBotRequest("Mail", once) { // the check passes ...
		t.Fatal("expected the request to be joinable")
	}
	approveCard(t, c, first["approval_id"].(string), "apply") // ... then the user decides it
	out, err := c.proposeBot(context.Background(), BotRequest{Agent: "Mail"}, once)
	if err != nil || out["approval_id"] == nil {
		t.Fatalf("the ask got lost: %v %v", out, err)
	}
	det, _ := c.Approvals.Detail(out["approval_id"].(string))
	if det.Kind != "rubi.access" || det.State != approvals.Pending {
		t.Fatalf("%+v", det)
	}
	approveCard(t, c, out["approval_id"].(string), "day")
	if perm, until := accountState(c, "Mail", "bob@example.com"); perm || !near(until, 24*time.Hour) {
		t.Fatalf("bob %v %v", perm, until)
	}
}

// 5. The access approval is cancelled only once the card exists.
func TestAFailedSubmitLeavesTheWaitingAccessAlive(t *testing.T) {
	c := targetCore(t)
	ctx := context.Background()
	acc, err := c.RequestAccess(ctx, "Mail", []AccessResource{{Plugin: "demo-mail", Account: "bob@example.com"}}, "check bob's mail", 60)
	if err != nil {
		t.Fatal(err)
	}
	accID := acc["approval_id"].(string)
	var fillers []string
	for { // fill Rubi's waiting list until the next request is refused
		out, err := c.Approvals.Submit(ctx, approvals.Request{Integration: "rubi", Kind: "rubi.filler", Summary: "x",
			Options: []approvals.Option{{Key: "ok", Label: "Ok"}}, Execute: func(context.Context, string) (any, error) { return nil, nil }})
		if err != nil {
			break
		}
		fillers = append(fillers, out["approval_id"].(string))
	}
	if _, err := c.ProposeBot(ctx, BotRequest{Agent: "Mail", Access: []PluginAccessWant{{Plugin: "demo-mail", Accounts: []string{"ann@example.com"}}}}); err == nil {
		t.Fatal("the proposal was accepted with a full waiting list")
	}
	if s, _ := c.Approvals.Get(accID); s.State != approvals.Pending {
		t.Fatalf("the user's waiting access was cancelled by a request that failed: %+v", s)
	}
	if c.peekPendingOnce("Mail") == nil {
		t.Fatal("the waiting access is no longer known")
	}
	// with room again, it is taken in
	_, _ = c.Approvals.Cancel(fillers[0])
	_, _ = c.Approvals.Cancel(fillers[1])
	res, err := c.ProposeBot(ctx, BotRequest{Agent: "Mail", Access: []PluginAccessWant{{Plugin: "demo-mail", Accounts: []string{"ann@example.com"}}}})
	if err != nil || res["merged_access"] != accID {
		t.Fatalf("%v %v", res, err)
	}
	if s, _ := c.Approvals.Get(accID); s.State != approvals.Cancelled {
		t.Fatalf("%+v", s)
	}
}

// 6. The one-off grant is recorded.
func TestTheOneOffGrantIsAudited(t *testing.T) {
	c, id, _ := webhookCard(t, false)
	approveCard(t, c, id, "day|items:webhook,"+onceItem)
	ev := auditFor(t, c, "bot.request.once")
	if len(ev) != 1 || ev[0]["once_accounts"] != "Demo Mail (bob@example.com)" || ev[0]["duration"] != "day" || ev[0]["task"] != "check bob's mail" {
		t.Fatalf("audit: %v", ev)
	}
	if strings.Contains(toJSON(ev), testHookKey) {
		t.Fatal("the audit holds the webhook key")
	}
}

func toJSON(v any) string { return fmt.Sprint(v) }

// 7. An assignment taken away while the card waited gets no code.
func TestAnAssignmentRevokedWhileTheCardWaitedGetsNoCode(t *testing.T) {
	c, id, _ := webhookCard(t, true)
	_ = c.Vault.Update(func(d *vault.Data) error {
		a, _ := accountOf(d, "demo-mail", "ann@example.com")
		a.Agents = []string{"Main"}
		return nil
	})
	approveCard(t, c, id, "allow|items:webhook,"+onceItem)
	if _, ok := grantOf(c, "Mail", "ann@example.com"); ok {
		t.Fatal("a code was issued for an account that was taken away from the Bot")
	}
	if _, ok := grantOf(c, "Mail", "bob@example.com"); !ok {
		t.Fatal("bob, allowed on the card, got no code")
	}
	// and when that was all that was left, the card says it gave nothing
	c, id, _ = webhookCard(t, true)
	_ = c.Vault.Update(func(d *vault.Data) error {
		a, _ := accountOf(d, "demo-mail", "ann@example.com")
		a.Agents = []string{"Main"}
		return nil
	})
	snap, err := c.Approvals.DecidePanel(context.Background(), id, "allow|items:webhook", true, approvals.Verified)
	res, _ := snap.Result.(map[string]any)
	if err != nil || res["access_failed"] == nil {
		t.Fatalf("%+v %v", snap, err)
	}
}

// 8. The rest of the combined card.
func TestAnUnknownDurationIsTheShortest(t *testing.T) {
	c, id := combinedCard(t)
	if _, err := c.Approvals.DecidePanel(context.Background(), id, "forever|items:"+onceItem, true, approvals.Verified); err == nil {
		t.Fatal("the engine accepted an unknown option")
	}
	b := &c.botReqs
	b.mu.Lock()
	bundle := b.byAgent["mail"]
	b.mu.Unlock()
	for _, d := range []string{"bogus", "", "ALWAYS"} {
		c.access.mu.Lock() // the throttle of repeated codes is not what is tested
		c.access.issued, c.access.grants = nil, nil
		c.access.mu.Unlock()
		if _, err := c.applyOnce(bundle, bundle.once.items, d); err != nil {
			t.Fatal(err)
		}
		perm, until := accountState(c, "Mail", "bob@example.com")
		g, _ := grantOf(c, "Mail", "bob@example.com")
		if perm || !until.IsZero() || !near(g.expires, time.Hour) {
			t.Fatalf("duration %q: permanent %v, until %v, code until %v", d, perm, until, g.expires)
		}
	}
}

func TestApprovingTheCardWithoutAnItemListTicksEverything(t *testing.T) {
	c, id := combinedCard(t)
	approveCard(t, c, id, "day")
	if perm, _ := accountState(c, "Mail", "ann@example.com"); !perm {
		t.Fatal("ann")
	}
	if perm, until := accountState(c, "Mail", "bob@example.com"); perm || !near(until, 24*time.Hour) {
		t.Fatalf("bob %v %v", perm, until)
	}
}

func TestWebhookAndOneOffItemTogether(t *testing.T) {
	c, id, hook := webhookCard(t, false)
	det, _ := c.Approvals.Detail(id)
	if len(det.Items) != 2 || det.Items[0].Key != webhookItem || det.Items[1].Key != onceItem || len(det.Options) != 4 {
		t.Fatalf("%+v", det)
	}
	approveCard(t, c, id, "day|items:webhook,"+onceItem)
	if a := agentOf(c, "Mail"); a == nil || a.URL != hook || a.Key != testHookKey {
		t.Fatalf("the webhook was not applied: %+v", a)
	}
	if perm, until := accountState(c, "Mail", "bob@example.com"); perm || !near(until, 24*time.Hour) {
		t.Fatalf("bob %v %v", perm, until)
	}
}

func TestAlwaysAssignsOnlyTheOneOffAccounts(t *testing.T) {
	c, id, _ := webhookCard(t, false)
	approveCard(t, c, id, "always|items:webhook,"+onceItem)
	if got := bobOnly(c); len(got) != 1 || got[0] != "Mail" {
		t.Fatalf("bob: %v", got)
	}
	if perm, until := accountState(c, "Mail", "ann@example.com"); perm || !until.IsZero() {
		t.Fatalf("ann, which was not on the card, was assigned: %v %v", perm, until)
	}
	if _, until := accountState(c, "Mail", "bob@example.com"); !until.IsZero() {
		t.Fatal("bob has an expiry as well as the assignment")
	}
}

func TestAllowLeavesNothingBehind(t *testing.T) {
	c, id, _ := webhookCard(t, false)
	approveCard(t, c, id, "allow|items:webhook,"+onceItem)
	if got := bobOnly(c); len(got) != 0 {
		t.Fatalf("bob stayed assigned: %v", got)
	}
	if perm, until := accountState(c, "Mail", "bob@example.com"); perm || !until.IsZero() {
		t.Fatalf("bob: %v %v", perm, until)
	}
	if _, ok := grantOf(c, "Mail", "bob@example.com"); !ok {
		t.Fatal("the task code is missing")
	}
	if ev := auditFor(t, c, "access.kept"); len(ev) != 0 {
		t.Fatalf("something was kept: %v", ev)
	}
}

func TestTheRateLimitStopsJoins(t *testing.T) {
	c, _ := combinedCard(t)
	var last map[string]any
	var err error
	for i := 0; i < 15 && err == nil; i++ {
		last, err = c.RequestAccess(context.Background(), "Mail", []AccessResource{{Plugin: "demo-mail", Account: "bob@example.com"}}, "check bob's mail", 60)
	}
	if err == nil || !strings.Contains(err.Error(), "too many requests") {
		t.Fatalf("joins were never limited: %v %v", last, err)
	}
	// the card that waits is the last one that was allowed
	if id := c.pendingBotRequest("Mail"); id == "" {
		t.Fatal("no card waits")
	} else if s, _ := c.Approvals.Get(id); s.State != approvals.Pending {
		t.Fatalf("%+v", s)
	}
}

func TestOneOffAsksForDifferentTasksGiveAClearError(t *testing.T) {
	if _, err := combineOnce(&onceAsk{task: "a"}, &onceAsk{task: "b"}); err == nil ||
		!strings.Contains(err.Error(), `"a" and "b"`) || !strings.Contains(err.Error(), "can't share one approval") {
		t.Fatalf("combineOnce: %v", err)
	}
	c, id := combinedCard(t)
	other := &onceAsk{task: "another task", d: time.Hour}
	other.items = []*botItem{c.onceItem("Mail", other.task, resolved{plugin: "demo-mail", name: "Demo Mail", account: "bob@example.com", label: "bob@example.com"})}
	_, err := c.proposeBot(context.Background(), BotRequest{Agent: "Mail"}, other)
	if err == nil || !strings.Contains(err.Error(), "different tasks") || !strings.Contains(err.Error(), "ask again with everything") {
		t.Fatalf("mid-merge: %v", err)
	}
	_ = id
}
