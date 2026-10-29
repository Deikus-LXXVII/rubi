package mcpserver_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWebhookConnectFeedback: connecting a Bot through its agent link checks the pasted key, warns
// when the address isn't a Grok Bot routine, and the test right after says whether the routine took it.
func TestWebhookConnectFeedback(t *testing.T) {
	r := newRig(t)
	okSrv, okCh := hookServer(t)
	if out := r.ag.call("rubi_link", map[string]any{"purpose": "agent:Mail"}); !strings.Contains(fmt.Sprint(out["send_with"]), "webhook field") {
		t.Fatalf("rubi_link for an agent doesn't say what to send with it: %v", out)
	}
	if wh := r.ag.call("rubi_status", nil)["webhook"].(map[string]any); !strings.Contains(fmt.Sprint(wh["note"]), "ONE message") {
		t.Fatalf("rubi_status doesn't tell the Bot to send both links at once: %v", wh)
	}
	link := r.panel("agent:Mail")
	var res map[string]any

	for _, key := range []string{"Bearer crsr_a\r\nX-Evil: 1", "crsr_a crsr_b", ""} {
		if err := link.Call("agent.add", map[string]any{"name": "Mail", "url": okSrv.URL, "key": key}, &res); err == nil {
			t.Fatalf("key %q accepted", key)
		}
	}
	if err := link.Call("agent.add", map[string]any{"name": "Mail", "url": "https://api2.cursor.sh@evil.example/automations/webhook/x", "key": "crsr_a"}, &res); err == nil {
		t.Fatal("a URL with userinfo was accepted")
	}

	connect := func(url string) {
		t.Helper()
		var res map[string]any
		if err := link.Call("agent.add", map[string]any{"name": "Mail", "url": url, "key": "Authorization: Bearer crsr_mail"}, &res); err != nil {
			t.Fatal(err)
		}
		pc := *link
		pc.Link.Ticket = res["ticket"].(string)
		info, err := pc.ApprovalInfo(res["approval_id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		var preview map[string]any
		_ = json.Unmarshal(info.Approval.Preview, &preview)
		if w, _ := preview["warning"].(string); !strings.Contains(w, "not a Grok Bot routine") {
			t.Fatalf("a local receiver was not flagged: %v", preview)
		}
		if out := r.approveChange(link, res); out["state"] != "executed" {
			t.Fatalf("connect: %v", out)
		}
	}
	test := func() map[string]any {
		t.Helper()
		var out map[string]any
		if err := link.Call("webhook.test", map[string]any{"name": "Mail", "wait": true}, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	connect(okSrv.URL)
	if out := test(); out["delivered"] != true || out["problem"] != nil {
		t.Fatalf("test to a working routine: %v", out)
	}
	if h := waitHit(t, okCh, "rubi.webhook.test"); h.auth != "Bearer crsr_mail" {
		t.Fatalf("test event auth %q", h.auth)
	}

	for status, want := range map[int]string{
		http.StatusUnauthorized: "refused the key",
		http.StatusBadRequest:   "switched off",
		http.StatusNotFound:     "no routine at this address",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		connect(srv.URL)
		out := test()
		if p, _ := out["problem"].(string); out["delivered"] != false || !strings.Contains(p, want) || strings.Contains(p, srv.URL) {
			t.Errorf("HTTP %d: %v", status, out)
		}
		srv.Close()
	}
}
