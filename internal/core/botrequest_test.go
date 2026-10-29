package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
	"github.com/Deikus-LXXVII/rubi/internal/paths"
	"github.com/Deikus-LXXVII/rubi/internal/vault"
)

// testHookKey must never show up anywhere but the vault and the Authorization header.
const testHookKey = "crsr_TESTKEY_never_logged_0123456789"

type hookCall struct{ auth, typ string }

// botCore is an unlocked, paired Rubi with a connected two-account plugin, and a routine stand-in.
func botCore(t *testing.T) (*Core, string, chan hookCall) {
	t.Helper()
	home := t.TempDir()
	c, err := Open(paths.Layout{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Lock)
	c.BotWebhookOK = localReceiver
	if err := c.Vault.Create(vault.NewKey(), vault.NewData("")); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.paired = true
	c.mu.Unlock()
	c.SetEndpoint("https://example.trycloudflare.com")
	installManifest(t, c, `{"schema":1,"id":"demo-mail","name":"Demo Mail","version":"v1.0.0","api":1,
		"entry":"demo-mail","publisher":{"name":"x","key":"MCowBQYDK2VwAyEAxeDfKAkO77JdARN7Y2jJT3tXw9mN+GqqH8R5mhcxt8c="},
		"egress":["mail.example.com"],
		"actions":[{"kind":"demo-mail.send","title":"Send email","default_level":"strong"}]}`)
	_ = c.Vault.Update(func(d *vault.Data) error {
		d.Integrations["demo-mail"] = &vault.Integration{Enabled: true, Accounts: []*vault.Account{
			{ID: "ann@example.com", Label: "ann@example.com", Default: true},
			{ID: "bob@example.com", Label: "bob@example.com"}}}
		return nil
	})
	ch := make(chan hookCall, 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		ch <- hookCall{auth: r.Header.Get("Authorization"), typ: fmt.Sprint(m["type"])}
	}))
	t.Cleanup(srv.Close)
	return c, srv.URL + "/automations/webhook/6076a0ff-5b26-405e-82ef-d6173cd96066", ch
}

func propose(t *testing.T, c *Core, r BotRequest) map[string]any {
	t.Helper()
	out, err := c.ProposeBot(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if out["status"] != "awaiting_approval" || out["approval_id"] == "" || !strings.Contains(fmt.Sprint(out["approval_url"]), "#") {
		t.Fatalf("propose: %v", out)
	}
	return out
}

func agentOf(c *Core, name string) *vault.Agent {
	var out *vault.Agent
	_ = c.Vault.View(func(d *vault.Data) error {
		if a := findAgent(d, name); a != nil {
			cp := *a
			out = &cp
		}
		return nil
	})
	return out
}

func assigned(c *Core, account string) []string {
	var out []string
	_ = c.Vault.View(func(d *vault.Data) error {
		a, _ := accountOf(d, "demo-mail", account)
		out = append(out, a.Agents...)
		return nil
	})
	return out
}

func waitGone(t *testing.T, c *Core, agent string) {
	t.Helper()
	for i := 0; i < 100 && c.pendingBotRequest(agent) != ""; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if id := c.pendingBotRequest(agent); id != "" {
		t.Fatalf("request %s still kept", id)
	}
}

// noKeyIn fails when the webhook key appears in what agents, the panel or the log can see.
func noKeyIn(t *testing.T, c *Core, what ...any) {
	t.Helper()
	for _, w := range what {
		b, _ := json.Marshal(w)
		if strings.Contains(string(b), testHookKey) || strings.Contains(fmt.Sprintf("%+v", w), testHookKey) {
			t.Fatalf("the webhook key leaked: %s", b)
		}
	}
	_ = filepath.Walk(c.Layout.Home, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), testHookKey) {
				t.Fatalf("the webhook key is on disk in plain text: %s", p)
			}
		}
		return nil
	})
	b := &c.botReqs
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, x := range b.byID {
		for _, it := range x.items {
			if strings.Contains(string(it.sealed), testHookKey) {
				t.Fatal("the waiting key isn't sealed")
			}
		}
	}
}

func TestBotConnectAppliesOnlyWithApproval(t *testing.T) {
	c, hook, ch := botCore(t)
	out := propose(t, c, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: "Authorization: Bearer " + testHookKey}})
	id := out["approval_id"].(string)
	if out["replaces_existing_webhook"] != false {
		t.Fatalf("a new Bot marked as a replacement: %v", out)
	}
	det, _ := c.Approvals.Detail(id)
	if det.Level != approvals.Strong || det.Kind != botRequestKind {
		t.Fatalf("not a strong Bot request: %+v", det.Snapshot)
	}
	p := det.Preview.(map[string]any)
	if !strings.Contains(fmt.Sprint(p["webhook"]), "/automations/webhook/6076a0ff") || p["agent"] != "Mail" || p["replaces"] != nil {
		t.Fatalf("preview: %v", p)
	}
	noKeyIn(t, c, out, det)

	// Without the user's proof nothing changes: the chat path is refused and the vault is untouched.
	if _, err := c.Approvals.ConfirmChat(context.Background(), id, "Approve · "+det.Code); !errors.Is(err, approvals.ErrWrongPath) {
		t.Fatalf("a chat confirmation was accepted: %v", err)
	}
	if agentOf(c, "Mail") != nil {
		t.Fatal("the webhook was set before approval")
	}

	// DecidePanel is what the panel API calls after checking the passkey or password.
	snap, err := c.Approvals.DecidePanel(context.Background(), id, "apply", true, approvals.Verified)
	if err != nil || snap.State != approvals.Executed {
		t.Fatalf("approve: %+v %v", snap, err)
	}
	a := agentOf(c, "Mail")
	if a == nil || a.URL != hook || a.Key != testHookKey || !a.Default {
		t.Fatalf("not applied: %+v", a)
	}
	got := <-ch
	if got.auth != "Bearer "+testHookKey || !strings.HasSuffix(got.typ, "webhook.test") {
		t.Fatalf("the test delivery: type %s, right key %v", got.typ, got.auth == "Bearer "+testHookKey)
	}
	res, _ := snap.Result.(map[string]any)
	if !strings.Contains(fmt.Sprint(res["receipt"]), "Test event delivered") {
		t.Fatalf("result: %v", snap.Result)
	}
	noKeyIn(t, c, snap)
	waitGone(t, c, "Mail")
}

func TestBotRequestDenyAndLockDiscardTheKey(t *testing.T) {
	c, hook, _ := botCore(t)
	out := propose(t, c, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: testHookKey}})
	id := out["approval_id"].(string)
	c.botReqs.mu.Lock()
	item := c.botReqs.byID[id].items[0]
	c.botReqs.mu.Unlock()
	if snap, err := c.Approvals.DecidePanel(context.Background(), id, "", false, approvals.Verified); err != nil || snap.State != approvals.Denied {
		t.Fatalf("deny: %+v %v", snap, err)
	}
	waitGone(t, c, "Mail")
	if item.sealed != nil || agentOf(c, "Mail") != nil {
		t.Fatal("a denied request kept its key or changed the webhook")
	}

	propose(t, c, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: testHookKey}})
	c.Lock()
	if c.pendingBotRequest("Mail") != "" {
		t.Fatal("locking kept a waiting request")
	}
}

func TestBotRequestExpiryDiscards(t *testing.T) {
	c, hook, _ := botCore(t)
	out := propose(t, c, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: testHookKey}})
	id := out["approval_id"].(string)
	c.Approvals.SetClock(func() time.Time { return time.Now().Add(time.Hour) })
	c.expireBotRequest(id) // what the request's timer does when its time is up
	if s, _ := c.Approvals.Get(id); s.State != approvals.Expired {
		t.Fatalf("state %s", s.State)
	}
	if c.pendingBotRequest("Mail") != "" {
		t.Fatal("an expired request kept its key")
	}
	if _, err := c.Approvals.DecidePanel(context.Background(), id, "apply", true, approvals.Verified); err == nil {
		t.Fatal("an expired request was approved")
	}
	if agentOf(c, "Mail") != nil {
		t.Fatal("an expired request changed the webhook")
	}
}

func TestBotRequestReplacementIsMarkedAndChecked(t *testing.T) {
	c, hook, _ := botCore(t)
	const was = "https://api2.cursor.sh/automations/webhook/aaaaaaaa-0000-0000-0000-000000000001"
	_ = c.Vault.Update(func(d *vault.Data) error {
		d.Agents = append(d.Agents, &vault.Agent{Name: "Mail", URL: was, Key: "crsr_old", Default: true})
		return nil
	})
	out := propose(t, c, BotRequest{Agent: "mail", Webhook: &WebhookProposal{URL: hook, Key: testHookKey}})
	id := out["approval_id"].(string)
	det, _ := c.Approvals.Detail(id)
	p := det.Preview.(map[string]any)
	if out["replaces_existing_webhook"] != true || !strings.HasPrefix(det.Summary, "Replace Mail's webhook") ||
		!strings.Contains(fmt.Sprint(p["replaces"]), "REPLACES the webhook https://api2.cursor.sh/automations/webhook/aaaaaaaa") {
		t.Fatalf("replacement not marked: %v %s %v", out, det.Summary, p)
	}
	// The webhook changes in between (e.g. the user connected it in the panel): approving the old request
	// must not silently replace what the user didn't see.
	const now = "https://api2.cursor.sh/automations/webhook/cccccccc-0000-0000-0000-000000000003"
	_ = c.Vault.Update(func(d *vault.Data) error { findAgent(d, "Mail").URL = now; return nil })
	snap, _ := c.Approvals.DecidePanel(context.Background(), id, "apply", true, approvals.Verified)
	if snap.State != approvals.Failed || agentOf(c, "Mail").URL != now {
		t.Fatalf("a stale replacement applied: %+v", snap)
	}
}

func TestBotRequestsMergeAndPartialApproval(t *testing.T) {
	c, hook, _ := botCore(t)
	first := propose(t, c, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: testHookKey}, Reason: "connect"})
	// Plugins for a Bot that isn't connected yet are refused unless its webhook is in the same request.
	if _, err := c.ProposeBot(context.Background(), BotRequest{Agent: "Other", Access: []PluginAccessWant{{Plugin: "demo-mail"}}}); err == nil {
		t.Fatal("accounts for an unknown Bot")
	}
	second := propose(t, c, BotRequest{Agent: "Mail", Access: []PluginAccessWant{{Plugin: "demo-mail"}}, Reason: "read mail"})
	id := second["approval_id"].(string)
	if second["merged"] != first["approval_id"] {
		t.Fatalf("not merged: %v", second)
	}
	if s, _ := c.Approvals.Get(first["approval_id"].(string)); s.State != approvals.Cancelled {
		t.Fatalf("the earlier request is still %s", s.State)
	}
	if c.pendingBotRequest("Mail") != id {
		t.Fatal("more than one waiting request for a Bot")
	}
	det, _ := c.Approvals.Detail(id)
	if len(det.Items) != 3 || det.Items[0].Key != webhookItem || det.Items[1].Key != "access:demo-mail|ann@example.com" {
		t.Fatalf("items: %+v", det.Items)
	}
	if ip := det.Items[1].Preview.(map[string]any); !strings.Contains(fmt.Sprint(ip["can"]), "Send email") || ip["connects_to"] != "mail.example.com" {
		t.Fatalf("the permissions aren't shown: %v", ip)
	}
	noKeyIn(t, c, second, det)

	// The user leaves Bob's mailbox out.
	opt := "items:webhook,access:demo-mail|ann@example.com"
	if err := c.Approvals.CheckOption(id, opt); err != nil {
		t.Fatal(err)
	}
	snap, err := c.Approvals.DecidePanel(context.Background(), id, opt, true, approvals.Verified)
	if err != nil || snap.State != approvals.Executed {
		t.Fatalf("approve: %+v %v", snap, err)
	}
	if a := agentOf(c, "Mail"); a == nil || a.Key != testHookKey {
		t.Fatal("the webhook wasn't applied")
	}
	if got := assigned(c, "ann@example.com"); len(got) != 1 || got[0] != "Mail" {
		t.Fatalf("ann: %v", got)
	}
	if got := assigned(c, "bob@example.com"); len(got) != 0 {
		t.Fatalf("a left-out account was granted: %v", got)
	}
	waitGone(t, c, "Mail")

	// Asking again for what it has is not a new approval.
	if out, err := c.ProposeBot(context.Background(), BotRequest{Agent: "Mail", Access: []PluginAccessWant{{Plugin: "demo-mail", Accounts: []string{"ann@example.com"}}}}); err != nil || out["status"] != "already_allowed" {
		t.Fatalf("again: %v %v", out, err)
	}
}

func TestBotRequestAccountsWithoutTheWebhookFail(t *testing.T) {
	c, hook, _ := botCore(t)
	out := propose(t, c, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: testHookKey},
		Access: []PluginAccessWant{{Plugin: "demo-mail", Accounts: []string{"bob@example.com"}}}})
	snap, _ := c.Approvals.DecidePanel(context.Background(), out["approval_id"].(string), "items:access:demo-mail|bob@example.com", true, approvals.Verified)
	if snap.State != approvals.Failed || len(assigned(c, "bob@example.com")) != 0 || agentOf(c, "Mail") != nil {
		t.Fatalf("accounts given to a Bot that isn't connected: %+v", snap)
	}
}

func TestBotRequestValidationAndRateLimit(t *testing.T) {
	c, hook, _ := botCore(t)
	bad := []BotRequest{
		{Agent: "Mail"},
		{Agent: "", Webhook: &WebhookProposal{URL: hook, Key: testHookKey}},
		{Agent: "Mail", Webhook: &WebhookProposal{URL: "https://api2.cursor.sh@evil.example/x", Key: testHookKey}},
		{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: testHookKey + "\r\nX: 1"}},
		{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: testHookKey, Events: []string{"nope"}}},
		{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: testHookKey}, Access: []PluginAccessWant{{Plugin: "not-installed"}}},
	}
	for i, r := range bad {
		if _, err := c.ProposeBot(context.Background(), r); err == nil {
			t.Fatalf("bad request %d accepted", i)
		} else if strings.Contains(err.Error(), testHookKey) {
			t.Fatalf("an error repeats the key: %v", err)
		}
	}
	for i := 0; i < botRequestsPerHour; i++ {
		propose(t, c, BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: testHookKey}})
	}
	if _, err := c.ProposeBot(context.Background(), BotRequest{Agent: "Mail", Webhook: &WebhookProposal{URL: hook, Key: testHookKey}}); err == nil {
		t.Fatal("no rate limit")
	}
	c.Lock()
	if _, err := c.ProposeBot(context.Background(), BotRequest{Agent: "Other", Webhook: &WebhookProposal{URL: hook, Key: testHookKey}}); err == nil {
		t.Fatal("a request while locked")
	}
}
