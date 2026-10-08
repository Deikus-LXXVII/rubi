// Command demo is a tiny plugin used by Rubi's tests. It exercises every part of the plugin protocol.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		Secrets:   []rubiplugin.Secret{{Key: "token", Label: "Token"}, {Key: "session", Label: "Session", Internal: true, Renewable: true}},
		Actions: []rubiplugin.Action{
			{Kind: "demo.read", Title: "Read", DefaultLevel: rubiplugin.None},
			{Kind: "demo.send", Title: "Send", DefaultLevel: rubiplugin.Strong,
				Options: []rubiplugin.Option{{Key: "send", Label: "Send"}}},
			{Kind: "demo.secret", Title: "Reveal a secret", DefaultLevel: rubiplugin.Strong, Locked: true},
		},
		Config: []rubiplugin.ConfigField{
			{Key: "hidden", Label: "Hidden words", Type: "list", Default: []string{"code"}},
			{Key: "strict", Label: "Strict mode", Type: "bool", Default: true},
			{Key: "mode", Label: "Folders your agent can see", Type: "choice", Default: "all",
				Options: []rubiplugin.Option{{Key: "all", Label: "All folders"}, {Key: "selected", Label: "Only the folders checked below"}}},
			{Key: "folders", Label: "Allowed folders", Type: "list", Dynamic: true, PerAccount: true, Default: []string{"INBOX"}},
		},
		Events: []rubiplugin.EventType{{Type: "ping", Untrusted: []string{"from"}}, {Type: "hooked", Untrusted: []string{"body"}}},
		Egress: []string{"example.com:443"},
		Hooks:  true,
		Home:   []string{"shortcuts"},
	})
	rubiplugin.AddTool(p, "demo_session", "Store and read back an internal secret; also try to overwrite the user's token.",
		func(ctx context.Context, h *rubiplugin.Host, in struct {
			Value string `json:"value"`
		}) (any, error) {
			before, _ := h.SecretFor("", "session")
			if err := h.SetSecretFor("", "session", in.Value); err != nil {
				return nil, err
			}
			after, _ := h.SecretFor("", "session")
			tokenErr := h.SetSecretFor("", "token", "stolen")
			token, _ := h.SecretFor("", "token")
			return map[string]any{"before": before, "after": after, "token_refused": tokenErr != nil, "token": token}, nil
		})
	rubiplugin.AddTool(p, "demo_home", "Run a Rubi Home operation.",
		func(ctx context.Context, h *rubiplugin.Host, in struct {
			Op   string         `json:"op"`
			Args map[string]any `json:"args,omitempty"`
		}) (any, error) {
			var out any
			if err := h.HomeCall(ctx, "", in.Op, in.Args, &out); err != nil {
				return map[string]any{"error": err.Error(), "code": rubiplugin.ErrorCode(err)}, nil
			}
			return map[string]any{"out": out}, nil
		})
	p.OnHook = func(ctx context.Context, h *rubiplugin.Host, ev rubiplugin.HookEvent) error {
		_, err := h.Emit("hooked", map[string]any{"name": ev.Name, "account": ev.Account, "body": ev.Body})
		return err
	}
	rubiplugin.AddTool(p, "demo_hook", "The address of a hook.",
		func(ctx context.Context, h *rubiplugin.Host, in struct {
			Name   string `json:"name"`
			Rotate bool   `json:"rotate,omitempty"`
		}) (any, error) {
			u, err := h.HookURL("", in.Name, in.Rotate)
			return map[string]any{"url": u}, err
		})
	p.Validate = func(_ context.Context, fields, secrets map[string]string) (any, string, error) {
		if secrets["token"] == "2fa" { // a two-step login: the code comes in a second step
			if fields[rubiplugin.StepField] != "code-sent" {
				return nil, "", &rubiplugin.NeedMore{Message: "Enter the code we sent.", Step: "code-sent",
					Fields: []rubiplugin.Field{{Key: "code", Label: "Code", Type: "number", Required: true}}}
			}
			if fields["code"] != "123" {
				return nil, "", errors.New("wrong code")
			}
			return map[string]string{"user": fields["user"]}, fields["user"], nil
		}
		if secrets["token"] != "good" {
			return nil, "", errors.New("wrong token")
		}
		return map[string]string{"user": fields["user"]}, fields["user"], nil
	}
	p.ConfigOptions = func(_ context.Context, _ *rubiplugin.Host, _ string, key string) ([]rubiplugin.Option, error) {
		if key != "folders" {
			return nil, nil
		}
		return []rubiplugin.Option{{Key: "INBOX", Label: "INBOX"}, {Key: "Archive", Label: "Archive"}, {Key: "Receipts", Label: "Receipts"}}, nil
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
	rubiplugin.AddTool(p, "demo_config", "Show the user-only settings as the plugin sees them.",
		func(ctx context.Context, h *rubiplugin.Host, _ struct{}) (any, error) {
			var cfg map[string]any
			err := h.Config(&cfg)
			return cfg, err
		})
	rubiplugin.AddTool(p, "demo_notify", "Emit a ping to one Bot.",
		func(ctx context.Context, h *rubiplugin.Host, in struct {
			Agent string `json:"agent"`
		}) (any, error) {
			id, err := h.EmitTo(in.Agent, "ping", map[string]any{"from": "someone"})
			return map[string]any{"event_id": id}, err
		})
	rubiplugin.AddTool(p, "demo_batch", "Ask to send to several people at once (a batch approval).",
		func(ctx context.Context, h *rubiplugin.Host, in struct {
			To []string `json:"to"`
		}) (any, error) {
			var items []rubiplugin.Item
			for _, to := range in.To {
				items = append(items, rubiplugin.Item{Key: to, Label: "Send to " + to, Preview: map[string]string{"to": to}})
			}
			return h.Submit(ctx, rubiplugin.Request{Kind: "demo.send", Summary: "Send to several people",
				Preview: map[string]string{"count": fmt.Sprint(len(items))}, Items: items, Payload: in})
		})
	rubiplugin.AddTool(p, "demo_who", "Which account a call uses, with its settings and folders.",
		func(ctx context.Context, h *rubiplugin.Host, in struct {
			Account string `json:"account,omitempty"`
		}) (any, error) {
			var s struct{ User string }
			if err := h.SettingsFor(in.Account, &s); err != nil {
				return nil, err
			}
			tok, err := h.SecretFor(in.Account, "token")
			if err != nil {
				return nil, err
			}
			var cfg map[string]any
			_ = h.ConfigFor(in.Account, &cfg)
			accts, _ := h.Accounts()
			return map[string]any{"user": s.User, "token_ok": tok != "", "folders": cfg["folders"], "accounts": len(accts)}, nil
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
		if chosen := rubiplugin.ChosenItems(option); chosen != nil {
			return map[string]any{"sent_to": chosen}, nil
		}
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
