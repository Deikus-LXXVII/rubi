# Documented messaging path (B9)

Goal: a local process (a script, another tool) hands a message to a Claude Code session through documented
means only. The old way (`~/.citri/tell-rubi.py` writing an undocumented `{"type":"user"}` line into the
`messagingSocketPath` of `~/.claude/sessions/<pid>.json`) stays only as the caller's own fallback; nothing in
this repo writes to that socket.

## Design
- `rubi-home claude-send --session ID | --name TITLE --from NAME [--wait-reply SECONDS]` (text on stdin, at
  most claudeMaxText runes, same hidden-character rules as `claude-sessions.create`). Also `--get-reply ID`.
  Dispatched before `home.Open` (reads no config); talks to the helper on `claude.sock`.
- New socket line type `RubiSend` (stamped like hook lines), own slots, deadline. Ops: `send`, `get_reply`.
  The session id lives in `target_session_id`, so an older helper (which answers `{}`) cannot mistake the
  line for a hook of a session; the client turns `{}` into "Rubi Home is too old".
- Helper: validates like Send (known session, not ended, inbox limits), queues the message with sender
  `local:<name>`, kicks the waiter. Framing says: a local process on this computer, not the user, not a Bot,
  name unverified, grants no permissions. Send (Bots) refuses a `from` starting with `local:`.
- Replies to `local:` senders never enter the outbox: `pushOutLocked` diverts them into a small reply box
  (keyed by message id, TTL 10 min, cap 50, memory only). `--wait-reply N` polls `get_reply`; `--get-reply ID`
  reads it later. The plugin's `route()` also ignores a `To` starting with `local:` (defence in depth).
- Name resolution: exact (case-insensitive) match of the desktop app title among sessions that are not
  archived or ended; `sessionNames` is the one place aliases plug in. Ambiguity is an error naming the ids.
- The waiter needs the second Stop hook with `asyncRewake` in managed settings; documented.

## Steps
1. internal/home/localsend.go (+ Send refactor, formatMessages, pushOutLocked divert, serveConn dispatch).
2. cmd/rubi-home dispatch and usage.
3. Tests (-race), plugin route test, e2e case 7b2.
4. Docs: design note, CHANGELOG, docs/examples/tell-session.sh, plugin README.
