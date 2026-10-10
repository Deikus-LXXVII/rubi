# Grouped plugin updates; Bots with a stale tool list

## Problem
1. Rubi emitted one `plugin.update_available` event per plugin, each saying "call rubi_plugin_update(id)".
   Each event woke the Bot in its own run, so the user got one approval link per plugin.
2. Some MCP clients keep the tool list they first saw. A Bot then can't call tools added later
   (rubi_access, new plugin tools), and `rubi_call` only reached plugin tools.

## Fix
- One event for all new updates (`plugins: [...]`, next step: one `rubi_plugin_update(ids)` call).
- At most one pending plugin-update approval: a request whose plugins a pending approval already covers
  gets that approval's link back; otherwise the pending one is cancelled and replaced by one approval for
  all of them. A single-plugin request also takes in the other available (non-quiet) updates; the user
  can untick any.
- `rubi_call` also reaches Rubi's own tools (rubi_access, …), so a stale tool list never blocks a Bot.
- `unknown tool` names the closest matching tools.

## Status
- [x] core changes + tests (reviewed: nested vault read, replace only after the successor exists, single-request answers kept)
- [ ] review, release
