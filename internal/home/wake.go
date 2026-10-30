package home

// Waking an idle Claude Code session. An idle session fires no hooks, so a message for it would wait until
// the user touches it. A second Stop hook, `rubi-home claude-hook --wait` (asyncRewake: true in Claude
// Code's settings), stays connected to the helper while the session is idle. When a message arrives, the
// helper sends it down that connection, the hook writes it to stderr and exits 2, and Claude Code wakes
// the session with it as a system reminder.
//
// The socket lines (all JSON, one per line):
//
//	hook -> helper   the Stop input with hook_event_name "RubiWait"
//	helper -> hook   {"wake":"<framed messages>","ids":["cm_..."]}   messages for the session
//	hook -> helper   ok                                              taken; the hook writes the text and exits 2
//	helper -> hook   {"close":true}                                  nothing to wait for any more (exit 0)
//	helper -> hook   {}                                              not understood or no slot (exit 0)
//
// The messages leave the inbox when they are sent and return to its front, in order, unless the hook
// acknowledges within claudeWakeAck. Only an acknowledged wake counts as delivered. The hook sends `ok`
// before it writes the text, so an `ok` that can't be sent means no text either. Two small windows remain:
// a hook stalled past claudeWakeAck (or a helper stopping) can still write the text after the messages went
// back to the inbox, so they reach Claude twice; and a hook that dies between `ok` and the write has them
// counted as delivered though Claude never saw them. A wake whose ack never came puts the messages
// back and its waiter has exited: that session is not re-armed and waits for its next prompt or Stop.
// Waiters only wake sessions the registry holds as idle; for a busy one the messages stay queued for the
// turn's own hooks. The waiter never blocks Claude Code (it is a background hook), never prints to stdout
// and exits 0 on every failure (a panic included).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	claudeWaitEvent  = "RubiWait"
	claudeMaxWaiters = 64 // waiters at once
	// ClaudeWaitLimit is how long a waiter stays by default: the example hook timeout (3600 s) less 30 s.
	ClaudeWaitLimit = 3570 * time.Second
	claudeWakeLead  = "While this session was idle, Rubi delivers"
)

// claudeWakeAck is how long the helper waits for the waiter's "ok".
var claudeWakeAck = 3 * time.Second

// claudeWaiter is one connected waiter hook. kick asks it to look at the inbox; shut ends it.
type claudeWaiter struct {
	kick chan struct{}
	shut chan struct{}
	once sync.Once
}

func (w *claudeWaiter) close() { w.once.Do(func() { close(w.shut) }) }

func (w *claudeWaiter) poke() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// addWaiter registers the session's waiter; an older one for the session is closed.
func (c *Claude) addWaiter(id string) *claudeWaiter {
	w := &claudeWaiter{kick: make(chan struct{}, 1), shut: make(chan struct{})}
	c.wmu.Lock()
	if c.waiters == nil {
		c.waiters = map[string]*claudeWaiter{}
	}
	old := c.waiters[id]
	c.waiters[id] = w
	c.wmu.Unlock()
	if old != nil {
		old.close()
	}
	return w
}

func (c *Claude) removeWaiter(id string, w *claudeWaiter) {
	c.wmu.Lock()
	if c.waiters[id] == w {
		delete(c.waiters, id)
	}
	c.wmu.Unlock()
}

// closeWaiter ends the session's waiter (the session is busy again, or over).
func (c *Claude) closeWaiter(id string) {
	c.wmu.Lock()
	w := c.waiters[id]
	delete(c.waiters, id)
	c.wmu.Unlock()
	if w != nil {
		w.close()
	}
}

// kickWaiter tells the session's waiter, if there is one, that messages may be waiting.
func (c *Claude) kickWaiter(id string) {
	c.wmu.Lock()
	w := c.waiters[id]
	c.wmu.Unlock()
	if w != nil {
		w.poke()
	}
}

func (c *Claude) hasWaiter(id string) bool {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.waiters[id] != nil
}

func writeJSONLine(conn net.Conn, v any) error {
	b, _ := json.Marshal(v)
	_ = conn.SetWriteDeadline(time.Now().Add(claudeReadTimeout))
	_, err := conn.Write(append(b, '\n'))
	return err
}

// serveWaiter keeps a waiter's connection until it is woken, closed or gone. It takes a waiter slot; with
// none free, or for input it can't use, the reply is `{}` (the waiter exits 0).
func (c *Claude) serveWaiter(ctx context.Context, conn net.Conn, line []byte, slots chan struct{}) {
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		_ = writeJSONLine(conn, struct{}{})
		return
	}
	defer func() {
		if p := recover(); p != nil {
			log.Printf("claude: a waiter failed: %v", p)
		}
	}()
	var in eventInput
	if json.Unmarshal(line, &in) != nil || in.SessionID == "" || len(in.SessionID) > 200 {
		_ = writeJSONLine(conn, struct{}{})
		return
	}
	id := in.SessionID
	if c.reg.stateOf(id) == "ended" {
		_ = writeJSONLine(conn, map[string]bool{"close": true})
		return
	}
	w := c.addWaiter(id)
	defer c.removeWaiter(id, w)

	// What the waiter says (its "ok") and its going away.
	lines := make(chan []byte, 4)
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		for {
			l, err := readLine(conn, 1024)
			if err != nil {
				return
			}
			select {
			case lines <- l:
			default:
			}
		}
	}()
	w.poke() // messages that came during the Stop wake it at once
	for {
		select {
		case <-w.kick:
			if c.wake(ctx, conn, id, lines, gone) {
				return
			}
		case <-w.shut:
			_ = writeJSONLine(conn, map[string]bool{"close": true})
			return
		case <-ctx.Done():
			_ = writeJSONLine(conn, map[string]bool{"close": true})
			return
		case <-gone:
			return
		}
	}
}

// wake sends the session's waiting messages to the waiter. It reports whether the connection is done
// with (a wake was tried, or the session ended).
func (c *Claude) wake(ctx context.Context, conn net.Conn, id string, lines <-chan []byte, gone <-chan struct{}) bool {
	switch c.reg.stateOf(id) {
	case "ended":
		_ = writeJSONLine(conn, map[string]bool{"close": true})
		return true
	case "idle":
	default: // busy (or unknown): the turn's own hooks deliver; the waiter stays, and a Stop that goes idle kicks it
		return false
	}
	msgs := c.takeForWake(id)
	if len(msgs) == 0 {
		return false
	}
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	if err := writeJSONLine(conn, map[string]any{"wake": formatMessages(msgs, claudeWakeLead), "ids": ids}); err != nil {
		c.restoreWake(id, msgs)
		return true
	}
	acked := func(l []byte) bool { return string(l) == "ok" }
	t := time.NewTimer(claudeWakeAck)
	defer t.Stop()
	ok := false
	select {
	case l := <-lines:
		ok = acked(l)
	case <-gone:
		select { // the ack may be followed by the close at once
		case l := <-lines:
			ok = acked(l)
		default:
		}
	case <-t.C:
	case <-ctx.Done():
	}
	if ok {
		c.commitWake(id, msgs)
	} else {
		c.restoreWake(id, msgs)
	}
	return true
}

// takeForWake takes the messages out of the inbox for a wake; they are tracked as in flight (and saved
// with the queue) until the waiter acknowledges them.
func (c *Claude) takeForWake(id string) []ClaudeMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.inbox[id]) == 0 {
		return nil
	}
	msgs := c.takeLocked(id, time.Now().UTC())
	if len(msgs) == 0 {
		return nil
	}
	if c.inflight == nil {
		c.inflight = map[string][]ClaudeMessage{}
	}
	c.inflight[id] = append(c.inflight[id], msgs...)
	return msgs
}

func (c *Claude) endInflightLocked(id string, msgs []ClaudeMessage) {
	gone := map[string]bool{}
	for _, m := range msgs {
		gone[m.ID] = true
	}
	keep := c.inflight[id][:0:0]
	for _, m := range c.inflight[id] {
		if !gone[m.ID] {
			keep = append(keep, m)
		}
	}
	if len(keep) == 0 {
		delete(c.inflight, id)
	} else {
		c.inflight[id] = keep
	}
}

// commitWake marks acknowledged messages delivered and notes their senders, so the turn's answer goes back.
func (c *Claude) commitWake(id string, msgs []ClaudeMessage) {
	c.mu.Lock()
	c.endInflightLocked(id, msgs)
	c.turnLocked(id).note(msgs)
	c.statusLocked(id, "delivered", msgs...)
	c.noteDeliveredLocked(id, msgs...)
	c.dirtyLocked()
	c.mu.Unlock()
	c.reg.touch(id, "", "", "", "busy", time.Now().UTC()) // the woken session is in a turn now
}

// restoreWake puts unacknowledged messages back at the front of the inbox, in order.
func (c *Claude) restoreWake(id string, msgs []ClaudeMessage) {
	c.mu.Lock()
	c.endInflightLocked(id, msgs)
	if c.inbox == nil {
		c.inbox = map[string][]ClaudeMessage{}
	}
	c.inbox[id] = append(append([]ClaudeMessage(nil), msgs...), c.inbox[id]...)
	c.dirtyLocked()
	c.mu.Unlock()
	c.kickWaiter(id) // another waiter may have come meanwhile
}

// ---- the waiter hook (rubi-home claude-hook --wait) ----

// RunClaudeWait is `rubi-home claude-hook --wait`: it reads a Stop hook's input from in, asks the helper at
// sock to wake this session when a message arrives, and stays up to limit. It returns the exit code: 2
// after writing the messages to stderr (Claude Code wakes the session with them), otherwise 0, whatever
// goes wrong. It prints nothing to stdout.
func RunClaudeWait(in io.Reader, stderr io.Writer, sock string, limit time.Duration) (code int) {
	defer func() {
		if p := recover(); p != nil {
			code = 0
		}
	}()
	raw, err := io.ReadAll(io.LimitReader(in, claudeMaxInput+1))
	if err != nil || len(raw) > claudeMaxInput {
		return 0
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return 0
	}
	m["hook_event_name"] = json.RawMessage(`"` + claudeWaitEvent + `"`)
	var line bytes.Buffer
	if json.NewEncoder(&line).Encode(m) != nil {
		return 0
	}
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return 0
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(limit))
	stamped := append(stampHookLine(bytes.TrimSpace(line.Bytes())), '\n')
	if _, err := conn.Write(stamped); err != nil {
		return 0
	}
	resp, err := readLine(conn, claudeMaxInput)
	if err != nil {
		return 0
	}
	var r struct {
		Wake string `json:"wake"`
	}
	if json.Unmarshal(resp, &r) != nil || strings.TrimSpace(r.Wake) == "" {
		return 0 // {"close":true}, {} (an older helper) or nothing usable
	}
	if _, err := conn.Write([]byte("ok\n")); err != nil {
		return 0 // the helper keeps the messages
	}
	_, _ = io.WriteString(stderr, r.Wake+"\n")
	return 2
}
