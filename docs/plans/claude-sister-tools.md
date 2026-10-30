# B3: Sister tools inside Claude Code, installed with zero setup

Branches: core `citri/claude-sister-tools` (from `citri/hook-version`), plugin `citri/claude-sister-tools`.
Not pushed.

## Problem
Claude talks to its sister (the Grok Bot bound to its session) only through text: the final answer goes back as a
reply and ```` ```rubi-ask ```` blocks become asks. Instructions are a skill the user copies by hand. Claude has no
real tools, cannot ask "who is my sister" or answer one message early, and nothing installs by itself.

## What the docs say (checked 2026-10-30)
- env-vars.md: `CLAUDE_CODE_SESSION_ID` is set "in ... stdio MCP server subprocesses". It matches the hooks'
  `session_id` but "an MCP server subprocess retains the ID it was spawned with": after `/clear` the hooks
  carry a new id; on `--continue`/`--resume` without an id it "may receive the initial startup ID instead".
- settings-reference.md: `extraKnownMarketplaces` takes `{name: {source: {source: "directory", path}}}`;
  `enabledPlugins` takes `"plugin@marketplace": true`. Both work in managed settings; a plugin with no hooks is
  untouched by `allowManagedHooksOnly`. `managed-mcp.json` is not used (it takes exclusive control of MCP).
- plugins-reference.md: a plugin's `.mcp.json` at its root; `claude plugin validate <dir>` checks marketplace and plugin.

## Design (core, Rubi Home)
1. **`rubi-home claude-mcp`**: a stdio MCP server (go-sdk), dispatched before `home.Open` (reads no config). Each
   tool call is one socket line to `claude.sock`: `{"hook_event_name":"RubiTool","tool":...,
   "claude_session_id":...,"tool_cwd":...,args}`, stamped by `stampHookLine`. The session id is in
   `claude_session_id` (not `session_id`) so an older helper's event path ignores the line. An older helper
   answers `{}` (unknown event): the tools say "Rubi Home is too old".
2. **Helper side.** `serveConn` routes `RubiTool` right after `noteHook`, before the waiter and the event path:
   it can never be an approval or an event. Own slots (16, full -> `{"error":...}`), a per-call deadline (5 s), a
   panic guard. Replies are `{"ok":true,...}` or `{"error":"..."}`. Hooks are never blocked: the handler does
   memory work only, under the existing locks.
3. **Which session.** First version: `CLAUDE_CODE_SESSION_ID` first, cwd as the fallback. Replaced after review
   (the line is forgeable by any same-user process, and the fallback could pick the wrong session): the kernel
   attests it. The helper takes the socket peer's pid (`LOCAL_PEERPID` / `SO_PEERCRED`), records the ancestors
   (6 levels, with start times) of every process that sends a session's hook as hosting that session
   (ambiguous when a process hosts two), and attributes a tool call to the first ancestor of its peer that
   hosts exactly one session. `claude_session_id` and `tool_cwd` are diagnostics only. Second review round: the
   identity is anchored on the session's own `claude` process (first ancestor named `claude` within 6 levels,
   else the direct parent; never shells or the app), the first claim on a process wins, claims end with
   a SessionEnd from the claiming process, a start-time mismatch or 24 h of silence (or move after 10 minutes of
   silence when another session's hook arrives from the same process), and the peer is read right after Accept. The text below is the
   first version, kept for the record.
   - Env id known to the hooks and not ended: it is the session.
   - Otherwise (id empty, ended by `/clear`, unknown): the live sessions whose hook-reported cwd equals the
     MCP server's cwd. One busy one wins; if no busy one, a single candidate wins; several busy ones, or
     several non-busy ones and none busy, are ambiguous. None or ambiguous: the tool errors with
     "Rubi Home can't tell which session this is yet; retry after the session's next prompt", nothing else.
   - Env id given but never seen by a hook and cwd finds nothing: the env id is used (the documented id).
   Ambiguity is an error and not a guess because a wrong guess would let one session act as another's sister.
4. **Bound sisters.** New op `claude-sessions.bound {session_id, sister, bound}` (capability claude-sessions,
   already covered by the `claude-sessions.` prefix). The helper keeps `bound[session] = [{sister, since}]`
   and saves it in `claude-queue.json` (new optional keys `bound`, `delivered`; `version` stays 1: older
   helpers ignore unknown keys).
5. **Delivered ring.** Every delivery (prompt, stop block, wake) is remembered per session
   `{id, from, at, answered}` (100 per session, 24 h, saved). `rubi_reply` and `rubi_inbox_status` use it.
6. **Tools** (input schemas described in plain English):
   - `rubi_sisters {}`: bound Bots (name, since) + senders of messages delivered in the last 24 h.
   - `rubi_ask {text, kind?}`: the same checks as a ```` ```rubi-ask ```` block (kind research|audit|lookup, <= 2000
     chars) but an error instead of silently dropping; at most 3 per minute per session. Queues
     `ClaudeOut{kind:ask}`, returns its id.
   - `rubi_reply {message_id, text}`: only for a message delivered to this session within 24 h; text <=
     `claudeMaxReply`. Queues `ClaudeOut{kind:reply, To: the message's sender, InReplyTo:[id]}` (the plugin's
     `route` requires `To` to be the bound Bot, so no plugin change), emits `answered`, and releases that
     message from the turn's owed replies: the turn's final answer is then not sent again for it (the final
     answer still goes to the senders of messages not replied to). So an explicit reply is the whole answer to
     that message. At most 3 per minute per session.
   - `rubi_inbox_status {}`: state, waiting messages, owed replies (ids, senders), waiter attached, recent
     message states for the session.
7. **Plugin + marketplace, embedded.** `claude-plugin/` in the core repo (`go:embed`): marketplace `rubi`, plugin
   `rubi-sisters` (skill + `.mcp.json` running `/usr/local/bin/rubi-home claude-mcp`).
8. **`install-hook --with-claude-plugin`** (opt-in, prints a hint when absent): installs the embedded tree to
   `/Library/Application Support/Rubi Home/claude-plugin` as root with no TOCTOU: file contents are inline in
   the privileged script (per-file base64, so nothing user-writable is read), decoded by root with absolute
   tool paths into a temp directory beside the target, then `mv` of the directory (the old one is moved aside
   first, restored if the rename fails, removed after). Same ancestor checks as the binary.

## Design (plugin repo)
- Calls `claude-sessions.bound` on bind (new session), on re-bind replacing another, on unbind (tool and panel),
  and when a binding follows a new session id. An older Rubi Home (op unknown) is logged once and ignored.
- `examples/managed-settings.json`: `extraKnownMarketplaces.rubi` (directory source) + `enabledPlugins`.
- README: tools, install, no added trust, manual check. The kit skill moves to the core embed.

## Tests
Core: MCP round trip (in-memory transport + real socket helper): list, sisters after `bound`, ask in the outbox,
reply rules (delivered only, To/InReplyTo, no duplicate final answer, `answered`), inbox_status, session
identification (env, cwd match, none, ambiguous, ended id), old helper `{}`, RubiTool is no approval/event,
slots, persistence, embedded files, install dry-run/fake runner/atomic rename/failed rename keeps old dir.
Plugin: bound calls, old-home tolerance, route of tool asks/replies, examples test. `claude plugin validate`,
`go test -race ./...` in both, Docker e2e must print E2E PASS.

## Status
- [x] core (`go test -race ./...` green)
- [x] plugin (`go test -race ./...` green)
- [x] `claude plugin validate` on the marketplace and the plugin directory: passed
- [x] e2e (Linux container, case 7d added): E2E PASS; manual Mac check of `install-hook --with-claude-plugin` and the
  tools in a real Claude Code session pending, user
