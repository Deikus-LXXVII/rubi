package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// HelloTimeout is how long a runner has to say hello; the runner itself gives up after 10 s too.
var HelloTimeout = 10 * time.Second

// NewToken makes a run token: 32 random bytes in hex, new for every start of a runner.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// AskParams is a runner's perm.ask.
type AskParams struct {
	AskID     string          `json:"ask_id"`
	SessionID string          `json:"session_id"`
	AgentID   string          `json:"agent_id,omitempty"`
	AgentType string          `json:"agent_type,omitempty"`
	Tool      string          `json:"tool"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Via       string          `json:"via,omitempty"`
	Input     json.RawMessage `json:"input"`
	Cwd       string          `json:"cwd,omitempty"`
	Mode      string          `json:"mode,omitempty"`
}

// AskResult is the answer to a perm.ask.
type AskResult struct {
	Behavior     string          `json:"behavior"`
	Message      string          `json:"message,omitempty"`
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
}

// CredParams is a runner's git.credential request: the git credential helper in the VM forwards `git
// credential get|store|erase` through the runner. Input is git's key=value text. Purpose is "push" when the
// helper was called for a push (the runner knows the command), anything else is a read.
type CredParams struct {
	Op      string `json:"op"`
	Input   string `json:"input"`
	Purpose string `json:"purpose,omitempty"`
}

// CredResult is git's answer text (empty for store and erase).
type CredResult struct {
	Output string `json:"output"`
}

// Event is a runner's event notification: Kind, the session it belongs to, and the whole object.
type Event struct {
	Kind      string
	SessionID string
	AgentID   string
	Done      bool
	Raw       json.RawMessage
}

// Handler receives what a runner sends. Ask must always return a decision.
type Handler interface {
	Ask(ctx context.Context, p AskParams) AskResult
	Event(ev Event)
	Credential(ctx context.Context, p CredParams) (CredResult, error)
	// Cancel: the runner gave up on a perm.ask (perm.cancel).
	Cancel(askID string)
}

// SSHHandler is implemented by a Handler that also serves the runner's ssh.* messages (ssh.go). notify sends
// a notification to this runner (ssh.data, ssh.ack, ssh.exit).
type SSHHandler interface {
	SSHOpen(ctx context.Context, p SSHOpenParams, notify func(method string, params any)) (*SSHOpened, error)
	SSHNote(method string, params json.RawMessage)
}

// Runner is the client of one runner process.
type Runner struct {
	conn Conn
	peer *peer
}

// Connect runs the handshake with a runner started with token and serves its requests with h. The
// runner's first line must be hello {run_token, version}; a wrong or missing token, a different first
// message or no hello in HelloTimeout ends it: stdin is closed and the process killed.
func Connect(ctx context.Context, conn Conn, token string, h Handler) (*Runner, error) {
	br := bufio.NewReader(conn)
	type first struct {
		line []byte
		err  error
	}
	ch := make(chan first, 1)
	go func() {
		l, err := readLine(br)
		ch <- first{l, err}
	}()
	refuse := func(err error) (*Runner, error) {
		_ = conn.CloseWrite()
		_ = conn.Close()
		return nil, err
	}
	var f first
	select {
	case f = <-ch:
	case <-time.After(HelloTimeout):
		return refuse(errors.New("the runner didn't say hello in time"))
	case <-ctx.Done():
		return refuse(ctx.Err())
	}
	if f.err != nil {
		return refuse(fmt.Errorf("the runner ended before hello: %w", f.err))
	}
	var m rpcMsg
	var hello struct {
		RunToken string `json:"run_token"`
		Version  string `json:"version"`
	}
	if json.Unmarshal(f.line, &m) != nil || m.Method != "hello" || len(m.ID) == 0 || json.Unmarshal(m.Params, &hello) != nil ||
		token == "" || subtle.ConstantTimeCompare([]byte(hello.RunToken), []byte(token)) != 1 {
		return refuse(errors.New("the runner's hello was refused"))
	}
	r := &Runner{conn: conn, peer: newPeer(conn)}
	sh, _ := h.(SSHHandler)
	r.peer.onReq = func(ctx context.Context, method string, params json.RawMessage) (any, error) {
		switch method {
		case "perm.ask":
			var p AskParams
			if json.Unmarshal(params, &p) != nil || p.Tool == "" || p.SessionID == "" {
				return AskResult{Behavior: "deny", Message: "Rubi: malformed permission request."}, nil
			}
			return h.Ask(ctx, p), nil
		case "git.credential":
			var p CredParams
			if json.Unmarshal(params, &p) != nil {
				return nil, &RPCError{Code: -32602, Message: "bad params"}
			}
			return h.Credential(ctx, p)
		case "ssh.open":
			var p SSHOpenParams
			if sh == nil || json.Unmarshal(params, &p) != nil {
				return nil, &RPCError{Code: -32602, Message: "bad params"}
			}
			return sh.SSHOpen(ctx, p, func(method string, params any) {
				if b, err := json.Marshal(params); err == nil {
					_ = r.peer.send(rpcMsg{Method: method, Params: b})
				}
			})
		}
		return nil, &RPCError{Code: -32601, Message: "method not found"}
	}
	r.peer.onNote = func(method string, params json.RawMessage) {
		switch method {
		case "ssh.data", "ssh.ack", "ssh.close":
			if sh != nil {
				sh.SSHNote(method, params)
			}
			return
		}
		if method == "perm.cancel" {
			var c struct {
				AskID string `json:"ask_id"`
			}
			if json.Unmarshal(params, &c) == nil && c.AskID != "" {
				h.Cancel(c.AskID)
			}
			return
		}
		if method != "event" {
			return
		}
		var e struct {
			SessionID string `json:"session_id"`
			Kind      string `json:"kind"`
			AgentID   string `json:"agent_id"`
			Done      bool   `json:"done"`
		}
		if json.Unmarshal(params, &e) != nil || e.Kind == "" {
			return
		}
		h.Event(Event{Kind: e.Kind, SessionID: e.SessionID, AgentID: e.AgentID, Done: e.Done, Raw: params})
	}
	if err := r.peer.send(rpcMsg{ID: m.ID, Result: json.RawMessage(`{"ok":true}`)}); err != nil {
		return refuse(err)
	}
	go r.peer.run(br)
	if rn, ok := conn.(ReadyNotifier); ok {
		rn.Ready()
	}
	go func() { // the process ending ends the channel
		select {
		case <-conn.Done():
			_ = conn.Close()
		case <-r.peer.done:
		}
	}()
	return r, nil
}

// Call makes a request of the runner.
func (r *Runner) Call(ctx context.Context, method string, params, result any) error {
	if params == nil {
		params = struct{}{}
	}
	return r.peer.call(ctx, method, params, result)
}

// Done is closed when the channel to the runner has ended.
func (r *Runner) Done() <-chan struct{} { return r.peer.done }

// Close ends the runner.
func (r *Runner) Close() error {
	_ = r.conn.CloseWrite()
	return r.conn.Close()
}
