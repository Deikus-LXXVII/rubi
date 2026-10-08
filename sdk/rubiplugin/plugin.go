package rubiplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
)

// Version, when set at build time (-ldflags "-X github.com/Deikus-LXXVII/rubi/sdk/rubiplugin.Version=v1.2.3"),
// replaces the manifest version, so the release tag is the single source of truth.
var Version string

// Plugin is a Rubi plugin built with this SDK.
type Plugin struct {
	// Validate checks what the user entered in the panel (for example by logging in) and returns the
	// settings to store and a one-line description of the account. Required if the manifest has fields
	// or secrets.
	Validate func(ctx context.Context, fields, secrets map[string]string) (settings any, account string, err error)
	// Start begins background work once the plugin is connected; Stop ends it. Both are optional.
	Start func(h *Host) error
	Stop  func()
	// ConfigOptions supplies the options of Dynamic config fields (e.g. the folders of one mailbox).
	// account is set for PerAccount fields.
	ConfigOptions func(ctx context.Context, h *Host, account, key string) ([]Option, error)

	m     Manifest
	tools map[string]func(ctx context.Context, h *Host, args json.RawMessage) (any, error)
	execs map[string]func(ctx context.Context, h *Host, option string, payload json.RawMessage) (any, error)

	mu      sync.Mutex
	host    *Host
	started bool
}

// New creates a plugin. Schema, API and (if set) Version are filled in automatically.
func New(m Manifest) *Plugin {
	m.Schema, m.API = 1, API
	if Version != "" {
		m.Version = Version
	}
	return &Plugin{m: m, tools: map[string]func(context.Context, *Host, json.RawMessage) (any, error){},
		execs: map[string]func(context.Context, *Host, string, json.RawMessage) (any, error){}}
}

// Manifest returns the manifest including the tools added so far.
func (p *Plugin) Manifest() Manifest { return p.m }

// AddTool adds a tool. Its input schema is inferred from In (use `json` and `jsonschema` struct tags).
// The name must start with the plugin's tool prefix (see ToolPrefix).
func AddTool[In any](p *Plugin, name, description string, fn func(ctx context.Context, h *Host, in In) (any, error)) {
	schema, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("tool %s: input schema: %v", name, err))
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		panic(err)
	}
	p.m.Tools = append(p.m.Tools, Tool{Name: name, Description: description, InputSchema: raw})
	p.tools[name] = func(ctx context.Context, h *Host, args json.RawMessage) (any, error) {
		var in In
		if len(args) > 0 && string(args) != "null" {
			if err := json.Unmarshal(args, &in); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
		}
		return fn(ctx, h, in)
	}
}

// OnExecute runs an approved action of kind. payload is exactly what was passed to Host.Submit.
func (p *Plugin) OnExecute(kind string, fn func(ctx context.Context, h *Host, option string, payload json.RawMessage) (any, error)) {
	p.execs[kind] = fn
}

// Main is the plugin's entry point. With --manifest it prints the manifest (used by release workflows);
// otherwise it serves Rubi on stdin/stdout until Rubi shuts it down.
func (p *Plugin) Main() {
	if len(os.Args) > 1 && os.Args[1] == "--manifest" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		_ = enc.Encode(p.m)
		return
	}
	if err := p.Serve(context.Background(), os.Stdin, os.Stdout); err != nil && !errors.Is(err, io.EOF) {
		fmt.Fprintln(os.Stderr, "plugin stopped:", err)
		os.Exit(1)
	}
}

// Serve speaks the plugin protocol on r and w.
func (p *Plugin) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	conn := NewConn(r, w, nil)
	p.host = &Host{conn: conn, id: p.m.ID}
	conn.handler = p.handle
	err := conn.Run(ctx)
	p.stop()
	return err
}

func (p *Plugin) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	h := p.host
	switch method {
	case "initialize":
		var in InitializeParams
		_ = json.Unmarshal(params, &in)
		if in.API != API {
			return nil, fmt.Errorf("this plugin speaks API %d, Rubi offered %d", API, in.API)
		}
		return map[string]any{"api": API}, nil
	case "validate":
		if p.Validate == nil {
			return ValidateResult{Settings: json.RawMessage("{}")}, nil
		}
		var in ValidateParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, err
		}
		if in.Secrets == nil {
			in.Secrets = map[string]string{}
		}
		settings, account, err := p.Validate(ctx, in.Fields, in.Secrets)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(settings)
		if err != nil {
			return nil, err
		}
		return ValidateResult{Settings: raw, Account: account, Secrets: in.Secrets}, nil
	case "start":
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.started {
			return nil, nil
		}
		if p.Start != nil {
			if err := p.Start(h); err != nil {
				return nil, err
			}
		}
		p.started = true
		return nil, nil
	case "stop":
		p.stop()
		return nil, nil
	case "config.options":
		var in struct {
			Key     string `json:"key"`
			Account string `json:"account"`
		}
		_ = json.Unmarshal(params, &in)
		if p.ConfigOptions == nil {
			return map[string]any{"options": []Option{}}, nil
		}
		opts, err := p.ConfigOptions(ctx, p.host, in.Account, in.Key)
		if err != nil {
			return nil, err
		}
		return map[string]any{"options": opts}, nil
	case "tool":
		var in ToolParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, err
		}
		fn := p.tools[in.Name]
		if fn == nil {
			return nil, &Error{Code: -32601, Message: "unknown tool " + in.Name}
		}
		return fn(ctx, h, in.Arguments)
	case "execute":
		var in ExecuteParams
		if err := json.Unmarshal(params, &in); err != nil {
			return nil, err
		}
		fn := p.execs[in.Kind]
		if fn == nil {
			return nil, fmt.Errorf("no executor for %s", in.Kind)
		}
		return fn(ctx, h, in.Option, in.Payload)
	case "shutdown":
		// Rubi closes stdin after this answer; Serve then returns and the process exits.
		p.stop()
		return nil, nil
	}
	return nil, &Error{Code: -32601, Message: "method not found: " + method}
}

func (p *Plugin) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started && p.Stop != nil {
		p.Stop()
	}
	p.started = false
}

// Request describes an action that may need the user's approval.
type Request = SubmitParams

// Host is Rubi, as seen from a plugin. Everything goes through Rubi and stays in the plugin's namespace.
type Host struct {
	conn *Conn
	id   string
}

// ID is the plugin id.
func (h *Host) ID() string { return h.id }

// Settings decodes the default account's settings (as returned by Validate).
func (h *Host) Settings(v any) error { return h.SettingsFor("", v) }

// SettingsFor decodes one account's settings ("" = the default account).
func (h *Host) SettingsFor(account string, v any) error {
	var out struct {
		Settings json.RawMessage `json:"settings"`
	}
	if err := h.conn.Call(context.Background(), "settings.get", map[string]string{"account": account}, &out); err != nil {
		return err
	}
	return json.Unmarshal(out.Settings, v)
}

// Secret returns a secret of the default account.
func (h *Host) Secret(key string) (string, error) { return h.SecretFor("", key) }

// SecretFor returns a secret declared in the manifest, for one account ("" = the default account).
func (h *Host) SecretFor(account, key string) (string, error) {
	var out struct {
		Value string `json:"value"`
	}
	err := h.conn.Call(context.Background(), "secret.get", map[string]string{"key": key, "account": account}, &out)
	return out.Value, err
}

// Accounts lists the connected accounts, the default one first.
func (h *Host) Accounts() ([]AccountInfo, error) {
	var out struct {
		Accounts []AccountInfo `json:"accounts"`
	}
	err := h.conn.Call(context.Background(), "accounts.list", nil, &out)
	return out.Accounts, err
}

// LoadState decodes the plugin's private state (kept encrypted by Rubi). v is left alone if none is saved.
func (h *Host) LoadState(v any) error {
	var out struct {
		State json.RawMessage `json:"state"`
	}
	if err := h.conn.Call(context.Background(), "state.get", nil, &out); err != nil {
		return err
	}
	if len(out.State) == 0 || string(out.State) == "null" {
		return nil
	}
	return json.Unmarshal(out.State, v)
}

// SaveState replaces the plugin's private state.
func (h *Host) SaveState(v any) error {
	return h.conn.Call(context.Background(), "state.set", map[string]any{"state": v}, nil)
}

// Level is the current approval level for one of the plugin's action kinds.
func (h *Host) Level(kind string) Level {
	var out struct {
		Level Level `json:"level"`
	}
	if err := h.conn.Call(context.Background(), "level.get", map[string]string{"kind": kind}, &out); err != nil {
		return Strong
	}
	return out.Level
}

// Submit asks for approval of an action. If its kind needs no approval, Rubi executes it right away
// (calling the OnExecute handler) and Submit returns the result. Otherwise it returns what the agent
// needs to get the user's approval; return that from the tool unchanged.
func (h *Host) Submit(ctx context.Context, r Request) (map[string]any, error) {
	var out map[string]any
	err := h.conn.Call(ctx, "approval.submit", r, &out)
	return out, err
}

// Emit tells the agent about something (for example a reply arrived). typ must be declared in the manifest.
// The event goes to the Bots that subscribed to this plugin's events.
func (h *Host) Emit(typ string, data map[string]any) (string, error) {
	return h.EmitTo("", typ, data)
}

// EmitTo sends an event to one Bot by its name (for example the Bot that set up a watch), or, if no Bot of
// that name is connected, like Emit.
func (h *Host) EmitTo(agent, typ string, data map[string]any) (string, error) {
	var out struct {
		EventID string `json:"event_id"`
	}
	err := h.conn.Call(context.Background(), "event.emit", map[string]any{"type": typ, "data": data, "agent": agent}, &out)
	return out.EventID, err
}

// Config decodes the user-only settings (manifest Config) for the default account, with defaults for
// anything not set.
func (h *Host) Config(v any) error { return h.ConfigFor("", v) }

// ConfigFor decodes the user-only settings as they apply to one account: the plugin-wide ones plus that
// account's PerAccount ones.
func (h *Host) ConfigFor(account string, v any) error {
	var out struct {
		Config json.RawMessage `json:"config"`
	}
	if err := h.conn.Call(context.Background(), "config.get", map[string]string{"account": account}, &out); err != nil {
		return err
	}
	return json.Unmarshal(out.Config, v)
}

// Audit records an entry in Rubi's audit log. Never put secrets or message bodies in it.
func (h *Host) Audit(event string, fields map[string]any) {
	_ = h.conn.Call(context.Background(), "audit", map[string]any{"event": event, "fields": fields}, nil)
}

// Logf writes to the plugin's log (stderr, which Rubi saves to logs/plugin-<id>.log).
func (h *Host) Logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}
