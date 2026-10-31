package home

// A documented way for a local process to message a Claude Code session: `rubi-home claude-send`. Claude
// Code documents cross-session messaging only for Claude to Claude (the SendMessage tool) and does not
// document the line format of a session's inbox socket, so nothing here writes to it. Instead the message
// goes into Rubi Home's own inbox, and the session gets it through documented hooks: the waiter (an
// asyncRewake Stop hook, wake.go) wakes an idle session, and a busy one takes it at its next prompt or Stop.
// The answer comes back through the same inbox path.
//
//	https://code.claude.com/docs/en/cross-session-messaging.md
//	https://code.claude.com/docs/en/channels.md   (channels don't work in the desktop app)
//	https://code.claude.com/docs/en/hooks.md      (asyncRewake: exit 2 wakes an idle session)
//
// The socket line (JSON, stamped like every hook line) and the answer:
//
//	client -> helper   {"hook_event_name":"RubiSend","op":"send","target_session_id":"..." | "target_name":"...",
//	                    "from":"bot-a","text":"..."}
//	helper -> client   {"ok":true,"queued":true,"id":"cm_...","session_id":"...","session_state":"idle","waiter":true,"waiting":1}
//	                   or {"error":"..."}                                  (an older helper answers {})
//	client -> helper   {"hook_event_name":"RubiSend","op":"get_reply","message_id":"cm_...","wait_seconds":20}
//	helper -> client   {"ok":true,"found":true,"state":"answered","text":"...","truncated":false} / {"ok":true,"found":false}
//
// The line is never an approval, an event or a tool: it is routed before all of them, takes its own slots,
// and can only queue a message for a session the registry knows (the socket is 0600: the same OS user).
// The session id is in target_session_id, not session_id, so a helper that predates this line cannot take it
// for a hook of that session.
//
// A message from a local process has the sender "local:<name>". Anything the session says to that sender
// stays in a small reply box here (localReplies) and never reaches the outbox, so no Bot ever gets it; a Bot's
// own message can't use a sender with that prefix (send).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	claudeSendEvent     = "RubiSend"
	claudeMaxSends      = 8  // RubiSend sends being handled at once
	claudeMaxReplyPolls = 4  // get_reply long-polls at once (their own slots, so waiting clients can't block sends)
	claudeMaxLocalBox   = 50 // local replies kept
	localPrefix         = "local:"
	claudeMaxSendWait   = 3600 * time.Second
	claudeReplyPollMax  = 20 * time.Second // one get_reply waits at most this long
)

// claudeSendDeadline is how long the socket waits for a send handler; claudeLocalReplyTTL how long a local
// sender can fetch a reply.
var (
	claudeSendDeadline  = 5 * time.Second
	claudeLocalReplyTTL = 10 * time.Minute
)

// errSendHelperTooOld is the client's error when the helper doesn't know the RubiSend line.
var errSendHelperTooOld = errors.New("Rubi Home is too old for claude-send (or speaks another hook protocol): update it with the installer, restart it, and run `" + InstallHookCommand + "`")

func isLocalSender(s string) bool {
	return len(s) >= len(localPrefix) && strings.EqualFold(s[:len(localPrefix)], localPrefix)
}

func localSender(name string) string { return localPrefix + name }

// localReply is what a session said to a local sender.
type localReply struct {
	State     string // "answered" or "expired"
	Text      string
	Truncated bool
	SessionID string
	At        time.Time
}

// localOutLocked takes what a session says to a local sender (a reply, or the news that its message
// expired). It is the only place such things go: they never reach the outbox. c.mu is held.
func (c *Claude) localOutLocked(o ClaudeOut) {
	now := time.Now().UTC()
	c.pruneLocalLocked(now)
	put := func(id string, r localReply) {
		if id == "" {
			return
		}
		if c.localReplies == nil {
			c.localReplies = map[string]localReply{}
		}
		if _, have := c.localReplies[id]; !have && len(c.localReplies) >= claudeMaxLocalBox {
			oldest, at := "", now.Add(time.Hour)
			for k, v := range c.localReplies {
				if v.At.Before(at) {
					oldest, at = k, v.At
				}
			}
			delete(c.localReplies, oldest)
		}
		c.localReplies[id] = r
	}
	switch {
	case o.Kind == "reply":
		for _, id := range o.InReplyTo {
			put(id, localReply{State: "answered", Text: o.Text, Truncated: o.Truncated, SessionID: o.SessionID, At: now})
		}
	case o.Kind == "status" && o.State == "expired":
		if _, have := c.localReplies[o.ID]; !have {
			put(o.ID, localReply{State: "expired", SessionID: o.SessionID, At: now})
		}
	default:
		return // queued, delivered and answered statuses: the client polls for the reply itself
	}
	if c.localSig != nil {
		close(c.localSig)
		c.localSig = nil
	}
}

func (c *Claude) pruneLocalLocked(now time.Time) {
	for k, v := range c.localReplies {
		if now.Sub(v.At) > claudeLocalReplyTTL {
			delete(c.localReplies, k)
		}
	}
}

// localReplyWait returns the reply to a message, waiting up to wait for it.
func (c *Claude) localReplyWait(ctx context.Context, id string, wait time.Duration) (localReply, bool) {
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		c.mu.Lock()
		c.pruneLocalLocked(time.Now().UTC())
		if r, ok := c.localReplies[id]; ok {
			c.mu.Unlock()
			return r, true
		}
		if c.localSig == nil {
			c.localSig = make(chan struct{})
		}
		sig := c.localSig
		c.mu.Unlock()
		select {
		case <-sig:
		case <-t.C:
			return localReply{}, false
		case <-ctx.Done():
			return localReply{}, false
		}
	}
}

// sessionNames are the names a user can call a session by. Aliases plug in here.
func sessionNames(s ClaudeSession) []string { return []string{s.Title} }

// resolveName finds the one live session called name (case-insensitive; archived and ended sessions are
// left out).
func (c *Claude) resolveName(name string) (string, error) {
	name = oneLineText(name, 200)
	if name == "" {
		return "", errors.New("name is empty")
	}
	all, _ := c.allSessions(false)
	var hits []ClaudeSession
	for _, s := range all {
		if s.State == "ended" || s.SessionID == "" {
			continue
		}
		for _, n := range sessionNames(s) {
			if n != "" && strings.EqualFold(n, name) {
				hits = append(hits, s)
				break
			}
		}
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no live session is called %q (use --session with an id from claude-sessions.list)", name)
	case 1:
		return hits[0].SessionID, nil
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].LastActivity.After(hits[j].LastActivity) })
	var ids []string
	for i, h := range hits {
		if i == 5 {
			ids = append(ids, "…")
			break
		}
		ids = append(ids, oneLineText(h.SessionID, 80))
	}
	return "", fmt.Errorf("%d sessions are called %q (%s): use --session with one of the ids", len(hits), name, strings.Join(ids, ", "))
}

type sendRequest struct {
	Op        string `json:"op"`
	SessionID string `json:"target_session_id"`
	Name      string `json:"target_name"`
	From      string `json:"from"`
	Text      string `json:"text"`
	MessageID string `json:"message_id"`
	Wait      int    `json:"wait_seconds"`
}

// localSend validates and queues one message from a local process.
func (c *Claude) localSend(req sendRequest) (map[string]any, error) {
	from := oneLineText(req.From, claudeMaxFrom)
	text := strings.TrimSpace(req.Text)
	switch {
	case (req.SessionID == "") == (req.Name == ""):
		return nil, errors.New("give exactly one of a session id and a name")
	case len(req.SessionID) > 200:
		return nil, errors.New("session id is too long")
	case from == "":
		return nil, errors.New("from is missing")
	case text == "":
		return nil, errors.New("text is empty")
	case utf8.RuneCountInString(text) > claudeMaxText:
		return nil, fmt.Errorf("text is longer than %d characters: send a short ask, not a task", claudeMaxText)
	}
	if err := checkTaskText(text); err != nil { // rejected, never stripped: nothing may hide in the message
		return nil, errors.New(strings.Replace(err.Error(), "prompt", "text", 1))
	}
	id := req.SessionID
	if id == "" {
		var err error
		if id, err = c.resolveName(req.Name); err != nil {
			return nil, err
		}
	}
	// State and waiter as the message found them (a send may wake the session at once).
	state, waiter := c.reg.stateOf(id), c.hasWaiter(id)
	if state == "" {
		state = "unknown"
	}
	m, waiting, err := c.send(id, from, text, true)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "queued": true, "id": m.ID, "session_id": id, "session_state": state, "waiter": waiter,
		"waiting": waiting}, nil
}

// serveSend answers one RubiSend line. It takes a send slot (none free: an error answer). It returns what
// happened for the hook log line (see hooklog.go): never the text.
func (c *Claude) serveSend(ctx context.Context, conn net.Conn, line []byte, sends, replies chan struct{}) (string, string) {
	var req sendRequest
	if json.Unmarshal(line, &req) != nil {
		_ = writeJSONLine(conn, map[string]string{"error": "Rubi Home couldn't read the call"})
		return "", "error"
	}
	slots := sends
	if req.Op == "get_reply" { // long-polls have their own slots
		slots = replies
	}
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	default:
		_ = writeJSONLine(conn, map[string]string{"error": "Rubi Home is handling too many claude-send calls at once; try again in a moment"})
		return "", "slots full"
	}
	switch req.Op {
	case "send":
		done := make(chan map[string]any, 1)
		go func() { // memory work and a brief wait for the first scan; the deadline covers a stuck scan
			defer func() {
				if p := recover(); p != nil {
					log.Printf("claude: a claude-send failed: %v", p)
					done <- map[string]any{"error": "Rubi Home failed to handle the call"}
				}
			}()
			res, err := c.localSend(req)
			if err != nil {
				res = toolErr(err)
			}
			done <- res
		}()
		t := time.NewTimer(claudeSendDeadline)
		defer t.Stop()
		select {
		case r := <-done:
			_ = writeJSONLine(conn, r)
			if r["error"] != nil {
				return "", "send error"
			}
			return fmt.Sprint(r["session_id"]), "send queued " + fmt.Sprint(r["id"])
		case <-t.C:
			log.Printf("claude: a claude-send took too long")
			_ = writeJSONLine(conn, map[string]string{"error": "Rubi Home took too long; the message may or may not have been queued"})
			return "", "send timeout"
		}
	case "get_reply":
		if !strings.HasPrefix(req.MessageID, "cm_") || len(req.MessageID) > 40 {
			_ = writeJSONLine(conn, map[string]string{"error": "message_id is not a message id"})
			return "", "error"
		}
		wait := time.Duration(req.Wait) * time.Second
		if wait < 0 {
			wait = 0
		}
		wait = min(wait, claudeReplyPollMax)
		r, ok := c.localReplyWait(ctx, req.MessageID, wait)
		if !ok {
			_ = writeJSONLine(conn, map[string]any{"ok": true, "found": false})
			return "", "get_reply none"
		}
		_ = writeJSONLine(conn, map[string]any{"ok": true, "found": true, "state": r.State, "text": r.Text,
			"truncated": r.Truncated, "session_id": r.SessionID})
		return r.SessionID, "get_reply " + r.State
	}
	_ = writeJSONLine(conn, map[string]string{"error": "Rubi Home doesn't know that claude-send call"})
	return "", "error"
}

// ---- the client (rubi-home claude-send) ----

const sendUsage = "usage: rubi-home claude-send (--session ID | --name TITLE) --from NAME [--wait-reply SECONDS]   (the text on stdin)\n" +
	"       rubi-home claude-send --get-reply MESSAGE_ID"

// callSend sends one RubiSend line and returns the helper's answer.
func callSend(sock string, req map[string]any, timeout time.Duration) (map[string]any, error) {
	req["hook_event_name"] = claudeSendEvent
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return nil, errors.New("Rubi Home isn't running on this computer (its socket doesn't answer)")
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(append(stampHookLine(b), '\n')); err != nil {
		return nil, errors.New("Rubi Home closed the connection")
	}
	resp, err := readLine(conn, claudeMaxInput)
	if err != nil {
		return nil, fmt.Errorf("Rubi Home gave no answer: %v", err)
	}
	var out map[string]any
	if json.Unmarshal(resp, &out) != nil {
		return nil, errors.New("Rubi Home gave an answer that couldn't be read")
	}
	if e, ok := out["error"].(string); ok {
		return nil, errors.New(oneLineText(e, 500))
	}
	if out["ok"] != true { // {} from a helper that doesn't know the line
		return nil, errSendHelperTooOld
	}
	return out, nil
}

// RunClaudeSend is `rubi-home claude-send`. It reads the text from in, queues it with the helper at sock and,
// with --wait-reply, prints the answer to out (everything else goes to errw). Exit codes: 0 done, 1 failed,
// 2 wrong usage, 3 no reply within the time (the message stays queued; --get-reply fetches the answer later).
func RunClaudeSend(args []string, in io.Reader, out, errw io.Writer, sock string) int {
	var session, name, from, getReply string
	wait := 0
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) {
			fmt.Fprintln(errw, sendUsage)
			return 2
		}
		v := args[i+1]
		switch args[i] {
		case "--session":
			session = v
		case "--name":
			name = v
		case "--from":
			from = v
		case "--get-reply":
			getReply = v
		case "--wait-reply":
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || time.Duration(n)*time.Second > claudeMaxSendWait {
				fmt.Fprintln(errw, "rubi-home claude-send: --wait-reply takes a number of seconds from 1 to 3600")
				return 2
			}
			wait = n
		default:
			fmt.Fprintln(errw, sendUsage)
			return 2
		}
		i++
	}
	if getReply != "" {
		if session != "" || name != "" || from != "" || wait != 0 {
			fmt.Fprintln(errw, sendUsage)
			return 2
		}
		return printReply(sock, getReply, 0, out, errw)
	}
	if (session == "") == (name == "") || strings.TrimSpace(from) == "" {
		fmt.Fprintln(errw, sendUsage)
		return 2
	}
	raw, err := io.ReadAll(io.LimitReader(in, 4*claudeMaxText+1))
	if err != nil {
		fmt.Fprintf(errw, "rubi-home claude-send: couldn't read the text: %v\n", err)
		return 1
	}
	if len(raw) > 4*claudeMaxText || !utf8.Valid(raw) {
		fmt.Fprintf(errw, "rubi-home claude-send: the text is not valid text or is longer than %d characters\n", claudeMaxText)
		return 1
	}
	text := strings.TrimSpace(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	switch {
	case text == "":
		fmt.Fprintln(errw, "rubi-home claude-send: the text on stdin is empty")
		return 1
	case utf8.RuneCountInString(text) > claudeMaxText:
		fmt.Fprintf(errw, "rubi-home claude-send: the text is longer than %d characters: send a short ask, not a task\n", claudeMaxText)
		return 1
	}
	if err := checkTaskText(text); err != nil {
		fmt.Fprintf(errw, "rubi-home claude-send: %s\n", strings.Replace(err.Error(), "prompt", "text", 1))
		return 1
	}
	req := map[string]any{"op": "send", "from": from, "text": text}
	if session != "" {
		req["target_session_id"] = session
	} else {
		req["target_name"] = name
	}
	res, err := callSend(sock, req, 10*time.Second)
	if err != nil {
		fmt.Fprintf(errw, "rubi-home claude-send: %v\n", err)
		return 1
	}
	id, _ := res["id"].(string)
	fmt.Fprintf(errw, "queued %s for session %v (%v, waiter %v)\n", id, res["session_id"], res["session_state"], res["waiter"])
	if res["waiter"] != true && res["session_state"] == "idle" {
		fmt.Fprintln(errw, "note: no waiter hook is attached to this idle session; the message is delivered when it next starts a turn (see docs/design/rubi-home.md)")
	}
	if wait == 0 {
		fmt.Fprintln(out, id)
		return 0
	}
	return printReply(sock, id, time.Duration(wait)*time.Second, out, errw)
}

// printReply waits up to total for the reply to message id and prints it.
func printReply(sock, id string, total time.Duration, out, errw io.Writer) int {
	deadline := time.Now().Add(total)
	for first := true; ; first = false {
		left := time.Until(deadline)
		if !first && left < time.Second { // less than a second is not worth another poll
			fmt.Fprintf(errw, "rubi-home claude-send: no reply to %s yet; fetch it later (within 10 minutes of its arrival) with: rubi-home claude-send --get-reply %s\n", id, id)
			return 3
		}
		w := min((max(left, 0)+time.Second-1)/time.Second*time.Second, claudeReplyPollMax) // whole seconds, rounded up
		res, err := callSend(sock, map[string]any{"op": "get_reply", "message_id": id, "wait_seconds": int(w / time.Second)}, w+10*time.Second)
		if err != nil {
			fmt.Fprintf(errw, "rubi-home claude-send: %v\n", err)
			return 1
		}
		if res["found"] == true {
			if res["state"] == "expired" {
				fmt.Fprintf(errw, "rubi-home claude-send: message %s expired before the session took it\n", id)
				return 1
			}
			text, _ := res["text"].(string)
			fmt.Fprintln(out, text)
			if res["truncated"] == true {
				fmt.Fprintln(errw, "note: the reply was cut short")
			}
			return 0
		}
	}
}
