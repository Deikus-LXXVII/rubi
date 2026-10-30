package home

// The sister tools of Claude Code (rubi_sisters, rubi_ask, rubi_reply, rubi_inbox_status). The MCP server
// `rubi-home claude-mcp` (claudemcp.go) sends each call to the helper as one line on the hook socket with the
// event name "RubiTool". The helper routes it before the waiter and the event path, so a tool call is never an
// approval and never an event: it can only queue an ask or a reply for the sister bound to the caller's own
// session, or read that session's inbox state. It adds no trust.
//
// The line (JSON, stamped like every hook line) and the answer:
//
//	tool -> helper   {"hook_event_name":"RubiTool","tool":"rubi_ask","claude_session_id":"...","tool_cwd":"...", ...}
//	helper -> tool   {"ok":true, ...} or {"error":"..."}     (an older helper answers {})
//
// Which session is calling: the kernel's answer (peer.go). Right after Accept the helper reads the socket peer's pid,
// finds the peer's `claude` ancestor (the session's own process) and looks up the session that claimed it.
// The session id and directory in the line are never used to attribute a call (the id is only compared for
// a diagnostic); a process that isn't inside a session's process tree gets an error.

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"time"
)

const (
	claudeToolEvent          = "RubiTool"
	claudeMaxToolCalls       = 16 // tool calls being handled at once
	claudeToolsPerMin        = 3  // asks, and replies, per session per minute (claudeMaxAsks, as in one answer)
	claudeToolsGlobal        = 20 // tool-made asks and replies per minute, all sessions together
	claudeMaxToolKeys        = 500
	claudeMaxBound           = 500
	claudeMaxBoundPerSession = 4
	claudeMaxDelivered       = 100 // delivered messages remembered per session
	claudeRecent             = 10  // message states shown by rubi_inbox_status
)

// claudeToolDeadline is how long the socket waits for a tool handler.
var claudeToolDeadline = 5 * time.Second

// boundSister is a Bot bound to a session, as the plugin told the helper.
type boundSister struct {
	Sister string    `json:"sister"`
	Since  time.Time `json:"since"`
}

// deliveredMsg is a message that reached a session. Answered is set when its answer went back (the turn's
// final answer or rubi_reply).
type deliveredMsg struct {
	ID       string    `json:"id"`
	From     string    `json:"from"`
	At       time.Time `json:"at"`
	Answered bool      `json:"answered,omitempty"`
}

type toolRequest struct {
	Tool      string `json:"tool"`
	SessionID string `json:"claude_session_id"` // diagnostics only
	Cwd       string `json:"tool_cwd"`          // diagnostics only
	Text      string `json:"text"`
	Kind      string `json:"kind"`
	MessageID string `json:"message_id"`
}

// ---- the session registry ----

func (r *sessionRegistry) cwdOf(id string) string {
	r.liveMu.Lock()
	defer r.liveMu.Unlock()
	if s := r.live[id]; s != nil {
		return s.cwd
	}
	return ""
}

// ---- bound sisters and delivered messages ----

// setBound records (or forgets) that sister is bound to a session. A Bot has one binding, so binding it
// here takes it off any other session.
func (c *Claude) setBound(sessionID, sister string, bound bool) error {
	sister = oneLineText(sister, claudeMaxFrom)
	if sessionID == "" || len(sessionID) > 200 || sister == "" {
		return errors.New("session_id and sister are required")
	}
	now := time.Now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bound == nil {
		c.bound = map[string][]boundSister{}
	}
	drop := func(id string) {
		keep := c.bound[id][:0:0]
		for _, b := range c.bound[id] {
			if !strings.EqualFold(b.Sister, sister) {
				keep = append(keep, b)
			}
		}
		if len(keep) == 0 {
			delete(c.bound, id)
		} else {
			c.bound[id] = keep
		}
	}
	if !bound {
		drop(sessionID)
		c.dirtyLocked()
		return nil
	}
	for id := range c.bound {
		drop(id)
	}
	if len(c.bound) >= claudeMaxBound { // forget the session bound longest ago
		old, oldAt := "", now
		for id, bs := range c.bound {
			for _, b := range bs {
				if b.Since.Before(oldAt) {
					old, oldAt = id, b.Since
				}
			}
		}
		delete(c.bound, old)
	}
	c.bound[sessionID] = append(c.bound[sessionID], boundSister{Sister: sister, Since: now})
	c.dirtyLocked()
	return nil
}

// noteDeliveredLocked remembers messages that reached a session.
func (c *Claude) noteDeliveredLocked(id string, msgs ...ClaudeMessage) {
	if c.delivered == nil {
		c.delivered = map[string][]deliveredMsg{}
	}
	now := time.Now().UTC()
	ds := c.pruneDeliveredLocked(id, now)
	for _, m := range msgs {
		ds = append(ds, deliveredMsg{ID: m.ID, From: m.From, At: now})
	}
	if len(ds) > claudeMaxDelivered {
		ds = append([]deliveredMsg(nil), ds[len(ds)-claudeMaxDelivered:]...)
	}
	c.delivered[id] = ds
	c.dirtyLocked()
}

// pruneDeliveredLocked drops what is older than the queue TTL and returns the session's list.
func (c *Claude) pruneDeliveredLocked(id string, now time.Time) []deliveredMsg {
	ds := c.delivered[id]
	keep := ds[:0:0]
	for _, d := range ds {
		if now.Sub(d.At) <= claudeQueueTTL {
			keep = append(keep, d)
		}
	}
	if len(keep) == 0 {
		delete(c.delivered, id)
		return nil
	}
	c.delivered[id] = keep
	return keep
}

// markAnsweredLocked notes that the answers to these messages went back.
func (c *Claude) markAnsweredLocked(id string, ids []string) {
	ds := c.delivered[id]
	for i := range ds {
		for _, x := range ids {
			if ds[i].ID == x {
				ds[i].Answered = true
			}
		}
	}
}

// release takes a message out of what the turn still owes a reply for. Senders are recomputed from what is
// left (an older saved turn without per-message senders falls back to its first sender), so a released
// message's sender doesn't stay owed a reply.
func (t *turnState) release(id string) {
	found := false
	var ids, from []string
	for i, x := range t.ids {
		if x == id {
			found = true
			continue
		}
		ids = append(ids, x)
		from = append(from, t.sender(i))
	}
	if !found {
		return
	}
	t.ids = ids
	if len(ids) == 0 {
		t.from, t.idFrom = nil, nil
		return
	}
	t.idFrom, t.from = from, nil
	for _, f := range from {
		dup := false
		for _, x := range t.from {
			dup = dup || x == f
		}
		if !dup {
			t.from = append(t.from, f)
		}
	}
}

// toolAllowLocked counts one use of key and says whether it stays within claudeToolsPerMin a minute. The keys
// (kind and attested session) are bounded: at the limit the key used longest ago is forgotten.
func (c *Claude) toolAllowLocked(key string, now time.Time) bool {
	if c.toolHits == nil {
		c.toolHits = map[string][]time.Time{}
	}
	keep := c.toolHits[key][:0:0]
	for _, t := range c.toolHits[key] {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	if len(keep) >= claudeToolsPerMin {
		c.toolHits[key] = keep
		return false
	}
	if _, known := c.toolHits[key]; !known && len(c.toolHits) >= claudeMaxToolKeys {
		old, oldAt := "", now.Add(time.Hour)
		for k, ts := range c.toolHits {
			if len(ts) == 0 {
				old, oldAt = k, time.Time{}
			} else if last := ts[len(ts)-1]; last.Before(oldAt) {
				old, oldAt = k, last
			}
		}
		delete(c.toolHits, old)
	}
	c.toolHits[key] = append(keep, now)
	return true
}

// toolGlobalLocked counts one tool-made outbox push against the cap for all sessions together.
func (c *Claude) toolGlobalLocked(now time.Time) bool {
	keep := c.toolGlobal[:0:0]
	for _, t := range c.toolGlobal {
		if now.Sub(t) < time.Minute {
			keep = append(keep, t)
		}
	}
	c.toolGlobal = keep
	if len(keep) >= claudeToolsGlobal {
		return false
	}
	c.toolGlobal = append(c.toolGlobal, now)
	return true
}

// ---- the socket ----

// serveTool answers one RubiTool line. It takes a tool slot (none free: an error answer) and gives the
// handler claudeToolDeadline; the handler does memory work only.
//
// It returns the session and what happened, for the hook log line (see hooklog.go).
func (c *Claude) serveTool(conn net.Conn, line []byte, slots chan struct{}, peer peerID) (string, string) {
	select {
	case slots <- struct{}{}:
	default:
		_ = writeJSONLine(conn, map[string]string{"error": "Rubi Home is handling too many tool calls at once; try again in a moment"})
		return lineSession(line), "slots full"
	}
	sid, err := c.sessionOfPeer(peer) // the kernel's answer, before anything else
	if err != nil {
		<-slots
		_ = writeJSONLine(conn, toolErr(err))
		return "", "refused (no session)"
	}
	done := make(chan map[string]any, 1)
	go func() {
		defer func() { <-slots }()
		defer func() {
			if p := recover(); p != nil {
				log.Printf("claude: a tool call failed: %v", p)
				done <- map[string]any{"error": "Rubi Home failed to handle the call"}
			}
		}()
		done <- c.tool(line, sid)
	}()
	t := time.NewTimer(claudeToolDeadline)
	defer t.Stop()
	select {
	case r := <-done:
		_ = writeJSONLine(conn, r)
		if r["error"] != nil {
			return sid, "error"
		}
		return sid, "ok"
	case <-t.C:
		log.Printf("claude: a tool call took too long")
		_ = writeJSONLine(conn, map[string]string{"error": "Rubi Home took too long; the call may or may not have gone through (rubi_inbox_status shows queued_outgoing)"})
		return sid, "timeout"
	}
}

// toolName is the name of the tool a RubiTool line calls, for the hook log line: one of ours or "?".
func toolName(line []byte) string {
	var req toolRequest
	if json.Unmarshal(line, &req) == nil {
		switch req.Tool {
		case "rubi_sisters", "rubi_ask", "rubi_reply", "rubi_inbox_status":
			return req.Tool
		}
	}
	return "?"
}

func toolErr(err error) map[string]any { return map[string]any{"error": err.Error()} }

// tool runs one tool call.
func (c *Claude) tool(line []byte, id string) map[string]any {
	var req toolRequest
	if json.Unmarshal(line, &req) != nil {
		return toolErr(errors.New("Rubi Home couldn't read the call"))
	}
	switch req.Tool {
	case "rubi_sisters", "rubi_ask", "rubi_reply", "rubi_inbox_status":
	default:
		return toolErr(fmt.Errorf("Rubi Home doesn't know the tool %q (it may be older than the plugin)", oneLineText(req.Tool, 40)))
	}
	if req.SessionID != "" && req.SessionID != id && c.diagOnce.CompareAndSwap(false, true) {
		log.Printf("claude: a tool call names another session than the one its process belongs to (the process wins)")
	}
	now := time.Now().UTC()
	var err error
	var res map[string]any
	switch req.Tool {
	case "rubi_sisters":
		res = c.toolSisters(id, now)
	case "rubi_ask":
		res, err = c.toolAsk(id, req, now)
	case "rubi_reply":
		res, err = c.toolReply(id, req, now)
	case "rubi_inbox_status":
		res = c.toolInboxStatus(id, now)
	}
	if err != nil {
		return toolErr(err)
	}
	res["ok"] = true
	return res
}

func (c *Claude) toolSisters(id string, now time.Time) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	sisters := []map[string]any{}
	for _, b := range c.bound[id] {
		sisters = append(sisters, map[string]any{"name": b.Sister, "since": b.Since})
	}
	type sender struct {
		last time.Time
		n    int
	}
	seen := map[string]*sender{}
	var order []string
	for _, d := range c.pruneDeliveredLocked(id, now) {
		s := seen[d.From]
		if s == nil {
			s = &sender{}
			seen[d.From] = s
			order = append(order, d.From)
		}
		s.n++
		if d.At.After(s.last) {
			s.last = d.At
		}
	}
	senders := []map[string]any{}
	for _, f := range order {
		senders = append(senders, map[string]any{"name": f, "messages": seen[f].n, "last": seen[f].last})
	}
	res := map[string]any{"sisters": sisters, "recent_senders": senders}
	if len(sisters) == 0 {
		res["note"] = "Rubi Home doesn't know a Bot bound to this session. A Bot binds itself to a session with the user's approval; an ask is dropped when none is bound."
	}
	return res
}

func (c *Claude) toolAsk(id string, req toolRequest, now time.Time) (map[string]any, error) {
	text := strings.TrimSpace(cleanText(req.Text))
	kind := req.Kind
	switch {
	case text == "":
		return nil, errors.New("text is empty")
	case len([]rune(text)) > claudeMaxAsk:
		return nil, fmt.Errorf("text is longer than %d characters: a sister takes small asks only; name the file or commit instead of pasting it", claudeMaxAsk)
	case kind == "":
		kind = "research"
	case kind != "research" && kind != "audit" && kind != "lookup":
		return nil, errors.New("kind must be research, audit or lookup")
	}
	cwd := c.reg.cwdOf(id)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.toolAllowLocked("ask|"+id, now) {
		return nil, fmt.Errorf("at most %d asks a minute from one session; keep working and ask again later", claudeToolsPerMin)
	}
	if !c.toolGlobalLocked(now) {
		return nil, fmt.Errorf("Rubi Home passes on at most %d sister tool messages a minute; try again shortly", claudeToolsGlobal)
	}
	oid := c.pushOutLocked(ClaudeOut{Kind: "ask", SessionID: id, Cwd: cwd, AskKind: kind, Text: text, At: now})
	res := map[string]any{"id": oid, "queued": true,
		"note": "Don't wait for the answer: it arrives as a message from the sister at your next prompt or stop."}
	if len(c.bound[id]) == 0 {
		res["warning"] = "Rubi Home doesn't know a Bot bound to this session; the ask goes to the Bot bound to this session or its project, and is dropped if there is none."
	}
	return res, nil
}

func (c *Claude) toolReply(id string, req toolRequest, now time.Time) (map[string]any, error) {
	text := strings.TrimSpace(cleanText(req.Text))
	if req.MessageID == "" {
		return nil, errors.New("message_id is missing (rubi_inbox_status lists the messages delivered to this session)")
	}
	if text == "" {
		return nil, errors.New("text is empty")
	}
	text, cut := clip(text, claudeMaxReply)
	cwd := c.reg.cwdOf(id)
	c.mu.Lock()
	defer c.mu.Unlock()
	var m *deliveredMsg
	c.pruneDeliveredLocked(id, now)
	for i := range c.delivered[id] {
		if c.delivered[id][i].ID == req.MessageID {
			m = &c.delivered[id][i]
		}
	}
	if m == nil {
		return nil, errors.New("no message with that id was delivered to this session in the last 24 hours (rubi_inbox_status lists them)")
	}
	if m.Answered {
		return nil, errors.New("that message was already answered (by a reply or by the turn's final answer)")
	}
	if !c.toolAllowLocked("reply|"+id, now) {
		return nil, fmt.Errorf("at most %d replies a minute from one session", claudeToolsPerMin)
	}
	if !c.toolGlobalLocked(now) {
		return nil, fmt.Errorf("Rubi Home passes on at most %d sister tool messages a minute; try again shortly", claudeToolsGlobal)
	}
	oid := c.pushOutLocked(ClaudeOut{Kind: "reply", SessionID: id, Cwd: cwd, To: m.From, InReplyTo: []string{m.ID}, Text: text,
		Truncated: cut, At: now})
	m.Answered = true
	c.pushOutLocked(ClaudeOut{Kind: "status", ID: m.ID, State: "answered", SessionID: id, To: m.From, At: now})
	c.turnLocked(id).release(m.ID) // the turn's final answer doesn't repeat it
	c.dirtyLocked()
	return map[string]any{"id": oid, "to": m.From, "truncated": cut}, nil
}

func (c *Claude) toolInboxStatus(id string, now time.Time) map[string]any {
	waiter := c.hasWaiter(id) // takes wmu, never together with mu
	state := c.reg.stateOf(id)
	if state == "" {
		state = "unknown"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	type st struct {
		id, from, state string
		at              time.Time
	}
	var recent []st
	for _, m := range c.inbox[id] {
		recent = append(recent, st{m.ID, m.From, "queued", m.At})
	}
	for _, m := range c.inflight[id] {
		recent = append(recent, st{m.ID, m.From, "queued", m.At})
	}
	for _, d := range c.pruneDeliveredLocked(id, now) {
		s := "delivered"
		if d.Answered {
			s = "answered"
		}
		recent = append(recent, st{d.ID, d.From, s, d.At})
	}
	for i := 0; i < len(recent); i++ { // newest first (lists are short)
		for j := i + 1; j < len(recent); j++ {
			if recent[j].at.After(recent[i].at) {
				recent[i], recent[j] = recent[j], recent[i]
			}
		}
	}
	if len(recent) > claudeRecent {
		recent = recent[:claudeRecent]
	}
	list := []map[string]any{}
	for _, r := range recent {
		list = append(list, map[string]any{"message_id": r.id, "from": r.from, "state": r.state, "at": r.at})
	}
	owed := []map[string]any{}
	if t := c.turns[id]; t != nil {
		for i, mid := range t.ids {
			owed = append(owed, map[string]any{"message_id": mid, "from": t.sender(i)})
		}
	}
	queuedOut := 0 // asks and replies of this session that Rubi hasn't taken yet
	for _, o := range c.outbox {
		if o.SessionID == id && (o.Kind == "ask" || o.Kind == "reply") {
			queuedOut++
		}
	}
	return map[string]any{"session_state": state, "waiting": len(c.inbox[id]) + len(c.inflight[id]), "waiter": waiter,
		"owed_replies": owed, "recent": list, "queued_outgoing": queuedOut}
}
