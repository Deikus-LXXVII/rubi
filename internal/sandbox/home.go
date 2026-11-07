package sandbox

import (
	"crypto/subtle"
	"encoding/json"
	"strings"
	"sync"

	"github.com/Deikus-LXXVII/rubi/internal/homeproto"
)

// SignInCode is the only code the fake Claude page accepts. It is a test fixture, not a credential.
const SignInCode = "sandbox-code"

// Home is a Rubi Home that advertises t3-setup and walks the first-time setup in fixed stages.
// It never opens a browser. The sign-in step hands the panel a URL on the sandbox OAuth stub.
type Home struct {
	Name      string
	SignInURL string

	mu       sync.Mutex
	secret   string
	token    string
	order    []string
	state    map[string]string
	signedIn bool
	login    bool
}

// NewHome starts every step not done except the container, which the sandbox stubs as already installed.
// The container install on a real Mac asks for an administrator password this machine does not have.
func NewHome(name, secret, signInURL string) *Home {
	return &Home{
		Name: name, SignInURL: signInURL, secret: secret,
		order: []string{"container", "service", "image", "t3", "pair", "login", "selftest"},
		state: map[string]string{
			"container": "done", "service": "todo", "image": "todo", "t3": "todo",
			"pair": "todo", "login": "todo", "selftest": "todo",
		},
	}
}

func (h *Home) Handle(env homeproto.Envelope) (any, error) {
	switch env.Op {
	case "pair":
		return h.pair(env.Args)
	}
	if !h.paired(env.Token) {
		return nil, errString(homeproto.NotPairedMessage)
	}
	switch env.Op {
	case "hello":
		return h.hello(), nil
	case "t3-setup.status":
		return h.status(), nil
	case "t3-setup.run":
		return h.run(env.Args)
	case "t3-setup.login.open":
		return h.open()
	case "t3-setup.login.code":
		return h.code(env.Args)
	case "t3-setup.cancel":
		h.mu.Lock()
		h.login = false
		h.mu.Unlock()
		return map[string]any{"stopped": true}, nil
	default:
		return nil, errString("Rubi Home doesn't do " + clip(env.Op, 40))
	}
}

func (h *Home) pair(raw json.RawMessage) (any, error) {
	var in struct {
		Secret string `json:"secret"`
		Token  string `json:"token"`
	}
	if json.Unmarshal(raw, &in) != nil || in.Token == "" {
		return nil, errString("bad arguments")
	}
	if subtle.ConstantTimeCompare([]byte(in.Secret), []byte(h.secret)) != 1 {
		return nil, errString("this pairing code is not the one this Rubi Home is showing")
	}
	h.mu.Lock()
	h.token = in.Token
	h.mu.Unlock()
	return h.hello(), nil
}

func (h *Home) paired(token string) bool {
	if token == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return subtle.ConstantTimeCompare([]byte(token), []byte(h.token)) == 1
}

func (h *Home) hello() map[string]any {
	return map[string]any{
		"name": h.Name, "version": "sandbox",
		"can": []string{"t3-setup"},
		"features": map[string]any{
			"t3": map[string]any{"available": true},
		},
	}
}

func (h *Home) status() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	titles := map[string]string{
		"container": "Apple container", "service": "Container service", "image": "Runner image",
		"t3": "T3 installed and pinned", "pair": "T3 paired with Rubi",
		"login": "Provider sign-in per role", "selftest": "Self-test",
	}
	reasons := map[string]string{
		"container": "Installed (sandbox: the container tool is stubbed, so this step starts done).",
		"service":   "Not started", "image": "Not built yet", "t3": "Not installed",
		"pair": "Not paired", "login": "Not signed in", "selftest": "Not run",
	}
	actions := map[string]string{
		"service": "start", "image": "build", "t3": "install", "pair": "pair",
		"login": "login", "selftest": "selftest",
	}
	var steps []map[string]any
	blocked := false
	var waiting []string
	for _, id := range h.order {
		st := h.state[id]
		reason := reasons[id]
		action := ""
		if st == "done" {
			reason = "Done"
			if id == "container" {
				reason = reasons[id]
			}
			if id == "login" && h.signedIn {
				reason = "Signed in with the sandbox Claude page."
			}
		} else if blocked {
			reason = "Waiting for earlier steps: " + strings.Join(waiting, ", ") + "."
		} else {
			action = actions[id]
			blocked = true
		}
		if st != "done" {
			waiting = append(waiting, titles[id])
		}
		step := map[string]any{"id": id, "title": titles[id], "state": st, "reason": reason, "action": action}
		if id == "login" {
			roleState, roleAction, roleReason := st, action, reason
			if h.login && !h.signedIn {
				roleState, roleAction = "working", ""
				roleReason = "Open the link and sign in."
				step["state"], step["reason"], step["action"] = roleState, roleReason, "cancel"
				step["url"], step["wants_code"] = h.SignInURL, true
			}
			step["projects"] = []map[string]any{{
				"id": "reviewer", "dir": "sandbox · role reviewer",
				"state": roleState, "reason": roleReason, "action": roleAction,
			}}
		}
		steps = append(steps, step)
	}
	out := map[string]any{"steps": steps, "supported": true, "running": h.state["t3"] == "done", "paired": h.state["pair"] == "done"}
	if h.login && !h.signedIn {
		out["login"] = map[string]any{"url": h.SignInURL, "wants_code": true, "role": "reviewer"}
	}
	return out
}

func (h *Home) run(raw json.RawMessage) (any, error) {
	var in struct {
		Step   string `json:"step"`
		Method string `json:"method"`
	}
	if len(raw) > 0 && string(raw) != "null" && json.Unmarshal(raw, &in) != nil {
		return nil, errString("bad arguments")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if in.Step == "all" {
		for _, id := range []string{"service", "image", "t3", "pair"} {
			h.state[id] = "done"
		}
		return map[string]any{"started": true}, nil
	}
	if in.Step == "login" {
		if in.Method != "" && in.Method != "claudeai" {
			return nil, errString(`method must be "claudeai"`)
		}
		if in.Method == "" {
			return map[string]any{"started": true}, nil
		}
		if !h.earlierDone("login") {
			return nil, errString("Waiting for earlier steps.")
		}
		h.login = true
		return map[string]any{"started": true}, nil
	}
	if _, ok := h.state[in.Step]; !ok {
		return nil, errString("unknown setup step")
	}
	if !h.earlierDone(in.Step) {
		return nil, errString("Waiting for earlier steps.")
	}
	h.state[in.Step] = "done"
	return map[string]any{"started": true}, nil
}

func (h *Home) earlierDone(step string) bool {
	for _, id := range h.order {
		if id == step {
			return true
		}
		if id == "login" || id == "selftest" {
			continue
		}
		if h.state[id] != "done" {
			return false
		}
	}
	return false
}

func (h *Home) open() (any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.login || h.SignInURL == "" {
		return nil, errString("no sign-in is waiting")
	}
	return map[string]any{"url": h.SignInURL, "opened": false}, nil
}

func (h *Home) code(raw json.RawMessage) (any, error) {
	var in struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &in) != nil {
		return nil, errString("bad arguments")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.login {
		return nil, errString("no sign-in is waiting for a code")
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(in.Code)), []byte(SignInCode)) != 1 {
		return nil, errString("that code was not accepted")
	}
	h.login = false
	h.signedIn = true
	h.state["login"] = "done"
	return map[string]any{"sent": true}, nil
}

type strErr string

func (e strErr) Error() string { return string(e) }
func errString(s string) error { return strErr(s) }

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
