// Package rubiplugin is the Go SDK for Rubi plugins.
//
// A Rubi plugin is a separate executable that Rubi starts while it is unlocked. It talks to Rubi over
// JSON-RPC on stdin/stdout (see docs/design/plugins.md for the protocol, which any language can speak).
// Rubi holds the user's secrets, approvals and events; the plugin asks for what it needs and never sees
// anything outside its own namespace.
//
// A minimal plugin:
//
//	p := rubiplugin.New(rubiplugin.Manifest{ID: "hello", Name: "Hello", ...})
//	rubiplugin.AddTool(p, "hello_greet", "Say hello.", func(ctx context.Context, h *rubiplugin.Host, in struct{ Name string }) (any, error) {
//		return map[string]any{"greeting": "Hello, " + in.Name}, nil
//	})
//	p.Main()
package rubiplugin

import (
	"encoding/json"
	"strings"
)

// API is the protocol version this SDK speaks.
const API = 1

// Level is how an action is approved.
type Level string

const (
	None   Level = "none"   // runs without asking
	Chat   Level = "chat"   // the user presses a button in the agent chat
	Strong Level = "strong" // the user approves in the Rubi panel with Face ID or a password
)

type Manifest struct {
	Schema      int         `json:"schema"`
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Version     string      `json:"version"`
	Description string      `json:"description"`
	Needs       string      `json:"needs,omitempty"` // one sentence the agent tells the user before setup
	Publisher   Publisher   `json:"publisher"`
	Source      string      `json:"source,omitempty"`
	MinRubi     string      `json:"min_rubi,omitempty"`
	API         int         `json:"api"`
	Entry       string      `json:"entry"` // executable inside the package
	Fields      []Field     `json:"fields,omitempty"`
	Secrets     []Secret    `json:"secrets,omitempty"`
	Actions     []Action    `json:"actions,omitempty"`
	Events      []EventType `json:"events,omitempty"`
	Egress      []string    `json:"egress,omitempty"`
	Tools       []Tool      `json:"tools,omitempty"`
	// Config are settings only the user can change, in the Rubi panel and with their approval (for example
	// a privacy filter). The agent can't read or change them through Rubi; the plugin reads them.
	Config []ConfigField `json:"config,omitempty"`
	// Hooks lets the plugin hand out private web addresses (e.g. for iPhone Shortcuts) whose requests
	// Rubi passes to it. Shown to the user before install.
	Hooks bool `json:"hooks,omitempty"`
	// Home lists what the plugin does through the user's Rubi Home helper: "hue", "shortcuts". It may
	// only call those operations. Shown to the user before install.
	Home []string `json:"home,omitempty"`
}

// ConfigField is one user-only setting.
type ConfigField struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	// Type is "bool", "text", "choice" (one of Options) or "list" (a list of strings: with Options, a
	// subset of them; without, free text edited one per line). "info" is read-only: the panel shows its
	// (Dynamic) Options as label and value, with a copy button, e.g. addresses to paste into a Shortcut.
	Type    string   `json:"type"`
	Default any      `json:"default,omitempty"`
	Help    string   `json:"help,omitempty"`
	Options []Option `json:"options,omitempty"`
	// Dynamic options come from the plugin when the panel opens the settings (Plugin.ConfigOptions), e.g.
	// the user's mail folders.
	Dynamic bool `json:"dynamic,omitempty"`
	// PerAccount settings are kept separately for each connected account (e.g. which folders of that
	// mailbox the agent may see); the others apply to the whole plugin.
	PerAccount bool `json:"per_account,omitempty"`
}

// AccountInfo is one connected account.
type AccountInfo struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Default bool   `json:"default,omitempty"`
}

type Publisher struct {
	Name string `json:"name"`
	Key  string `json:"key"` // base64 Ed25519 public key (SubjectPublicKeyInfo DER)
	URL  string `json:"url,omitempty"`
}

// Field is a non-secret setup value the panel asks for (e.g. an email address).
type Field struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"` // "text" | "email" | "tel" | "number" | "choice"
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
	Required    bool   `json:"required,omitempty"`
	// Options are the choices of a "choice" field.
	Options []Option `json:"options,omitempty"`
}

// Secret is a value the user enters in the panel and Rubi keeps encrypted (e.g. an app password).
type Secret struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Help    string `json:"help,omitempty"`
	HelpURL string `json:"help_url,omitempty"`
	// HelpLink is the label of the HelpURL button, e.g. "Open Google app passwords".
	HelpLink string `json:"help_link,omitempty"`
	// Optional secrets may be left empty.
	Optional bool `json:"optional,omitempty"`
	// Internal secrets aren't asked in the setup form: the plugin fills them during setup (e.g. a login
	// session obtained in a later NeedMore step) and returns them for safekeeping.
	Internal bool `json:"internal,omitempty"`
}

type Action struct {
	Kind         string `json:"kind"` // "<plugin id>.<action>"
	Title        string `json:"title"`
	DefaultLevel Level  `json:"default_level"`
	// Locked keeps the default level: the user can't lower it (e.g. revealing a private email).
	Locked  bool     `json:"locked,omitempty"`
	Options []Option `json:"options,omitempty"`
}

// Option is one way to approve an action, shown as a button (plain text, no emoji).
type Option struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Meaning string `json:"meaning,omitempty"`
}

type EventType struct {
	Type string `json:"type"`
	// Untrusted lists data fields that come from third parties, so the agent treats them as data.
	Untrusted []string `json:"untrusted_fields,omitempty"`
}

type Tool struct {
	Name        string          `json:"name"` // "<plugin id with _ for ->_<tool>"
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolPrefix is the prefix every tool name of plugin id must have.
func ToolPrefix(id string) string { return strings.ReplaceAll(id, "-", "_") + "_" }

// Action returns the declared action for kind.
func (m *Manifest) Action(kind string) (Action, bool) {
	for _, a := range m.Actions {
		if a.Kind == kind {
			return a, true
		}
	}
	return Action{}, false
}

// Event returns the declared event type.
func (m *Manifest) Event(typ string) (EventType, bool) {
	for _, e := range m.Events {
		if e.Type == typ {
			return e, true
		}
	}
	return EventType{}, false
}

// Tool returns the declared tool.
func (m *Manifest) Tool(name string) (Tool, bool) {
	for _, t := range m.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// ---- protocol messages ----

type InitializeParams struct {
	API         int    `json:"api"`
	RubiVersion string `json:"rubi_version"`
	PluginID    string `json:"plugin_id"`
}

type ValidateParams struct {
	Fields  map[string]string `json:"fields"`
	Secrets map[string]string `json:"secrets"`
}

type ValidateResult struct {
	Settings json.RawMessage `json:"settings"`
	Account  string          `json:"account"`
	// AccountID, if set, identifies the account instead of its name (two accounts may share a name).
	AccountID string `json:"account_id,omitempty"`
	// Secrets, if set, replaces what is stored (for example trimmed of whitespace).
	Secrets map[string]string `json:"secrets,omitempty"`
	// NeedMore, if set, means the account isn't connected yet: the user must enter more (see NeedMore).
	NeedMore *NeedMore `json:"need_more,omitempty"`
}

// StepField is the field that carries NeedMore.Step back to Validate.
const StepField = "_step"

// NeedMore, returned as the error from Validate, asks the user for more before the account can be
// connected, e.g. the login code a service just sent. The panel shows Message and the new inputs, then
// calls Validate again with the fields entered so far, the new ones, and fields[StepField] = Step. The
// plugin keeps whatever it needs between the steps (in memory, keyed by Step).
type NeedMore struct {
	Message string   `json:"message"`
	Fields  []Field  `json:"fields,omitempty"`
	Secrets []Secret `json:"secrets,omitempty"`
	Step    string   `json:"step"`
}

func (n *NeedMore) Error() string { return "more input needed: " + n.Message }

type ToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ExecuteParams struct {
	Kind    string          `json:"kind"`
	Option  string          `json:"option"`
	Payload json.RawMessage `json:"payload"`
}

type SubmitParams struct {
	Kind     string   `json:"kind"`
	Summary  string   `json:"summary"`
	Question string   `json:"question,omitempty"`
	Preview  any      `json:"preview"`
	Options  []Option `json:"options"`
	// Items make a batch: the user approves any subset at once. The executor then gets the option
	// "items:<key>,<key>…" with the chosen keys (see ChosenItems).
	Items   []Item `json:"items,omitempty"`
	Payload any    `json:"payload,omitempty"`
}

// Item is one entry of a batch request.
type Item struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Preview any    `json:"preview,omitempty"`
}

// ChosenItems returns the item keys the user approved in a batch ("items:a,b" -> [a b]).
func ChosenItems(option string) []string {
	rest, ok := strings.CutPrefix(option, "items:")
	if !ok || rest == "" {
		return nil
	}
	return strings.Split(rest, ",")
}
