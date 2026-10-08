// Package approvals gates integration actions behind the user's consent.
//
// An integration describes an action (kind, preview, options, and an Execute function) and submits it.
// Depending on the policy level for that kind:
//
//   - none:   the action runs immediately.
//   - chat:   the agent shows plain-text buttons; the action runs when the agent reports the exact label
//     the user pressed (ConfirmChat). This is a protocol, not proof: it relies on the agent's honesty.
//   - strong: the user approves in the panel with a passkey or password. The panel API verifies the proof
//     and calls DecideStrong. The agent cannot complete these.
//
// Execute always runs on the action exactly as submitted, at most once.
package approvals

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Level string

const (
	None   Level = "none"
	Chat   Level = "chat"
	Strong Level = "strong"
)

func ParseLevel(s string) (Level, bool) {
	switch Level(s) {
	case None, Chat, Strong:
		return Level(s), true
	}
	return "", false
}

type State string

const (
	Pending   State = "pending"
	Executing State = "executing"
	Executed  State = "executed"
	Failed    State = "failed"
	Denied    State = "denied"
	Cancelled State = "cancelled"
	Expired   State = "expired"
)

func (s State) terminal() bool { return s != Pending && s != Executing }

type Option struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Meaning string `json:"meaning,omitempty"`
}

type Executor func(ctx context.Context, option string) (any, error)

type Request struct {
	Integration string
	Kind        string // e.g. "icloud-mail.send"
	Summary     string // one line, e.g. `Send email to anna@example.com: "Meeting"`
	Question    string // optional, answered by the choice of option
	Preview     any    // full details shown to the user, rendered by the panel or the agent
	Options     []Option
	// Items make this a batch: the user approves any subset in one go. Execute then receives
	// "items:<key>,<key>…" with the chosen keys (all of them for an option key or level none).
	Items   []Item
	Execute Executor
}

// Item is one entry of a batch approval.
type Item struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Preview any    `json:"preview,omitempty"`
}

// ItemsPrefix starts the option of a batch approval: "items:a,b".
const ItemsPrefix = "items:"

// normalize checks an option and, for batches, turns it into "items:" plus the chosen keys in item order.
func (r *Request) normalize(option string) (string, error) {
	isOption := false
	for _, o := range r.Options {
		isOption = isOption || o.Key == option
	}
	if len(r.Items) == 0 {
		if !isOption {
			return "", fmt.Errorf("unknown option %q", option)
		}
		return option, nil
	}
	chosen := map[string]bool{}
	if isOption {
		for _, it := range r.Items {
			chosen[it.Key] = true
		}
	} else if rest, ok := strings.CutPrefix(option, ItemsPrefix); ok {
		for _, k := range strings.Split(rest, ",") {
			chosen[k] = true
		}
	} else {
		return "", fmt.Errorf("unknown option %q", option)
	}
	var keys []string
	for _, it := range r.Items {
		if chosen[it.Key] {
			keys = append(keys, it.Key)
			delete(chosen, it.Key)
		}
	}
	if len(chosen) > 0 {
		return "", errors.New("unknown item in the selection")
	}
	if len(keys) == 0 {
		return "", errors.New("choose at least one item")
	}
	return ItemsPrefix + strings.Join(keys, ","), nil
}

// CheckOption validates an option for a pending approval (the panel asks before signing a batch subset).
func (e *Engine) CheckOption(id, option string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, err := e.getLocked(id)
	if err != nil {
		return err
	}
	_, err = a.req.normalize(option)
	return err
}

// Snapshot is the externally visible state of an approval.
type Snapshot struct {
	ID          string    `json:"approval_id"`
	Integration string    `json:"integration"`
	Kind        string    `json:"kind"`
	Level       Level     `json:"level"`
	State       State     `json:"state"`
	Summary     string    `json:"summary"`
	Code        string    `json:"code"`
	Chosen      string    `json:"chosen_option,omitempty"`
	Result      any       `json:"result,omitempty"`
	Error       string    `json:"error,omitempty"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Detail adds what the panel needs to render the approval screen.
type Detail struct {
	Snapshot
	Question string   `json:"question,omitempty"`
	Preview  any      `json:"preview"`
	Options  []Option `json:"options"`
	Items    []Item   `json:"items,omitempty"`
	Nonce    string   `json:"-"`
}

type Config struct {
	TTL        time.Duration
	MinDelay   time.Duration // between submission and a chat confirmation
	MaxPerHour int           // executed gated actions, all kinds
}

func DefaultConfig() Config {
	return Config{TTL: 15 * time.Minute, MinDelay: 3 * time.Second, MaxPerHour: 30}
}

type Hooks struct {
	// Level returns the policy level for an action kind.
	Level func(kind string) Level
	// Link returns the panel URL for a strong approval.
	Link func(approvalID string) (string, error)
	// Label localizes a base English label ("Send", "Edit", "Cancel").
	Label func(string) string
	// Audit records approval lifecycle events (no secrets).
	Audit func(event string, fields map[string]any)
	// OnFinish is called (asynchronously) when a gated approval reaches a terminal state.
	OnFinish func(Snapshot)
}

type approval struct {
	req      Request
	id, code string
	nonce    string // binds approval challenges to this exact approval
	level    Level
	created  time.Time
	expires  time.Time
	state    State
	chosen   string
	result   any
	errText  string
	done     chan struct{}
}

type Engine struct {
	cfg   Config
	hooks Hooks
	now   func() time.Time

	mu       sync.Mutex
	items    map[string]*approval
	executed []time.Time
}

var (
	ErrUnknown   = errors.New("unknown or expired approval_id; prepare the action again and re-ask the user")
	ErrWrongPath = errors.New("this approval is not confirmed this way")
)

func New(cfg Config, hooks Hooks) *Engine {
	if hooks.Label == nil {
		hooks.Label = func(s string) string { return s }
	}
	if hooks.Audit == nil {
		hooks.Audit = func(string, map[string]any) {}
	}
	return &Engine{cfg: cfg, hooks: hooks, now: time.Now, items: map[string]*approval{}}
}

const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O/1/I

func randomID(prefix string, n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return prefix + base64.RawURLEncoding.EncodeToString(b)
}

func randomCode() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	var sb strings.Builder
	for _, c := range b {
		sb.WriteByte(codeAlphabet[int(c)%len(codeAlphabet)])
	}
	return sb.String()
}

// Submit gates req according to policy. For level none it executes immediately.
func (e *Engine) Submit(ctx context.Context, req Request) (map[string]any, error) {
	if len(req.Options) == 0 || req.Execute == nil {
		return nil, errors.New("approval request needs at least one option and an executor")
	}
	level := Strong
	if e.hooks.Level != nil {
		level = e.hooks.Level(req.Kind)
	}
	if level == None {
		opt, err := req.normalize(req.Options[0].Key)
		if err != nil {
			return nil, err
		}
		res, err := req.Execute(ctx, opt)
		e.hooks.Audit("action.executed", map[string]any{"kind": req.Kind, "level": level, "ok": err == nil})
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": "done", "result": res}, nil
	}

	now := e.now()
	a := &approval{req: req, id: randomID("apr_", 12), code: randomCode(), nonce: randomID("", 16), level: level, created: now,
		expires: now.Add(e.cfg.TTL), state: Pending, done: make(chan struct{})}
	e.mu.Lock()
	e.pruneLocked(now)
	e.items[a.id] = a
	e.mu.Unlock()
	e.hooks.Audit("approval.requested", map[string]any{"approval_id": a.id, "kind": req.Kind, "level": level,
		"summary": req.Summary})

	out := map[string]any{
		"status":      "awaiting_approval",
		"approval_id": a.id,
		"level":       level,
		"summary":     req.Summary,
		"expires_at":  a.expires.UTC().Format(time.RFC3339),
	}
	if req.Question != "" {
		out["question"] = req.Question
	}
	switch level {
	case Chat:
		out["preview"] = req.Preview
		out["buttons"] = e.buttons(a)
		out["instructions"] = "Show the preview and question to the user with these exact button labels " +
			"(plain text, no emoji) and wait. Only after the user presses an option button, call rubi_confirm " +
			"with that label verbatim. Edit: ask what to change and prepare again. Cancel: call rubi_cancel."
	case Strong:
		if e.hooks.Link != nil {
			if url, err := e.hooks.Link(a.id); err == nil {
				out["approval_url"] = url
			} else {
				out["approval_url_error"] = err.Error()
			}
		}
		out["instructions"] = "Send the user the approval link with one sentence about what is waiting. " +
			"The user reviews and approves in the Rubi panel; you cannot approve it. Then call rubi_approval " +
			"with wait_seconds to learn the outcome."
	}
	return out, nil
}

func (e *Engine) buttons(a *approval) []map[string]string {
	var bs []map[string]string
	for _, o := range a.req.Options {
		b := map[string]string{"label": e.optionLabel(a, o)}
		if o.Meaning != "" {
			b["meaning"] = o.Meaning
		}
		bs = append(bs, b)
	}
	bs = append(bs, map[string]string{"label": e.hooks.Label("Edit")},
		map[string]string{"label": e.cancelLabel(a)})
	return bs
}

func (e *Engine) optionLabel(a *approval, o Option) string {
	return e.hooks.Label(o.Label) + " · " + a.code
}

func (e *Engine) cancelLabel(a *approval) string { return e.hooks.Label("Cancel") + " · " + a.code }

// ConfirmChat completes a chat-level approval with the label the user pressed.
func (e *Engine) ConfirmChat(ctx context.Context, id, userResponse string) (Snapshot, error) {
	e.mu.Lock()
	a, err := e.getLocked(id)
	if err != nil {
		e.mu.Unlock()
		return Snapshot{}, err
	}
	if a.level != Chat {
		e.mu.Unlock()
		return Snapshot{}, fmt.Errorf("%w: it needs approval in the Rubi panel", ErrWrongPath)
	}
	pressed := strings.TrimSpace(userResponse)
	if pressed == e.cancelLabel(a) {
		e.finishLocked(a, Cancelled)
		snap := e.snapshotLocked(a)
		e.mu.Unlock()
		return snap, nil
	}
	var chosen *Option
	var labels []string
	for i, o := range a.req.Options {
		l := e.optionLabel(a, o)
		labels = append(labels, fmt.Sprintf("%q", l))
		if pressed == l {
			chosen = &a.req.Options[i]
		}
	}
	if chosen == nil {
		e.mu.Unlock()
		return Snapshot{}, fmt.Errorf("user_response must be exactly the label of the button the user pressed "+
			"(one of %s). If the user has not pressed one, do not confirm", strings.Join(labels, ", "))
	}
	if e.now().Sub(a.created) < e.cfg.MinDelay {
		e.mu.Unlock()
		return Snapshot{}, errors.New("too soon after the action was prepared; wait for the user's button press")
	}
	option, err := a.req.normalize(chosen.Key) // a chat button approves every item of a batch
	if err != nil {
		e.mu.Unlock()
		return Snapshot{}, err
	}
	if err := e.claimLocked(a, option); err != nil {
		e.mu.Unlock()
		return Snapshot{}, err
	}
	e.mu.Unlock()
	return e.run(ctx, a), nil
}

// DecideStrong records the user's panel decision. The caller must already have verified the user's
// passkey or password proof for this approval id and option.
func (e *Engine) DecideStrong(ctx context.Context, id, option string, approve bool) (Snapshot, error) {
	e.mu.Lock()
	a, err := e.getLocked(id)
	if err != nil {
		e.mu.Unlock()
		return Snapshot{}, err
	}
	if a.level != Strong {
		e.mu.Unlock()
		return Snapshot{}, ErrWrongPath
	}
	if !approve {
		e.finishLocked(a, Denied)
		snap := e.snapshotLocked(a)
		e.mu.Unlock()
		e.hooks.Audit("approval.denied", map[string]any{"approval_id": id})
		return snap, nil
	}
	option, err = a.req.normalize(option)
	if err != nil {
		e.mu.Unlock()
		return Snapshot{}, err
	}
	if err := e.claimLocked(a, option); err != nil {
		e.mu.Unlock()
		return Snapshot{}, err
	}
	e.mu.Unlock()
	return e.run(ctx, a), nil
}

// claimLocked moves a pending approval to executing, enforcing the hourly limit.
func (e *Engine) claimLocked(a *approval, option string) error {
	if a.state != Pending {
		return fmt.Errorf("this action is already %s", a.state)
	}
	now := e.now()
	cutoff := now.Add(-time.Hour)
	kept := e.executed[:0]
	for _, t := range e.executed {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	e.executed = kept
	if e.cfg.MaxPerHour > 0 && len(e.executed) >= e.cfg.MaxPerHour {
		return errors.New("hourly limit for approved actions reached")
	}
	e.executed = append(e.executed, now)
	a.state, a.chosen = Executing, option
	return nil
}

func (e *Engine) run(ctx context.Context, a *approval) Snapshot {
	res, err := a.req.Execute(ctx, a.chosen)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		a.errText = err.Error()
		e.finishLocked(a, Failed)
	} else {
		a.result = res
		e.finishLocked(a, Executed)
	}
	if e.hooks.OnFinish != nil {
		go e.hooks.OnFinish(e.snapshotLocked(a))
	}
	e.hooks.Audit("approval."+string(a.state), map[string]any{"approval_id": a.id, "kind": a.req.Kind,
		"option": a.chosen})
	return e.snapshotLocked(a)
}

func (e *Engine) Cancel(id string) (Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, err := e.getLocked(id)
	if err != nil {
		return Snapshot{}, err
	}
	if a.state == Pending {
		e.finishLocked(a, Cancelled)
	}
	return e.snapshotLocked(a), nil
}

func (e *Engine) Get(id string) (Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, err := e.getLocked(id)
	if err != nil {
		return Snapshot{}, err
	}
	return e.snapshotLocked(a), nil
}

// Detail returns what the panel renders for a strong approval.
func (e *Engine) Detail(id string) (Detail, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	a, err := e.getLocked(id)
	if err != nil {
		return Detail{}, err
	}
	return Detail{Snapshot: e.snapshotLocked(a), Question: a.req.Question, Preview: a.req.Preview,
		Options: a.req.Options, Items: a.req.Items, Nonce: a.nonce}, nil
}

// Wait blocks until the approval reaches a terminal state, the timeout elapses, or ctx ends.
func (e *Engine) Wait(ctx context.Context, id string, timeout time.Duration) (Snapshot, error) {
	e.mu.Lock()
	a, err := e.getLocked(id)
	e.mu.Unlock()
	if err != nil {
		return Snapshot{}, err
	}
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-a.done:
	case <-t.C:
	case <-ctx.Done():
	}
	return e.Get(id)
}

// CancelAll cancels every pending approval (used when Rubi locks).
func (e *Engine) CancelAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, a := range e.items {
		if a.state == Pending {
			e.finishLocked(a, Cancelled)
		}
	}
}

func (e *Engine) getLocked(id string) (*approval, error) {
	a, ok := e.items[id]
	if !ok {
		return nil, ErrUnknown
	}
	if a.state == Pending && e.now().After(a.expires) {
		e.finishLocked(a, Expired)
	}
	return a, nil
}

func (e *Engine) finishLocked(a *approval, s State) {
	if a.state.terminal() {
		return
	}
	a.state = s
	close(a.done)
	if e.hooks.OnFinish != nil && s != Executed && s != Failed {
		go e.hooks.OnFinish(e.snapshotLocked(a))
	}
}

func (e *Engine) snapshotLocked(a *approval) Snapshot {
	return Snapshot{ID: a.id, Integration: a.req.Integration, Kind: a.req.Kind, Level: a.level, State: a.state,
		Summary: a.req.Summary, Code: a.code, Chosen: a.chosen, Result: a.result, Error: a.errText,
		ExpiresAt: a.expires.UTC()}
}

// pruneLocked forgets approvals that ended more than an hour ago.
func (e *Engine) pruneLocked(now time.Time) {
	for id, a := range e.items {
		if now.After(a.expires.Add(time.Hour)) {
			delete(e.items, id)
		}
	}
}
