# Changelog

## v0.12.1 (2026-10-29)

- **Webhook key from a file Rubi reads (`key_file`).** v0.12.0's recipe passed the routine key to
  `rubi_agent_connect` as `"$file:/workspace/.rubi-hook-key"`; the Grok Bot runtime redacts known secrets
  inside that substitution, so Rubi stored `[REDACTED]` and the routine refused the key on `webhook.test`.
  `rubi_agent_connect` now takes `key_file: <path>`: the daemon, which serves the MCP tools (`rubi mcp`
  only relays stdio to its socket), reads the file and deletes it, success or failure. The path must be
  absolute and clean, under `/workspace`, `/tmp` or `$TMPDIR` (links in its directory resolved), a regular
  file that is not a symlink, owned by Rubi's user, mode 0600 or stricter, at most 4 KB; whitespace is
  trimmed. `key` and `key_file` together are refused.
- **A Bot's key is checked before the approval.** A redaction placeholder (`[REDACTED]`, only `*` or `•`), fewer
  than 20 characters, whitespace inside or a literal `$file:` is refused with a hint to use `key_file`; for
  a Grok Bot routine address the key must be `crsr_` plus letters, digits, `_` or `-`. A whole
  `Authorization: Bearer <key>` header or `Bearer <key>` is still accepted and normalized. Keys the user
  pastes or sets in settings are unchanged.
- **Recipe.** `rubi_status` (`webhook.how`, `webhook.note`), the MCP instructions, the
  `rubi_agent_connect` description and the Grok Bot skill now say `umask 077; printenv
  RUBI_WEBHOOK_KEY_<NAME> > /workspace/.rubi-hook-key` and `key_file`, with no `$file:` substitution.

## v0.12.0 (2026-10-29)

> **Behaviour change / Изменение поведения.**
> EN: events that rubi-mailkit or rubi-telegram address to a named Bot with no registered webhook (or to
> an unknown Bot) are now dropped and audited as `event.dropped`, instead of going to all subscribers.
> RU: события, которые rubi-mailkit или rubi-telegram адресуют названному Боту без зарегистрированного
> вебхука (или неизвестному Боту), теперь отбрасываются и записываются в аудит как `event.dropped`, а не
> уходят всем подписчикам.

- **Strict delivery of addressed events (breaking for plugins that relied on the fallback).** An event a
  plugin addresses to one Bot (`EmitTo`) goes to that Bot's webhook only. If no Bot of that name is
  connected with a webhook, Rubi drops the event, records `event.dropped` (plugin, type, name) in the audit
  log and returns an error to the plugin; it never falls back to the subscribers or the administrator. In
  the event list an addressed event is its Bot's alone (the administrator no longer sees other Bots'
  addressed events).
- **Verified caller for plugins.** A plugin tool call may name the Bot (`rubi_agent`) with a proof only its
  webhook receives: `rubi_agent_code` (the `agent_code` of its latest webhook, or from `rubi_verify`) or
  its `rubi_access` code for that plugin. Rubi checks it, strips the code and passes the plugin
  `rubi_verified_agent`, the Bot's registered name. A caller-supplied `rubi_verified_agent` is always
  stripped. A wrong `rubi_agent_code` or an unknown name fails the call.
- **Claude Code sessions in Rubi Home.** The `claude-hook` also reports SessionStart, UserPromptSubmit,
  Stop and SessionEnd; Rubi Home keeps a read-only registry of sessions (Claude desktop app store,
  transcripts in `~/.claude/projects`, project from the origin remote, main vs sub-session) and a per-
  session inbox: queued messages reach a session through UserPromptSubmit context or by blocking Stop
  (at most 5 times in a row); the turn's answer and ```` ```rubi-ask ```` blocks go back to the sender.
  The helper never starts or resumes a session. This works in bypassPermissions sessions too (those hooks
  still run there; only permission requests don't).
- **New Rubi Home capability `claude-sessions`**, separate from `claude` and shown on the install screen:
  `claude-sessions.list`, `claude-sessions.send` and `claude-sessions.next` (replies and asks). `claude`
  keeps only permission requests (`claude.next`, `claude.decide`); `claude.next` no longer carries session
  messages, and polling `claude-sessions.next` doesn't count as Rubi being there to approve.

- **Pending approvals in the panel.** Settings now shows a "Waiting for you" list of every request still
  waiting for a tap or passkey, and each one opens the usual approval screen. New panel ops are
  `approval.list` (no previews) and `approval.open` (mints a ticket for one request). Both need a full
  `settings` ticket.
- **Plugin approvals reach the user.** An approval a plugin submits outside a tool call (for example a
  Claude Code permission request) used to wake no one unless a Bot followed that plugin. Rubi now
  emits `rubi` `approval.requested` to the administrator, and the webhook carries a fresh approval link and
  a `next_step` to send it. This is skipped when the tool result or the plugin's own event already
  brought a Bot the link.
- **Connecting a Bot takes one paste.** The panel's Connect page reads the routine's whole Webhook
  section (or a curl line) in any order, says what is missing, refuses non-routine hosts and goes straight
  to the passkey. Pasted keys must be one printable token (no header injection), and URLs with userinfo are
  refused. Addresses that aren't a Grok Bot routine still work but the approval carries a warning. After
  saving, the panel waits up to 20 s for the routine's answer to a test event and explains a failure in
  plain words (wrong key, routine off, no routine, unreachable) without showing the address or key. The
  Bot guidance in `rubi_status`, the MCP instructions, `rubi_link` (new `send_with` hint) and the Bot skill
  all ask for one message with both links.
- **Bots propose their own connection.** New MCP tool `rubi_agent_connect(name, url?, key?, events?,
  plugins?, reason?)`: a Bot proposes its routine webhook; Rubi keeps the key sealed in memory (never on
  disk, in logs or on screen) and changes nothing until the user approves with a passkey. The approval
  shows the Bot, the webhook's host and path and the events, and highlights when it replaces an existing
  webhook (applying fails if that webhook changed meanwhile). On approval Rubi applies it and sends a test
  event; deny, expiry, lock and restart discard the key.
- **Bots ask for plugin access.** New MCP tool `rubi_access_request(agent, plugins: [{id, accounts?}],
  reason)` asks to be assigned plugin accounts for good. Each account is one item the user can leave out;
  a Bot's requests (connect and access) are merged into one pending approval with one link. Rate-limited
  (10 per Bot, 30 overall per hour).
- **Store search.** `rubi_store` takes optional `query` and `capability`; it searches the signed catalog
  and returns per match the latest version, `min_rubi`, installed/connected, what the plugin needs and the
  next step. Without arguments the output is unchanged.
- **Connecting a Grok Bot: key through a secret field.** The main path: the user gives the routine URL in
  chat and the key in a masked secret field (env `RUBI_WEBHOOK_KEY_<NAME>`); the Bot passes it to
  `rubi_agent_connect` as `"$file:<path>"` from a 600 file it shreds right after, and the user confirms
  with the passkey. A Bot may propose only Grok Bot routine webhooks; others are refused (the user's own
  settings still accept them, with a warning).
- **Fallback: paste on the approval screen.** `url` and `key` are optional. Without a key the approval screen asks the user to paste the routine's Webhook section
  (prefilled with the Bot's `url`) and confirm with the passkey in the same step. Approvals can now carry
  typed inputs (`approval.get` lists them; `approval.challenge` and `approval.decide` take `inputs`). The
  passkey or password proof covers a digest of the pasted values, so they can't be swapped after signing;
  they are never stored or echoed. A key passed with a trailing newline (read from a file) is trimmed, and
  a literal `$file:` reference is refused. Bot requests appear in "Waiting for you" like any other.
- Panel: `replaces` is highlighted on approval screens; Bot requests have their own labels.
