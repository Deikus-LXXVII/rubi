# B2: Wake idle Claude Code sessions, delivery status for senders

Task from Цитри's audit (F3/B2). Branches: core `citri/idle-wake` (from `citri/home-nonblocking`), plugin
`citri/idle-wake` (from `citri/mandate-hardening`). Not pushed.

## Problem
A message for a session reaches it only on UserPromptSubmit or Stop (internal/home/inbox.go). An idle
session (stopped, waiting for the user) fires no hooks, so a message waits until the user touches it.
Inbox and outbox live only in memory; senders never learn what happened to a message.

## What the docs say (code.claude.com/docs/en/hooks.md, checked 2026-10-30)
- `asyncRewake: true` on a command hook: runs in the background; exit 2 wakes Claude, with stderr (or
  stdout) shown to Claude as a system reminder. stdin is the normal event JSON.
- The hook's `timeout` is enforced for asyncRewake hooks (default 600 s); no documented maximum.
- Not documented: that Stop supports asyncRewake; what happens to a running background hook when the
  user sends a new prompt; desktop-app (stream-json) behaviour. Hence a manual check on the Mac (below).

## Design (core, Rubi Home)
1. **Waiter hook.** `rubi-home claude-hook --wait [--wait-limit N]`, registered as a second Stop hook with
   `asyncRewake: true` and a long timeout (example: 3600 s; the waiter gives up at limit − 30 s, exit 0).
   It reads the Stop input, connects to the socket and sends one line: the input with
   `hook_event_name` replaced by `"RubiWait"`. An older helper treats an unknown event as a no-op `{}`,
   so the waiter exits 0 (nothing lost).
2. **Helper side.** serveConn recognises `RubiWait` before the event path; waiters have their own slots
   (64; full → `{}`, waiter exits 0). One waiter per session: a newer one replaces the older (the older
   gets `{"close":true}`). The waiter's connection stays open:
   - a message arrives for that session while it is idle → the helper sends
     `{"wake":"<framed messages>","ids":[…]}`; the waiter writes the text to stderr, replies `ok\n` and
     exits 2. The helper commits on `ok` (marks the messages delivered, notes the turn's senders so the
     answer at the next Stop goes back); on EOF, error or no `ok` within 3 s it puts them back in the
     inbox (front, order kept).
   - messages already waiting when the waiter connects (they came during the Stop) wake it at once.
   - UserPromptSubmit or SessionEnd for the session → `{"close":true}` → exit 0. Helper shutdown or the
     waiter's limit → exit 0.
   - The waiter never blocks Claude Code: it runs in the background; every failure is exit 0.
3. **Persistence.** Inbox, outbox and owed replies (turn senders) are saved to
   `<helper dir>/claude-queue.json` (0600, atomic write, debounced ≤ 1 s, also on stop) and loaded on
   start. TTL: an inbox message older than 24 h expires; an outbox entry older than 24 h is dropped.
4. **Message states.** The outbox gains `kind: "status"` entries `{id, to, session_id, state}` with state
   `queued` (on send), `delivered` (prompt, stop-block or wake), `answered` (its turn's answer went
   back), `expired` (TTL; no `dropped`: an evicted reply or ask is only logged). `claude-sessions.next` already carries
   outbox entries to the plugin.
5. **Send status.** `claude-sessions.send` answers `{queued, id, waiting, session_state, waiter: bool}`
   so the plugin can say "an idle session will be woken now" vs "when the user next prompts it".

## Design (plugin)
- Route `kind:"status"` outs to the bound sister as `claude_message` events
  (`{id, session_id, state}`); unknown kinds are ignored as today.
- `claude_code_status {sister, bind_code}`: the bound session's state, waiting messages, whether a waiter
  is attached, and an ETA text (busy → at its next stop; idle with waiter → within seconds; idle without →
  when the user next prompts it; ended → error).
- `sendTool` uses the new send fields for its ETA text.
- README: the second Stop hook, the manual check, message states.
- examples/managed-settings.json: the second Stop hook (`args: ["claude-hook","--wait"]`,
  `asyncRewake: true`, `timeout: 3600`).

## Tests
- Core: waiter woken by a send (unit, over the real socket); replaced waiter closes; prompt/end closes;
  lost ack requeues; old-helper compatibility (`RubiWait` → `{}`); persistence round trip and TTL;
  status entries; `go test -race ./internal/home/... ./cmd/rubi-home/...`.
- Plugin: status routing, claude_code_status, send ETA; e2e: an idle session gets a send and is woken
  (the driver plays the waiter hook), state events reach the sister; helper restart keeps a queued message.

## Manual check on the Mac (user, after install)
1. Add the second Stop hook to managed settings (needs admin: the file is root-owned).
2. In the desktop app, let a session go idle; from a Bot, send it a message with claude_code_send.
3. Expect within seconds: the session resumes with the message as a system reminder and answers; the
   Bot gets `claude_message` delivered, then the reply and `answered`.
4. Also check: typing a prompt in that idle session closes the waiter (no duplicate delivery).

## Status
- [x] core (Rubi Home: waiter, wake, persistence, statuses; tests -race green; not committed)
- [x] plugin
- [x] e2e (Linux container): 7b idle wake, 7c restart keeps the queue
- [x] review (2 rounds), commits, report; manual Mac check pending (user)
