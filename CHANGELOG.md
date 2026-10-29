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
- **Bots propose their own connection.** New MCP tool `rubi_agent_connect(name, url, key, events?,
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
- Panel: `replaces` is highlighted on approval screens; Bot requests have their own labels.
