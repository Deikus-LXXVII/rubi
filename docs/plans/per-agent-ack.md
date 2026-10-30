# Per-agent event acks (B6)

## Problem
`internal/events` kept one global `Acked` flag per event. Of several Bots sharing a broadcast event
(update.available, update.completed, approval.decided to followers) only the first got `acked: true`, and
its ack hid the event from the rest. `access.granted` is never stored but its next_step said to ack it.
`rubi_ack` returned a bare flag, so a Bot could not tell "already done" from "wrong id".

## Design
- `Event.AckedBy map[string]time.Time` (lower-cased, trimmed agent name, as core compares names with
  EqualFold). `"*"` (`events.AckedAll`) means every agent.
- `Store.ListFor(agent, includeAcked)` is the per-agent view (`Acked` filled per reader, `AckedBy` hidden);
  `List` is the full state for saving. `Store.Ack(agent, id)` returns acked / already_acked / unknown_or_expired.
- `Core.AckFor` returns a reason: adds `not_visible` (event exists, not addressed to the agent) and
  `not_stored` (id of a webhook-only message: evt_access_, evt_code_, evt_locked_, evt_stopping_).
- After an ack, if no Bot still sees the event unacked, the event is marked `"*"` so it prunes after an
  hour; otherwise it lives until the 24 h limit.
- Migration: on `Restore`, the old `Acked: true` becomes `AckedBy["*"]`.
- Webhook-only messages (access.granted, agent.code) get a next_step without ack.
- `rubi_ack` output: `{acked, reason}`; `acked` is true only for a new ack (compat: unchanged for the
  first ack, still false otherwise).

## Status
- [x] store, core, mcpserver, tests (events, core), docs, changelog
- [ ] review
