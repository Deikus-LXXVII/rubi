package t3vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/Deikus-LXXVII/rubi/internal/home/agent"
)

// Asker is the approval path of the runner's perm.ask (agent.Manager.AskTool): the gate adds nothing to it.
type Asker interface {
	AskTool(ctx context.Context, project, role string, a agent.AskParams) agent.AskResult
}

// Target says whose approval the calls of one VM belong to: the project and the role key the manager files the ask
// under, and the workspace the VM was started for (used only to notice a hook input from somewhere else).
type Target struct {
	Project string
	Role    string
	Cwd     string
	// Thread is the T3 thread the VM serves (T3 names it in T3_AGENT_THREAD). When set, asks are filed under it instead
	// of under claude's own session id, so a plugin sees the id the thread is known by.
	Thread string
}

const (
	// DefaultGateWait is how long the gate waits for a decision, below the manager's wait, the script's and the hook's.
	DefaultGateWait = 285 * time.Second
	// MaxHookBody bounds a hook input (the tool input has its own limit in the manager).
	MaxHookBody = 2 << 20
)

// Gate is the handler served on one VM's hook service: POST /ask with Claude Code's PreToolUse input, answered in the
// PreToolUse shape. It is made for one VM (NewGate, bound to that VM's PipeSpec.Hook listener) and files every ask
// under that VM's target: the session id and cwd in the input are claimed by the code inside the VM and never decide
// whose approval it is (a cwd that differs from the target's is logged). Everything it can't vouch for is a deny with
// a reason: malformed input, an event that isn't PreToolUse, a body that is too large, no answer in time.
type Gate struct {
	asker  Asker
	target Target
	// Wait bounds the decision (DefaultGateWait when zero).
	Wait time.Duration
	// Logf receives one line per deny that the gate made itself (never the tool input); log.Printf when nil.
	Logf func(format string, args ...any)
	// ThreadOf, on a VM several sessions share, is the T3 thread of a claude session id ("" when none): the session ids
	// are those Home started claude with (--session-id), so a hook input can at most name another session of the same
	// VM, whose asks belong to the same project and role anyway. Target.Thread is a VM's one thread instead.
	ThreadOf func(sessionID string) string
}

// NewGate makes the gate of the VM whose calls are asked about under target.
func NewGate(asker Asker, target Target) *Gate { return &Gate{asker: asker, target: target} }

// hookInput is Claude Code's PreToolUse input (runner/src/sdk.ts PreToolUseInput).
type hookInput struct {
	HookEventName  string          `json:"hook_event_name"`
	SessionID      string          `json:"session_id"`
	Cwd            string          `json:"cwd"`
	PermissionMode string          `json:"permission_mode"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolUseID      string          `json:"tool_use_id"`
	AgentID        string          `json:"agent_id"`
	AgentType      string          `json:"agent_type"`
}

func (g *Gate) logf(format string, args ...any) {
	if g.Logf != nil {
		g.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func hookDeny(reason string) map[string]any {
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": reason}}
}

func (g *Gate) answer(w http.ResponseWriter, v map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (g *Gate) refuse(w http.ResponseWriter, why, reason string) {
	g.logf("t3vm: hook gate denied a call: %s", why)
	g.answer(w, hookDeny("Rubi: "+reason+", so the call was denied."))
}

// ServeHTTP implements http.Handler.
func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/ask" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxHookBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			g.refuse(w, "body too large", "the call is too large to review")
			return
		}
		g.refuse(w, "unreadable body", "the call could not be read")
		return
	}
	var in hookInput
	if err := json.Unmarshal(body, &in); err != nil {
		g.refuse(w, "malformed input", "the approval request was malformed")
		return
	}
	if in.HookEventName != "PreToolUse" || in.ToolName == "" || !isObject(in.ToolInput) || in.SessionID == "" {
		g.refuse(w, "not a PreToolUse input", "the approval request was incomplete")
		return
	}
	// AskUserQuestion is T3's own prompt to the person; the runner leaves it to the permission callback too.
	if in.ToolName == "AskUserQuestion" {
		g.answer(w, map[string]any{})
		return
	}
	if g.asker == nil || g.target.Project == "" {
		g.refuse(w, "no asker", "Rubi has no approval path for this session")
		return
	}
	if g.target.Cwd != "" && in.Cwd != "" && in.Cwd != g.target.Cwd && !within(g.target.Cwd, in.Cwd) {
		g.logf("t3vm: hook input of role %s claims cwd %q, the VM was started for %q", g.target.Role, in.Cwd, g.target.Cwd)
	}
	wait := g.Wait
	if wait <= 0 {
		wait = DefaultGateWait
	}
	// r.Context() ends when the hook (and so the call) is gone: the ask is dropped with it.
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	session := in.SessionID
	if g.target.Thread != "" {
		session = g.target.Thread
	} else if g.ThreadOf != nil {
		if t := g.ThreadOf(in.SessionID); t != "" {
			session = t
		}
	}
	res := g.asker.AskTool(ctx, g.target.Project, g.target.Role, agent.AskParams{
		SessionID: session, AgentID: in.AgentID, AgentType: in.AgentType, Tool: in.ToolName, ToolUseID: in.ToolUseID,
		Via: "hook", Input: in.ToolInput, Cwd: in.Cwd, Mode: in.PermissionMode,
	})
	switch {
	case res.Behavior != "allow":
		msg := res.Message
		if msg == "" {
			msg = "Denied by Rubi"
		}
		g.answer(w, hookDeny(msg))
	case len(res.UpdatedInput) > 0:
		// The changed input goes back without a decision (as the runner does): the SDK's own rules still apply.
		// Checked with the real CLI (claude 2.1.314 in the runner image, bypassPermissions, a Bash call): a command
		// hook's updatedInput without permissionDecision is honoured and the changed command is what runs.
		g.answer(w, map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "updatedInput": res.UpdatedInput}})
	default:
		// An allow is never returned from the hook: T3's own approval for the mode still applies after Rubi's.
		g.answer(w, map[string]any{})
	}
}

func isObject(b json.RawMessage) bool {
	b = bytes.TrimSpace(b)
	return len(b) > 1 && b[0] == '{'
}
