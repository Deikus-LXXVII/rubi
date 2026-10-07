package icloudmail

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Deikus-LXXVII/rubi/internal/core"
	"github.com/Deikus-LXXVII/rubi/internal/integrations"
	"github.com/Deikus-LXXVII/rubi/internal/mcpserver"
	"github.com/Deikus-LXXVII/rubi/internal/panelapi"
	"github.com/Deikus-LXXVII/rubi/internal/panelclient"
	"github.com/Deikus-LXXVII/rubi/internal/paths"
)

type agent struct {
	t  *testing.T
	ss *mcp.ClientSession
}

func (a agent) call(name string, args map[string]any) map[string]any {
	a.t.Helper()
	res, err := a.ss.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		a.t.Fatal(err)
	}
	if res.IsError {
		return map[string]any{"tool_error": res.Content[0].(*mcp.TextContent).Text}
	}
	var out map[string]any
	b, _ := json.Marshal(res.StructuredContent)
	_ = json.Unmarshal(b, &out)
	return out
}

func panelFor(t *testing.T, c *core.Core, purpose string) *panelclient.Client {
	t.Helper()
	link, err := c.Link(purpose)
	if err != nil {
		t.Fatal(err)
	}
	return panelWithLink(t, link)
}

func panelWithLink(t *testing.T, link string) *panelclient.Client {
	t.Helper()
	l, err := panelclient.ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := panelclient.New(l)
	if err != nil {
		t.Fatal(err)
	}
	return pc
}

// approveChange approves a settings change returned by a settings operation.
func approveChange(t *testing.T, base *panelclient.Client, res map[string]any, option, password string) map[string]any {
	t.Helper()
	pc := *base
	pc.Link.Ticket = res["ticket"].(string)
	out, err := pc.ApproveWithPassword(res["approval_id"].(string), option, password)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFullStack(t *testing.T) {
	addr := startIMAP(t)
	defaultIMAPAddr = addr
	var mu sync.Mutex
	var sent []string
	sendMail = func(_, _, _, _ string, _ []string, msg []byte) error {
		mu.Lock()
		defer mu.Unlock()
		sent = append(sent, string(msg))
		return nil
	}

	// The agent's routine webhook.
	type hit struct {
		auth string
		body map[string]any
	}
	hits := make(chan hit, 20)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		hits <- hit{auth: r.Header.Get("Authorization"), body: m}
	}))
	defer hook.Close()

	c, err := core.Open(paths.Layout{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(panelapi.New(c).Handler())
	defer api.Close()
	c.SetEndpoint(api.URL)

	ct, st := mcp.NewInMemoryTransports()
	srv := mcpserver.New(c)
	go func() { _ = srv.MCP().Run(context.Background(), st) }()
	client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "test"}, nil)
	ss, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	ag := agent{t: t, ss: ss}

	const pw = "correct horse battery"
	if _, err := panelFor(t, c, "pair").PairWithPassword(pw); err != nil {
		t.Fatal(err)
	}

	// 1. Not connected yet: the agent gets the setup link and what's needed.
	out := ag.call("icloud_mail_search", map[string]any{"subject": "x"})
	if out["status"] != "not_connected" || !strings.Contains(out["link"].(string), "setup%3Aicloud-mail") {
		t.Fatalf("not connected: %v", out)
	}

	// 2. The user connects iCloud Mail in the panel; a wrong password never gets to an approval.
	setup := panelFor(t, c, "setup:icloud-mail")
	if err := setup.Call("integration.setup", map[string]any{"id": ID, "fields": map[string]string{"address": testUser},
		"secrets": map[string]string{"app_password": "wrong"}}, nil); err == nil {
		t.Fatal("setup accepted a wrong app password")
	}
	var res map[string]any
	if err := setup.Call("integration.setup", map[string]any{"id": ID, "fields": map[string]string{"address": testUser, "from_name": "Test User"},
		"secrets": map[string]string{"app_password": testPass}}, &res); err != nil {
		t.Fatal(err)
	}
	if r := approveChange(t, setup, res, "apply", pw); r["state"] != "executed" {
		t.Fatalf("connect approval: %v", r)
	}

	// Webhook for the agent's routine (also a strong-approved change).
	if err := setup.Call("webhook.set", map[string]any{"url": hook.URL, "key": "crsr_testkey"}, &res); err == nil {
		t.Fatal("setup ticket changed the webhook")
	}
	settings := panelFor(t, c, "settings")
	if err := settings.Call("webhook.set", map[string]any{"url": hook.URL, "key": "crsr_testkey"}, &res); err != nil {
		t.Fatal(err)
	}
	approveChange(t, settings, res, "apply", pw)

	// 3. Read and search work without approval (default level "none") and don't mark mail as read.
	deliver(t, addr, "INBOX", "From: Anna <anna@example.com>\nTo: me@icloud.com\nSubject: Meeting\nMessage-ID: <m1@example.com>\n\nShall we meet?\n")
	out = ag.call("icloud_mail_search", map[string]any{"subject": "Meeting"})
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("search: %v", out)
	}
	uid := msgs[0].(map[string]any)["uid"]

	// 4. Sending needs the panel; the agent can't confirm it itself.
	out = ag.call("icloud_mail_send", map[string]any{"to": []string{"anna@example.com"}, "subject": "", "body": "Yes, 15:00.",
		"reply_to_uid": uid})
	if out["status"] != "awaiting_approval" || out["level"] != "strong" {
		t.Fatalf("send: %v", out)
	}
	approvalID := out["approval_id"].(string)
	if r := ag.call("rubi_confirm", map[string]any{"approval_id": approvalID, "user_response": "Send"}); r["tool_error"] == nil {
		t.Fatalf("agent confirmed a strong approval: %v", r)
	}
	if len(sent) != 0 {
		t.Fatal("sent without approval")
	}

	// 5. The user approves "Send and notify on reply" in the panel.
	approve := panelWithLink(t, out["approval_url"].(string))
	if r, err := approve.ApproveWithPassword(approvalID, "send_track", pw); err != nil || r["state"] != "executed" {
		t.Fatalf("approve send: %v %v", r, err)
	}
	if len(sent) != 1 || !strings.Contains(sent[0], "In-Reply-To: <m1@example.com>") || !strings.Contains(sent[0], "Subject: Re: Meeting") {
		t.Fatalf("sent message: %v", sent)
	}
	out = ag.call("rubi_approval", map[string]any{"approval_id": approvalID})
	result := out["approval"].(map[string]any)["result"].(map[string]any)
	if result["tracking"] == nil || result["saved_to_sent"] != true {
		t.Fatalf("result: %v", result)
	}
	msgID := result["message_id"].(string)

	// 6. A reply arrives; the watcher finds it and the agent's webhook is called with the routine key.
	deliver(t, addr, "INBOX", fmt.Sprintf("From: Anna <anna@example.com>\nTo: me@icloud.com\nSubject: Re: Re: Meeting\nMessage-ID: <m2@example.com>\nIn-Reply-To: %s\n\nGreat.\n", msgID))
	rt, _ := integrations.Get(ID)
	x := rt.(*integration)
	x.mu.Lock()
	h := x.host
	x.mu.Unlock()
	if h == nil {
		t.Fatal("integration not running after connect")
	}
	if err := x.poll(h); err != nil {
		t.Fatal(err)
	}
	gotReply := false
	deadline := time.After(5 * time.Second)
	for !gotReply {
		select {
		case hh := <-hits:
			if hh.auth != "Bearer crsr_testkey" {
				t.Fatalf("webhook auth %q", hh.auth)
			}
			if hh.body["type"] == "icloud-mail.reply" {
				gotReply = true
				if f, _ := hh.body["untrusted_fields"].([]any); len(f) != 2 {
					t.Fatalf("untrusted fields: %v", hh.body)
				}
			}
		case <-deadline:
			t.Fatal("no reply event at the webhook")
		}
	}
	evs := ag.call("rubi_events", nil)["events"].([]any)
	found := false
	for _, e := range evs {
		if e.(map[string]any)["type"] == "reply" {
			found = true
		}
	}
	if !found {
		t.Fatalf("events: %v", evs)
	}

	// 7. Locking stops everything; the agent gets the unlock link.
	ag.call("rubi_lock", nil)
	if out := ag.call("icloud_mail_search", map[string]any{}); out["status"] != "locked" {
		t.Fatalf("after lock: %v", out)
	}
	x.mu.Lock()
	stopped := x.host == nil
	x.mu.Unlock()
	if !stopped {
		t.Fatal("watcher still running after lock")
	}
}
