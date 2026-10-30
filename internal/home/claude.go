package home

// Claude Code approvals (capability "claude"). A PermissionRequest hook of Claude Code on this computer
// (`rubi-home claude-hook`) hands each request to the helper over a local socket and waits; the socket can
// only ask, never answer. The paired Rubi takes requests with claude.next and answers with claude.decide,
// over the encrypted channel like every other operation. An answer must name the request's id, session
// and SHA-256 of its tool input, and counts once. Anything else (no answer in time, Rubi locked or
// offline, the helper not running) is a denial.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

const (
	// ClaudeSocket is the local socket's name in the helper's directory.
	ClaudeSocket = "claude.sock"
	// ClaudeDefaultWait is how long a hook waits for Rubi's answer; it must stay below the hook's own
	// timeout in Claude Code's settings (300 seconds in the example).
	ClaudeDefaultWait = 280 * time.Second
	claudeMaxPoll     = 25 * time.Second
	// claudeGrace: Rubi counts as connected while a claude.next is open and for this long after one ends.
	claudeGrace       = 15 * time.Second
	claudeMaxPending  = 16
	claudeMaxConns    = 32 // PermissionRequest connections at once
	claudeMaxReading  = 64 // connections whose first line is still being read
	claudeMaxEvents   = 16 // event hooks being handled at once
	claudeMaxInput    = 1 << 20
	claudeMaxTool     = 64 << 10
	claudeMaxMessage  = 500
	claudeReadTimeout = 10 * time.Second
	// claudeFirstLineWait is how long a connection may take to send its request line. A hook writes it at
	// once; a client that stalls holds one of the claudeMaxReading slots, so the wait is short.
	claudeFirstLineWait = 3 * time.Second
)

// ClaudeRequest is a permission request as Rubi sees it. ToolInput is the tool input exactly as hashed
// (compact JSON); Command and Description are read from it.
type ClaudeRequest struct {
	ID             string    `json:"id"`
	SessionID      string    `json:"session_id"`
	ToolName       string    `json:"tool_name"`
	ToolInput      string    `json:"tool_input"`
	Digest         string    `json:"digest"` // hex SHA-256 of ToolInput
	Command        string    `json:"command"`
	Description    string    `json:"description,omitempty"`
	Cwd            string    `json:"cwd,omitempty"`
	PermissionMode string    `json:"permission_mode,omitempty"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// ClaudeDecision is the answer a hook prints: behavior "allow" or "deny", and for a denial the reason
// Claude is told.
type ClaudeDecision struct {
	Behavior string `json:"behavior"`
	Message  string `json:"message,omitempty"`
}

func claudeDeny(msg string) ClaudeDecision {
	return ClaudeDecision{Behavior: "deny", Message: "Rubi: " + msg}
}

// Messages of the denials the helper makes itself.
const (
	ClaudeNotConnected = "Rubi isn't connected to this computer (it may be locked or offline), so the command was denied."
	ClaudeStopped      = "Rubi stopped answering (it may have been locked or gone offline), so the command was denied."
	ClaudeNoAnswer     = "no answer in time, so the command was denied."
	ClaudeUnreachable  = "Rubi Home isn't running on this computer, so the command was denied."
)

// Claude queues Claude Code permission requests for Rubi. The zero value is ready to use.
type Claude struct {
	// Wait is how long a request waits for Rubi (ClaudeDefaultWait when zero).
	Wait time.Duration
	// DesktopStore is the Claude desktop app's session store and Transcripts Claude Code's projects
	// directory (~/.claude/projects); both are only read, and "" leaves one out (see sessions.go).
	DesktopStore string
	Transcripts  string
	// Procs answers the kernel questions behind tool identity (the operating system's when nil; a seam for tests).
	Procs procTree
	// QueueFile is where inbox, outbox and owed replies are kept between runs (see queue.go); "" keeps
	// them in memory only.
	QueueFile string

	mu       sync.Mutex
	pending  map[string]*claudeItem
	order    []string // ids not yet handed to Rubi, oldest first
	polls    int      // claude.next calls in progress
	lastPoll time.Time
	signal   chan struct{} // closed when a request or an outgoing message is queued

	hooks       hookState // the last hook binary seen (see hookver.go)
	reg         sessionRegistry
	answerOnce  sync.Once
	answerSlots chan struct{} // slots for transcript reads (see lastAnswer)
	flightMu    sync.Mutex
	flights     map[string]*flight         // claude-sessions.list calls still running, by key (see within)
	inbox       map[string][]ClaudeMessage // by session id
	turns       map[string]*turnState
	bound       map[string][]boundSister  // Bots bound to each session, as the plugin said (see sistertools.go)
	delivered   map[string][]deliveredMsg // messages that reached each session, for rubi_reply
	toolHits    map[string][]time.Time    // recent asks and replies of tool calls, by kind and session
	toolGlobal  []time.Time               // recent outbox pushes of all tool calls
	peers       peerMap                   // hook process ancestors -> sessions (see peer.go)
	peerOnce    atomic.Bool               // an ignored hook claim is logged once
	diagOnce    atomic.Bool               // a tool line naming another session than the attested one is logged once
	outbox      []ClaudeOut
	inflight    map[string][]ClaudeMessage // messages taken out for a wake that isn't acknowledged yet
	saveKick    chan struct{}              // tells the saver the queue changed (see queue.go)
	saveMu      sync.Mutex                 // one save at a time
	writeQueue  func(path string, b []byte) error

	wmu     sync.Mutex // guards waiters only; never held while doing I/O or taking mu
	waiters map[string]*claudeWaiter
}

type claudeItem struct {
	req       ClaudeRequest
	delivered bool
	answer    chan ClaudeDecision // buffered; gets exactly one decision
}

func (c *Claude) wait() time.Duration {
	if c.Wait > 0 {
		return c.Wait
	}
	return ClaudeDefaultWait
}

func (c *Claude) connectedLocked(now time.Time) bool {
	return c.polls > 0 || (!c.lastPoll.IsZero() && now.Sub(c.lastPoll) <= claudeGrace)
}

func (c *Claude) connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectedLocked(time.Now())
}

// hookInput is what Claude Code sends a PermissionRequest hook on stdin (the fields used here).
type hookInput struct {
	SessionID      string          `json:"session_id"`
	Cwd            string          `json:"cwd"`
	PermissionMode string          `json:"permission_mode"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
}

// parseHookInput turns a hook's input into a request (without id and expiry).
func parseHookInput(raw []byte) (ClaudeRequest, error) {
	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return ClaudeRequest{}, errors.New("the hook input isn't valid JSON")
	}
	if in.HookEventName != "PermissionRequest" {
		return ClaudeRequest{}, fmt.Errorf("unexpected hook event %q", in.HookEventName)
	}
	if in.ToolName != "Bash" {
		return ClaudeRequest{}, fmt.Errorf("only Bash commands can be approved through Rubi, not %q", in.ToolName)
	}
	if in.SessionID == "" || len(in.SessionID) > 200 {
		return ClaudeRequest{}, errors.New("the request has no valid session id")
	}
	var tool bytes.Buffer
	if err := json.Compact(&tool, in.ToolInput); err != nil || tool.Len() == 0 || tool.Bytes()[0] != '{' {
		return ClaudeRequest{}, errors.New("the tool input isn't a JSON object")
	}
	if tool.Len() > claudeMaxTool {
		return ClaudeRequest{}, errors.New("the command is too long to review")
	}
	var bash struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(tool.Bytes(), &bash); err != nil || strings.TrimSpace(bash.Command) == "" {
		return ClaudeRequest{}, errors.New("the request has no command")
	}
	sum := sha256.Sum256(tool.Bytes())
	return ClaudeRequest{SessionID: in.SessionID, ToolName: in.ToolName, ToolInput: tool.String(),
		Digest: hex.EncodeToString(sum[:]), Command: bash.Command, Description: bash.Description,
		Cwd: in.Cwd, PermissionMode: in.PermissionMode}, nil
}

// Ask queues one hook request and waits for Rubi's decision. It always returns a decision: anything
// but an answer from Rubi is a denial. ctx ends when the hook goes away.
func (c *Claude) Ask(ctx context.Context, raw []byte) ClaudeDecision {
	req, err := parseHookInput(raw)
	if err != nil {
		return claudeDeny(err.Error() + "; denied.")
	}
	now := time.Now()
	c.mu.Lock()
	if !c.connectedLocked(now) {
		c.mu.Unlock()
		return claudeDeny(ClaudeNotConnected)
	}
	if len(c.pending) >= claudeMaxPending {
		c.mu.Unlock()
		return claudeDeny("too many commands are already waiting for approval; denied.")
	}
	req.ID = "cc_" + rand.Text()
	req.ExpiresAt = now.Add(c.wait()).UTC()
	it := &claudeItem{req: req, answer: make(chan ClaudeDecision, 1)}
	if c.pending == nil {
		c.pending = map[string]*claudeItem{}
	}
	c.pending[req.ID] = it
	c.order = append(c.order, req.ID)
	if c.signal != nil {
		close(c.signal)
		c.signal = nil
	}
	c.mu.Unlock()
	defer c.drop(req.ID)

	deadline := time.NewTimer(c.wait())
	defer deadline.Stop()
	alive := time.NewTicker(time.Second)
	defer alive.Stop()
	for {
		select {
		case d := <-it.answer:
			return d
		case <-ctx.Done():
			return claudeDeny("the request was cancelled.")
		case <-deadline.C:
			return claudeDeny(ClaudeNoAnswer)
		case <-alive.C:
			if !c.connected() {
				return claudeDeny(ClaudeStopped)
			}
		}
	}
}

// drop forgets a request, so a late answer finds nothing.
func (c *Claude) drop(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, id)
	for i, x := range c.order {
		if x == id {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

// Next hands Rubi the oldest request not yet handed out, waiting up to wait (at most 25 seconds) for
// one. It returns nil when none came. While Rubi polls it, the helper counts Rubi as present.
func (c *Claude) Next(ctx context.Context, wait time.Duration) (*ClaudeRequest, error) {
	c.mu.Lock()
	c.polls++
	c.lastPoll = time.Now()
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.polls--
		c.lastPoll = time.Now()
		c.mu.Unlock()
	}()
	r, _, err := c.next(ctx, wait, true, false)
	return r, err
}

// NextOut hands out what sessions say (replies and asks), one item per call, waiting like Next. It is
// the claude-sessions capability's poll and doesn't count as Rubi being there for approvals.
func (c *Claude) NextOut(ctx context.Context, wait time.Duration) (*ClaudeOut, error) {
	_, o, err := c.next(ctx, wait, false, true)
	return o, err
}

// next waits for a request (req) or an outgoing message (out).
func (c *Claude) next(ctx context.Context, wait time.Duration, req, out bool) (*ClaudeRequest, *ClaudeOut, error) {
	if wait <= 0 || wait > claudeMaxPoll {
		wait = claudeMaxPoll
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		c.mu.Lock()
		for req && len(c.order) > 0 {
			id := c.order[0]
			c.order = c.order[1:]
			if it := c.pending[id]; it != nil {
				it.delivered = true
				r := it.req
				c.mu.Unlock()
				return &r, nil, nil
			}
		}
		if out && len(c.outbox) > 0 {
			o := c.outbox[0]
			c.outbox = c.outbox[1:]
			c.dirtyLocked()
			c.mu.Unlock()
			return nil, &o, nil
		}
		if c.signal == nil {
			c.signal = make(chan struct{})
		}
		sig := c.signal
		c.mu.Unlock()
		select {
		case <-sig:
		case <-t.C:
			return nil, nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

// ClaudeAnswer is Rubi's decision on one request (claude.decide).
type ClaudeAnswer struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Digest    string `json:"digest"`
	Behavior  string `json:"behavior"`
	Message   string `json:"message,omitempty"`
}

// Decide answers a request Rubi was handed. It must match the request's id, session and digest, and
// works once; an answer that names the request but doesn't match denies it.
func (c *Claude) Decide(a ClaudeAnswer) error {
	if a.Behavior != "allow" && a.Behavior != "deny" {
		return errors.New(`behavior must be "allow" or "deny"`)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	it := c.pending[a.ID]
	if it == nil || !it.delivered {
		return errors.New("unknown or expired request")
	}
	delete(c.pending, a.ID)
	if time.Now().After(it.req.ExpiresAt) {
		it.answer <- claudeDeny(ClaudeNoAnswer)
		return errors.New("the request has expired")
	}
	if subtle.ConstantTimeCompare([]byte(a.SessionID), []byte(it.req.SessionID)) != 1 ||
		subtle.ConstantTimeCompare([]byte(strings.ToLower(a.Digest)), []byte(it.req.Digest)) != 1 {
		it.answer <- claudeDeny("the answer didn't match this command, so it was denied.")
		return errors.New("the answer doesn't match the request")
	}
	d := ClaudeDecision{Behavior: a.Behavior}
	if a.Behavior == "deny" {
		msg := cleanMessage(a.Message)
		if msg == "" {
			msg = "denied."
		}
		d = claudeDeny(msg)
	}
	it.answer <- d
	return nil
}

func cleanMessage(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if r := []rune(strings.TrimSpace(s)); len(r) > claudeMaxMessage {
		return string(r[:claudeMaxMessage])
	}
	return strings.TrimSpace(s)
}

// handle serves a paired Rubi: claude.next and claude.decide (the "claude" capability, approvals) and
// claude-sessions.list, .send and .next (the "claude-sessions" capability: sessions and messages).
func (c *Claude) handle(ctx context.Context, op string, args json.RawMessage) (any, error) {
	switch op {
	case "claude.next", "claude-sessions.next":
		var in struct {
			WaitSeconds int `json:"wait_seconds"`
		}
		if len(args) > 0 && json.Unmarshal(args, &in) != nil {
			return nil, errors.New("bad arguments")
		}
		wait := time.Duration(in.WaitSeconds) * time.Second
		if op == "claude-sessions.next" {
			o, err := c.NextOut(ctx, wait)
			if err != nil {
				return nil, err
			}
			return map[string]any{"out": o}, nil
		}
		r, err := c.Next(ctx, wait)
		if err != nil {
			return nil, err
		}
		return map[string]any{"request": r}, nil
	case "claude-sessions.list":
		var in struct {
			Repo            string `json:"repo"`
			IncludeArchived bool   `json:"include_archived"`
		}
		if len(args) > 0 && json.Unmarshal(args, &in) != nil {
			return nil, errors.New("bad arguments")
		}
		// One flight per archived flag; the repo filter is applied to the shared result, so no number of
		// different filters can use up the flights.
		key := fmt.Sprintf("sessions.list|%v", in.IncludeArchived)
		v, err := c.within(ctx, key, sessionsListTimeout, func() (any, error) {
			all, meta := c.allSessions(in.IncludeArchived)
			return sessionsListed{all, meta}, nil
		})
		if err != nil {
			return nil, err
		}
		l := v.(sessionsListed)
		sources := map[string]any{"desktop_app": dirExists(c.DesktopStore), "transcripts": dirExists(c.Transcripts)}
		if !l.meta.at.IsZero() {
			sources["scanned_at"] = l.meta.at
		}
		if l.meta.stale { // an older scan (a refresh runs), or the hook-reported sessions only
			sources["stale"] = true
		}
		res := map[string]any{"sessions": filterSessions(l.all, in.Repo), "sources": sources}
		if hk := c.hookInfo(); hk != nil {
			res["hook"] = hk
		}
		return res, nil
	case "claude-sessions.send":
		var in struct {
			SessionID string `json:"session_id"`
			From      string `json:"from"`
			Text      string `json:"text"`
		}
		if json.Unmarshal(args, &in) != nil {
			return nil, errors.New("bad arguments")
		}
		// State and waiter as the message found them: Send may wake the session at once, which ends the
		// waiter and marks the session busy before this answer is built.
		state, waiter := c.reg.stateOf(in.SessionID), c.hasWaiter(in.SessionID)
		if state == "" {
			state = "unknown"
		}
		m, waiting, err := c.Send(in.SessionID, in.From, in.Text)
		if err != nil {
			return nil, err
		}
		return map[string]any{"queued": true, "id": m.ID, "waiting": waiting, "session_state": state,
			"waiter": waiter}, nil
	case "claude-sessions.bound":
		var in struct {
			SessionID string `json:"session_id"`
			Sister    string `json:"sister"`
			Bound     bool   `json:"bound"`
		}
		if json.Unmarshal(args, &in) != nil {
			return nil, errors.New("bad arguments")
		}
		if err := c.setBound(in.SessionID, in.Sister, in.Bound); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	case "claude.decide":
		var a ClaudeAnswer
		if json.Unmarshal(args, &a) != nil {
			return nil, errors.New("bad arguments")
		}
		if err := c.Decide(a); err != nil {
			return nil, err
		}
		return map[string]any{"ok": true}, nil
	}
	return nil, fmt.Errorf("Rubi Home doesn't do %q", op)
}

// claudeEventDeadline is how long the socket waits for an event hook's handler; it stays below
// claudeEventWait, how long the hook itself waits. claudeCommitWait is how much longer it waits for a
// handler that has taken its messages just at the deadline (its reply only needs building).
var (
	claudeEventDeadline = 5 * time.Second
	claudeCommitWait    = time.Second
)

// claudeTooManyAtOnce is the denial when more connections arrive at once than can be read.
const claudeTooManyAtOnce = "too many hook requests are arriving at once, so the command was denied."

// sessionsListTimeout bounds claude-sessions.list, which reads the session stores and git
// files: well below the time a caller (Rubi, then its MCP client) waits, so a hanging disk gives an error.
var sessionsListTimeout = 20 * time.Second

var errListTooLong = errors.New("reading the Claude Code sessions on this computer takes too long (a project's disk may not be responding); try again later")

// sessionsListed is what one claude-sessions.list flight found.
type sessionsListed struct {
	all  []ClaudeSession
	meta scanMeta
}

// flight is one call of fn that callers of within share.
type flight struct {
	done chan struct{}
	v    any
	err  error
}

// within runs fn but returns by d (or ctx's end) with an error if fn hasn't; fn keeps running. It is
// single-flight per key: while a call for the key still runs, further callers wait for that call's result
// (each for at most its own d) instead of starting another, so stuck calls don't pile up goroutines. The
// keys are fixed (one per include_archived value), so at most two calls are ever running.
func (c *Claude) within(ctx context.Context, key string, d time.Duration, fn func() (any, error)) (any, error) {
	c.flightMu.Lock()
	f := c.flights[key]
	if f == nil {
		if c.flights == nil {
			c.flights = map[string]*flight{}
		}
		f = &flight{done: make(chan struct{})}
		c.flights[key] = f
		go func() {
			defer func() { // also when fn panics: the flight must end and its waiters be released
				if p := recover(); p != nil {
					log.Printf("claude: %s panicked: %v", key, p)
					f.v, f.err = nil, errors.New("reading the Claude Code sessions failed")
				}
				c.flightMu.Lock()
				delete(c.flights, key)
				c.flightMu.Unlock()
				close(f.done)
			}()
			f.v, f.err = fn()
		}()
	}
	c.flightMu.Unlock()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-f.done:
		return f.v, f.err
	case <-t.C:
	case <-ctx.Done():
	}
	return nil, errListTooLong
}

// ---- the local socket ----

// Serve listens on the local socket at path until ctx ends. A connection sends one line (the hook's
// input as compact JSON) and gets one line back (a ClaudeDecision). Nothing else is understood: there
// is no way to answer a request through the socket.
func (c *Claude) Serve(ctx context.Context, path string) error {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("%s exists and isn't a socket", path)
		}
		_ = os.Remove(path) // left over from an earlier run
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return err
	}
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	defer os.Remove(path)
	// Three limits. reading covers the time before a request is classified (one line, with a read
	// deadline). Then a PermissionRequest takes a slot of perms (a denial when none is free), and an event
	// takes one of events (an empty reply when none is free, never a denial: events can't block anything).
	s := &claudeSlots{reading: make(chan struct{}, claudeMaxReading), perms: make(chan struct{}, claudeMaxConns),
		events: make(chan struct{}, claudeMaxEvents), waits: make(chan struct{}, claudeMaxWaiters),
		tools: make(chan struct{}, claudeMaxToolCalls)}
	if c.QueueFile != "" {
		// The saver outlives the connections: it is stopped after they are done, so its final save has
		// their last commits and restores. Serve returning for any reason stops it.
		sctx, stopSaver := context.WithCancel(context.Background())
		saved := make(chan struct{})
		go func() {
			defer close(saved)
			c.runSaver(sctx)
		}()
		defer func() { // a hung disk doesn't keep the helper from stopping
			stopSaver()
			select {
			case <-saved:
			case <-time.After(3 * time.Second):
				log.Printf("claude: the final save of the queue takes too long; giving up on it")
			}
		}()
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	stopped := ctx                            // Serve was asked to stop (the listener is closed by an AfterFunc, which can run before the derived ctx below is cancelled)
	ctx, stopConns := context.WithCancel(ctx) // also ends the waiters when Accept fails on its own
	defer stopConns()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if stopped.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case s.reading <- struct{}{}:
		default:
			// Not classified yet, so it may be an event: a denial with its reason, which an approval shows
			// and an event hook reads as an empty reply (it has none of a HookReply's fields).
			_ = conn.SetWriteDeadline(time.Now().Add(claudeReadTimeout))
			_ = writeDecision(conn, claudeDeny(claudeTooManyAtOnce))
			conn.Close()
			continue
		}
		peer := c.peerOf(conn) // the kernel's view, taken before the line is read
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.serveConn(ctx, conn, s, peer)
		}()
	}
}

type claudeSlots struct{ reading, perms, events, waits, tools chan struct{} }

func (c *Claude) serveConn(ctx context.Context, conn net.Conn, s *claudeSlots, peer peerID) {
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(claudeFirstLineWait))
	line, err := readLine(conn, claudeMaxInput+claudeStampRoom)
	<-s.reading
	if err != nil {
		_ = writeDecision(conn, claudeDeny("couldn't read the request; denied."))
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	switch hookEvent(line) { // the kernel says which processes host this session's hooks (see peer.go)
	case "SessionStart", "UserPromptSubmit", "Stop", "PermissionRequest", claudeWaitEvent:
		if sid := lineSession(line); sid != "" {
			c.attestHook(peer, sid)
		}
	case "SessionEnd":
		if sid := lineSession(line); sid != "" {
			c.releaseHook(peer, sid)
		}
	}
	if refuse := c.noteHook(line); refuse != "" { // a hook binary of a protocol this helper doesn't speak
		_ = conn.SetWriteDeadline(time.Now().Add(claudeReadTimeout))
		if ev := hookEvent(line); ev != "PermissionRequest" && ev != "" {
			_ = writeJSONLine(conn, struct{}{}) // events and the waiter: as for an unknown event
		} else {
			_ = writeDecision(conn, claudeDeny(refuse))
		}
		return
	}
	if hookEvent(line) == claudeToolEvent { // a sister tool of Claude (claude-mcp): its own slots, never an event or an approval
		c.serveTool(conn, line, s.tools, peer)
		return
	}
	if hookEvent(line) == claudeWaitEvent { // an idle session's waiter: its own slots, never an event or an approval
		c.serveWaiter(ctx, conn, line, s.waits)
		return
	}
	if ev := hookEvent(line); ev != "" && ev != "PermissionRequest" {
		_ = conn.SetWriteDeadline(time.Now().Add(claudeReadTimeout))
		b, _ := json.Marshal(c.event(line, s.events))
		_, _ = conn.Write(append(b, '\n'))
		return
	}
	select {
	case s.perms <- struct{}{}:
		defer func() { <-s.perms }()
	default:
		_ = writeDecision(conn, claudeDeny("too many commands are already waiting for approval; denied."))
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { // the hook going away (Claude Code gave up) cancels the request
		var b [1]byte
		_, _ = conn.Read(b[:])
		cancel()
	}()
	_ = writeDecision(conn, c.Ask(ctx, line))
}

// event handles an event hook within claudeEventDeadline. The slot stays taken until Hook returns, so
// handlers that hang are capped by the slots; with none free, or when Hook is late, the reply is empty
// (Claude Code goes on as without a hook). A late handler is told its answer isn't wanted, so it leaves
// the inbox alone (see eventTicket).
func (c *Claude) event(line []byte, slots chan struct{}) HookReply {
	select {
	case slots <- struct{}{}:
	default:
		return HookReply{}
	}
	tk := &eventTicket{}
	done := make(chan HookReply, 1)
	go func() {
		defer func() { <-slots }()
		defer func() { // a broken hook mustn't take the helper down with it
			if p := recover(); p != nil {
				log.Printf("claude: an event hook failed: %v", p)
				done <- HookReply{}
			}
		}()
		done <- c.hook(line, tk)
	}()
	t := time.NewTimer(claudeEventDeadline)
	defer t.Stop()
	select {
	case r := <-done:
		return r
	case <-t.C:
	}
	if !tk.abandon() { // it took the messages just now: its reply is on the way
		t.Reset(claudeCommitWait)
		select {
		case r := <-done:
			return r
		case <-t.C:
			log.Printf("claude: an event hook took its messages but didn't answer in time; answered without a reply")
			return HookReply{}
		}
	}
	log.Printf("claude: an event hook took too long; answered without a reply")
	return HookReply{}
}

func readLine(r io.Reader, limit int) ([]byte, error) {
	br := bufio.NewReaderSize(io.LimitReader(r, int64(limit)+1), 64<<10)
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > limit {
			return nil, errors.New("request too large")
		}
		if err == nil {
			return bytes.TrimSpace(buf), nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return nil, err
		}
	}
}

func writeDecision(w io.Writer, d ClaudeDecision) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

// ---- the hook (rubi-home claude-hook) ----

// HookOutput is what a PermissionRequest hook prints for Claude Code.
func HookOutput(d ClaudeDecision) []byte {
	dec := map[string]any{"behavior": "allow"}
	if d.Behavior != "allow" { // anything else denies
		dec = map[string]any{"behavior": "deny", "message": d.Message}
	}
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PermissionRequest", "decision": dec}})
	return append(b, '\n')
}

// RunClaudeHook is `rubi-home claude-hook`: it reads Claude Code's PermissionRequest input from in, asks
// the helper listening at sock, and writes the decision to out. It always writes a decision; when
// anything fails, that is a denial. limit bounds the whole wait.
//
// For SessionStart, UserPromptSubmit, Stop and SessionEnd it reports to the helper and prints what the
// helper answers (messages for the session); when anything fails it prints nothing, so Claude Code goes
// on as if there were no hook.
func RunClaudeHook(in io.Reader, out io.Writer, sock string, limit time.Duration) error {
	raw, err := io.ReadAll(io.LimitReader(in, claudeMaxInput+1))
	if err != nil || len(raw) > claudeMaxInput {
		_, err := out.Write(HookOutput(claudeDeny("couldn't read the request; denied.")))
		return err
	}
	if ev := hookEvent(raw); ev != "" && ev != "PermissionRequest" {
		b := EventHookOutput(ev, askHelperEvent(raw, sock))
		if len(b) == 0 {
			return nil
		}
		_, err := out.Write(b)
		return err
	}
	d := askHelper(bytes.NewReader(raw), sock, limit)
	_, err = out.Write(HookOutput(d))
	return err
}

func askHelper(in io.Reader, sock string, limit time.Duration) ClaudeDecision {
	raw, err := io.ReadAll(io.LimitReader(in, claudeMaxInput+1))
	if err != nil || len(raw) > claudeMaxInput {
		return claudeDeny("couldn't read the request; denied.")
	}
	var line bytes.Buffer
	if err := json.Compact(&line, raw); err != nil {
		return claudeDeny("the hook input isn't valid JSON; denied.")
	}
	conn, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		return claudeDeny(ClaudeUnreachable)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(limit))
	if _, err := conn.Write(append(stampHookLine(line.Bytes()), '\n')); err != nil {
		return claudeDeny(ClaudeUnreachable)
	}
	resp, err := readLine(conn, 64<<10)
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return claudeDeny(ClaudeNoAnswer)
		}
		return claudeDeny("Rubi Home closed the connection, so the command was denied.")
	}
	var d ClaudeDecision
	if json.Unmarshal(resp, &d) != nil || (d.Behavior != "allow" && d.Behavior != "deny") {
		return claudeDeny("Rubi Home gave an answer that couldn't be read, so the command was denied.")
	}
	return d
}

func askHelperEvent(raw []byte, sock string) HookReply {
	var line bytes.Buffer
	if json.Compact(&line, raw) != nil {
		return HookReply{}
	}
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return HookReply{}
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(claudeEventWait))
	if _, err := conn.Write(append(stampHookLine(line.Bytes()), '\n')); err != nil {
		return HookReply{}
	}
	resp, err := readLine(conn, claudeMaxInput)
	if err != nil {
		return HookReply{}
	}
	var r HookReply
	_ = json.Unmarshal(resp, &r)
	return r
}

// EventHookOutput is what a hook other than PermissionRequest prints (nothing when there is nothing to
// say).
func EventHookOutput(event string, r HookReply) []byte {
	var v any
	switch {
	case event == "Stop" && r.Block && r.Reason != "":
		v = map[string]any{"decision": "block", "reason": r.Reason}
	case (event == "UserPromptSubmit" || event == "SessionStart") && r.Context != "":
		v = map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "additionalContext": r.Context}}
	default:
		return nil
	}
	b, _ := json.Marshal(v)
	return append(b, '\n')
}

func dirExists(p string) bool {
	if p == "" {
		return false
	}
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// DefaultClaudeDirs are the desktop app's session store and Claude Code's projects directory for the
// current user.
func DefaultClaudeDirs() (desktop, transcripts string) {
	if cfg, err := os.UserConfigDir(); err == nil {
		desktop = filepath.Join(cfg, "Claude", "claude-code-sessions")
	}
	if home, err := os.UserHomeDir(); err == nil {
		transcripts = filepath.Join(home, ".claude", "projects")
	}
	return desktop, transcripts
}

// ClaudeSocketPath is the socket's path in dir.
func ClaudeSocketPath(dir string) string { return filepath.Join(dir, ClaudeSocket) }
