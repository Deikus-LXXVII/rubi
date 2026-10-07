// Command demo is a tiny plugin used by Rubi's tests. It exercises every part of the plugin protocol.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/Deikus-LXXVII/rubi/sdk/rubiplugin"
)

// PublisherKey is replaced at build time by the tests.
var PublisherKey string

type echoIn struct {
	Text string `json:"text"`
}

type sendIn struct {
	To string `json:"to"`
}

func main() {
	p := rubiplugin.New(rubiplugin.Manifest{
		ID: "demo", Name: "Demo", Version: "v0.0.0", Description: "Test plugin.", Needs: "A token.",
		Publisher: rubiplugin.Publisher{Name: "Test Publisher", Key: PublisherKey},
		Entry:     "demo",
		Fields:    []rubiplugin.Field{{Key: "user", Label: "User", Type: "text"}},
		Secrets:   []rubiplugin.Secret{{Key: "token", Label: "Token"}},
		Actions: []rubiplugin.Action{
			{Kind: "demo.read", Title: "Read", DefaultLevel: rubiplugin.None},
			{Kind: "demo.send", Title: "Send", DefaultLevel: rubiplugin.Strong,
				Options: []rubiplugin.Option{{Key: "send", Label: "Send"}}},
		},
		Events: []rubiplugin.EventType{{Type: "ping", Untrusted: []string{"from"}}},
		Egress: []string{"example.com:443"},
	})
	p.Validate = func(_ context.Context, fields, secrets map[string]string) (any, string, error) {
		if secrets["token"] != "good" {
			return nil, "", errors.New("wrong token")
		}
		return map[string]string{"user": fields["user"]}, fields["user"], nil
	}
	started := false
	p.Start = func(h *rubiplugin.Host) error { started = true; return nil }

	rubiplugin.AddTool(p, "demo_echo", "Echo text back, with the stored user.",
		func(ctx context.Context, h *rubiplugin.Host, in echoIn) (any, error) {
			return h.Submit(ctx, rubiplugin.Request{Kind: "demo.read", Summary: "Echo", Preview: in, Payload: in})
		})
	rubiplugin.AddTool(p, "demo_send", "Pretend to send something (strong approval).",
		func(ctx context.Context, h *rubiplugin.Host, in sendIn) (any, error) {
			return h.Submit(ctx, rubiplugin.Request{Kind: "demo.send", Summary: "Send to " + in.To,
				Preview: map[string]string{"to": in.To}, Payload: in})
		})
	rubiplugin.AddTool(p, "demo_ping", "Emit a ping event.",
		func(ctx context.Context, h *rubiplugin.Host, _ struct{}) (any, error) {
			id, err := h.Emit("ping", map[string]any{"from": "someone"})
			return map[string]any{"event_id": id, "started": started}, err
		})
	rubiplugin.AddTool(p, "demo_secret", "Try to read secrets, including one that isn't declared.",
		func(ctx context.Context, h *rubiplugin.Host, _ struct{}) (any, error) {
			tok, err1 := h.Secret("token")
			_, err2 := h.Secret("other")
			return map[string]any{"token_ok": err1 == nil && tok == "good", "undeclared_refused": err2 != nil}, nil
		})
	rubiplugin.AddTool(p, "demo_crash", "Exit immediately.",
		func(ctx context.Context, h *rubiplugin.Host, _ struct{}) (any, error) {
			os.Exit(3)
			return nil, nil
		})

	p.OnExecute("demo.read", func(_ context.Context, h *rubiplugin.Host, _ string, payload json.RawMessage) (any, error) {
		var in echoIn
		_ = json.Unmarshal(payload, &in)
		var s struct{ User string }
		if err := h.Settings(&s); err != nil {
			return nil, err
		}
		return map[string]any{"echo": in.Text, "user": s.User}, nil
	})
	p.OnExecute("demo.send", func(_ context.Context, h *rubiplugin.Host, option string, payload json.RawMessage) (any, error) {
		var in sendIn
		_ = json.Unmarshal(payload, &in)
		var st struct{ Sent []string }
		_ = h.LoadState(&st)
		st.Sent = append(st.Sent, in.To)
		if err := h.SaveState(st); err != nil {
			return nil, err
		}
		h.Audit("sent", map[string]any{"to": in.To})
		return map[string]any{"sent_to": in.To, "option": option, "total": len(st.Sent)}, nil
	})
	p.Main()
}
