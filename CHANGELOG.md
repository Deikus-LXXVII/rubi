# Changelog

## Unreleased

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
