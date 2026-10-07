package integrations

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Deikus-LXXVII/rubi/internal/approvals"
)

// Field is a non-secret setup value the panel asks for (e.g. an email address).
type Field struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Type        string `json:"type"` // "text" | "email"
	Placeholder string `json:"placeholder,omitempty"`
	Help        string `json:"help,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// Host is what the core offers an integration. It exists only while Rubi is unlocked.
type Host interface {
	ID() string
	// Settings decodes the integration's stored settings into v.
	Settings(v any) error
	Secret(key string) (string, error)
	// Level is the current approval level for an action kind.
	Level(kind string) approvals.Level
	// Submit gates an action behind the user's approval according to policy.
	Submit(ctx context.Context, req approvals.Request) (map[string]any, error)
	// Emit reports an event to the agent (and the configured webhook).
	Emit(typ string, data map[string]any, untrusted []string)
	Audit(event string, fields map[string]any)
	// LoadState/SaveState keep integration-private state encrypted in the vault.
	LoadState(v any) error
	SaveState(v any) error
	Logf(format string, args ...any)
}

// Runtime is an integration with behavior.
type Runtime interface {
	Integration
	// Tools registers the integration's MCP tools. Called once; tools stay listed in every state.
	Tools(r *Registrar)
	// Validate checks values the user entered in the panel (for example by logging in) and returns the
	// settings to store plus a one-line description of the connected account.
	Validate(ctx context.Context, fields, secrets map[string]string) (settings json.RawMessage, account string, err error)
	// Start begins background work after unlock; Stop ends it on lock or disconnect.
	Start(h Host) error
	Stop()
}

// Registrar lets integrations add MCP tools whose handlers receive a Host only when the integration
// can act. Otherwise the agent gets an explanation (locked, not connected) with the link to fix it.
type Registrar struct {
	Server  *mcp.Server
	Resolve func(integrationID string) (Host, map[string]any)
}

// AddTool registers a tool for an integration.
func AddTool[In any](r *Registrar, integrationID string, t *mcp.Tool,
	h func(ctx context.Context, host Host, in In) (map[string]any, error)) {
	mcp.AddTool(r.Server, t, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, map[string]any, error) {
		host, unavailable := r.Resolve(integrationID)
		if unavailable != nil {
			return nil, unavailable, nil
		}
		out, err := h(ctx, host, in)
		return nil, out, err
	})
}

// Run executes fn directly when the action kind needs no approval, and otherwise gates it.
// Integrations use it for actions that are usually free (reading, drafts) but that the user may restrict.
func Run(ctx context.Context, h Host, req approvals.Request) (map[string]any, error) {
	if h.Level(req.Kind) == approvals.None {
		res, err := req.Execute(ctx, req.Options[0].Key)
		if err != nil {
			return nil, err
		}
		if m, ok := res.(map[string]any); ok {
			return m, nil
		}
		return map[string]any{"result": res}, nil
	}
	return h.Submit(ctx, req)
}
