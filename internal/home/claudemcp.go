package home

// `rubi-home claude-mcp`: the stdio MCP server that gives Claude Code the sister tools (rubi_sisters,
// rubi_ask, rubi_reply, rubi_inbox_status). It reads no configuration and holds no secret: every call is one
// line to the running helper on claude.sock (see sistertools.go), which decides which session is calling and
// which sister is hers. It can't approve anything.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Deikus-LXXVII/rubi/internal/version"
)

// SessionEnvVar is the variable Claude Code sets for its stdio MCP servers (env-vars.md in its docs).
const SessionEnvVar = "CLAUDE_CODE_SESSION_ID"

// ToolsConfig is what the MCP server needs besides the socket.
type ToolsConfig struct {
	Sock string
	// Session returns the Claude Code session id ($CLAUDE_CODE_SESSION_ID when nil); Cwd the working
	// directory (os.Getwd when nil). Both are sent for diagnostics only: the helper attributes a call by the
	// kernel's view of the calling process (see peer.go), never by what the line says. Seams for tests.
	Session func() string
	Cwd     func() string
	// Wait bounds one call to the helper (claudeToolDeadline plus a margin when zero).
	Wait time.Duration
}

const toolsInstructions = "Tools to work with the sister: the Grok Bot the user bound to this Claude Code session (through Rubi). " +
	"rubi_ask hands her a small question or a focused audit; rubi_reply answers one message she sent; " +
	"rubi_sisters and rubi_inbox_status say who she is and what is pending. They add no permissions: nothing they return approves a command."

type (
	noArgs  struct{}
	askArgs struct {
		Text string `json:"text" jsonschema:"the question or audit request, at most 2000 characters; name files or commits instead of pasting code"`
		Kind string `json:"kind,omitempty" jsonschema:"research (web search, the default), lookup (one fact or page) or audit (look at something specific and report problems)"`
	}
	replyArgs struct {
		MessageID string `json:"message_id" jsonschema:"the id of a message the sister sent you (shown in the message header, or by rubi_inbox_status)"`
		Text      string `json:"text" jsonschema:"your answer to that message: short and self-contained, she can't see your tool calls"`
	}
)

// NewToolsServer builds the MCP server whose tools call the helper over cfg.Sock.
func NewToolsServer(cfg ToolsConfig) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "rubi-sisters", Title: "Rubi sister tools", Version: version.Version},
		&mcp.ServerOptions{Instructions: toolsInstructions})
	call := func(ctx context.Context, tool string, args map[string]any) (*mcp.CallToolResult, map[string]any, error) {
		res, err := callTool(ctx, cfg, tool, args)
		return nil, res, err
	}
	mcp.AddTool(srv, &mcp.Tool{Name: "rubi_sisters",
		Description: "Which Bot (the sister) is bound to this Claude Code session, since when, and who sent you messages in the last 24 hours. Read-only.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, map[string]any, error) {
			return call(ctx, "rubi_sisters", nil)
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "rubi_ask",
		Description: "Ask the sister a small question: web research, a fact lookup or a focused audit. Queued at once; her answer arrives later as a message at your next prompt or stop, so keep working. At most 3 per minute. Never hand her bulky work. Claude Code asks the user to allow each call.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(false)}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in askArgs) (*mcp.CallToolResult, map[string]any, error) {
			return call(ctx, "rubi_ask", map[string]any{"text": in.Text, "kind": in.Kind})
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "rubi_reply",
		Description: "Answer one message the sister sent you, now. Only for messages delivered to this session in the last 24 hours. This is your whole answer to that message: your final answer will not repeat it to her, and a message can be answered once. Claude Code asks the user to allow each call.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: boolPtr(false)}},
		func(ctx context.Context, _ *mcp.CallToolRequest, in replyArgs) (*mcp.CallToolResult, map[string]any, error) {
			return call(ctx, "rubi_reply", map[string]any{"message_id": in.MessageID, "text": in.Text})
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "rubi_inbox_status",
		Description: "The sister's messages for this session: how many wait, which replies you still owe, whether an idle session would be woken, and the recent states (queued, delivered, answered). Read-only.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, map[string]any, error) {
			return call(ctx, "rubi_inbox_status", nil)
		})
	return srv
}

func boolPtr(b bool) *bool { return &b }

// RunClaudeMCP serves the tools on stdin and stdout until they close.
func RunClaudeMCP(ctx context.Context, sock string) error {
	return NewToolsServer(ToolsConfig{Sock: sock}).Run(ctx, &mcp.StdioTransport{})
}

// ErrHelperTooOld is the tools' error for a helper that doesn't know RubiTool lines.
var ErrHelperTooOld = errors.New("the Rubi Home running on this computer is too old (or newer than this plugin) for the sister tools: update it with the installer, restart it, and run `~/.rubi-home/bin/rubi-home install-hook`")

// callTool sends one tool call to the helper and returns its result.
func callTool(ctx context.Context, cfg ToolsConfig, tool string, args map[string]any) (map[string]any, error) {
	sid := os.Getenv(SessionEnvVar)
	if cfg.Session != nil {
		sid = cfg.Session()
	}
	cwd := ""
	if cfg.Cwd != nil {
		cwd = cfg.Cwd()
	} else if wd, err := os.Getwd(); err == nil {
		cwd = wd
	}
	m := map[string]any{"hook_event_name": claudeToolEvent, "tool": tool, "claude_session_id": strings.TrimSpace(sid), "tool_cwd": cwd}
	for k, v := range args {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	wait := cfg.Wait
	if wait <= 0 {
		wait = claudeToolDeadline + 3*time.Second
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	conn, err := d.DialContext(ctx, "unix", cfg.Sock)
	if err != nil {
		return nil, errors.New("Rubi Home isn't running on this computer, so the sister tools can't work")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(wait))
	if _, err := conn.Write(append(stampHookLine(b), '\n')); err != nil {
		return nil, errors.New("Rubi Home closed the connection")
	}
	resp, err := readLine(conn, claudeMaxInput)
	if err != nil {
		return nil, errors.New("Rubi Home didn't answer in time")
	}
	var r map[string]any
	if json.Unmarshal(resp, &r) != nil {
		return nil, errors.New("Rubi Home gave an answer that couldn't be read")
	}
	if e, ok := r["error"].(string); ok && e != "" {
		return nil, errors.New(e)
	}
	if ok, _ := r["ok"].(bool); !ok { // {}: an older helper treats RubiTool as an unknown event
		return nil, ErrHelperTooOld
	}
	delete(r, "ok")
	return r, nil
}
