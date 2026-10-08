package mcpserver_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/plugins/plugintest"
)

type hookHit struct {
	typ  string
	auth string
	data map[string]any
}

func hookServer(t *testing.T) (*httptest.Server, chan hookHit) {
	ch := make(chan hookHit, 50)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		d, _ := m["data"].(map[string]any)
		ch <- hookHit{typ: toString(m["type"]), auth: req.Header.Get("Authorization"), data: d}
	}))
	t.Cleanup(s.Close)
	return s, ch
}

func waitHit(t *testing.T, ch chan hookHit, typ string) hookHit {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case h := <-ch:
			if h.typ == `"`+typ+`"` {
				return h
			}
		case <-deadline:
			t.Fatalf("no %s event", typ)
		}
	}
}

func noHit(t *testing.T, ch chan hookHit, typ string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case h := <-ch:
			if h.typ == `"`+typ+`"` {
				t.Fatalf("unexpected %s event: %v", typ, h)
			}
		case <-deadline:
			return
		}
	}
}

// TestSeveralAgents: Bots sharing one Rubi each get their own events.
func TestSeveralAgents(t *testing.T) {
	reg := plugintest.New(t)
	reg.Review("v1.0.0", reg.Publish("v1.0.0", nil, nil), "")
	r := newRig(t) // registers "Main" (default) through the older single-webhook setting
	mainSrv, mainCh := hookServer(t)
	mailSrv, mailCh := hookServer(t)
	r.setWebhook(mainSrv.URL)

	// The mail Bot registers itself through its own link; the paste may include the whole header.
	link := r.panel("agent:Mail")
	var res map[string]any
	if err := link.Call("agent.add", map[string]any{"name": "Other", "url": mailSrv.URL, "key": "x"}, &res); err == nil {
		t.Fatal("an agent link registered a different agent")
	}
	if err := link.Call("policy.set", map[string]any{"levels": map[string]string{}}, &res); err == nil {
		t.Fatal("an agent link changed other settings")
	}
	if err := link.Call("agent.add", map[string]any{"name": "mail", "url": mailSrv.URL, "key": "Authorization: Bearer crsr_mail"}, &res); err != nil {
		t.Fatal(err)
	}
	r.approveChange(link, res)
	agents := r.ag.call("rubi_status", nil)["webhook"].(map[string]any)["agents"].([]any)
	if len(agents) != 2 {
		t.Fatalf("agents: %v", agents)
	}

	r.approve(r.ag.call("rubi_plugin_install", map[string]any{"plugin": "demo"}), "install")
	r.connectDemo()
	r.waitFor("plugin", func() bool { return r.ag.call("demo_ping", nil)["started"] == true })
	waitHit(t, mainCh, "demo.ping") // unassigned plugin: the default agent

	// Route the plugin's events to the mail Bot.
	settings := r.panel("settings")
	if err := settings.Call("plugin.route", map[string]any{"id": "demo", "agent": "Mail"}, &res); err != nil {
		t.Fatal(err)
	}
	r.approveChange(settings, res)
	r.ag.call("demo_ping", nil)
	if h := waitHit(t, mailCh, "demo.ping"); h.auth != "Bearer crsr_mail" {
		t.Fatalf("mail Bot auth: %q", h.auth)
	}
	noHit(t, mainCh, "demo.ping")

	// An approval wakes the Bot that asked; unknown names are refused.
	out := r.ag.call("demo_send", map[string]any{"to": "x"})
	if e := r.ag.call("rubi_continue_after", map[string]any{"approval_id": out["approval_id"], "plan": "p", "agent": "Nobody"}); !strings.Contains(toString(e["tool_error"]), "no agent") {
		t.Fatalf("unknown agent: %v", e)
	}
	r.ag.call("rubi_continue_after", map[string]any{"approval_id": out["approval_id"], "plan": "tell the user", "agent": "Mail"})
	if r2, err := r.panelLink(out["approval_url"].(string)).ApproveWithPassword(out["approval_id"].(string), "send", pw); err != nil || r2["state"] != "executed" {
		t.Fatalf("approve: %v %v", r2, err)
	}
	if h := waitHit(t, mailCh, "rubi.approval.decided"); h.data["your_plan"] != "tell the user" {
		t.Fatalf("decision: %v", h)
	}
	noHit(t, mainCh, "rubi.approval.decided")
}
