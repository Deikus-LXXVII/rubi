# Claude through the Agent SDK: runner, Rubi Home and plugin contract

Status: working contract for milestones M1-M6. Background and reasons: the Agent SDK design notes (v2) and
the M0 spike (`spike/agent-sdk/README.md` on the M0 spike branch).
Everything here runs only on the user's Mac; Rubi core never starts a runner and never talks to Anthropic.

## Pieces

```
plugin claude-code v0.4 (in Rubi)  --Home ops (relay, E2E)-->  Rubi Home (Mac, user)  --stdio JSON-RPC-->  runner (TS, Agent SDK)
  approvals, pt_/md_, sisters,                                  supervisor (VM iface),                     one query per session,
  always-ask, modes, panel pages                                slot, disk queue, proxy,                   PreToolUse -> perm.ask
                                                                git credential helper
```

- **runner/** (TypeScript, `@anthropic-ai/claude-agent-sdk`): one process per project VM. Talks newline-delimited
  JSON-RPC 2.0 on stdin/stdout. No network of its own beyond the proxy given in its env.
- **internal/home/agent/** (Go): supervisor behind `VM` interface (`AppleContainer` real impl, `Fake` impl for
  tests), runner client, session slots (per project and total), disk queue, allow-list proxy, git credential helper, `run_token`.
- **plugin claude-code v0.4**: replaces `claude-hook` approvals with `perm.ask`; sisters use the new Home ops.

## Runner protocol (stdio, JSON-RPC 2.0, one JSON object per line)

Start: Rubi Home starts the runner with `RUBI_RUN_TOKEN=<random 32 bytes hex>` in its env (inside the VM:
passed by the supervisor over the exec channel, never on argv). The runner's first line must be a request
`hello {run_token, version}`; Home answers `{ok:true}` only if the token matches (constant-time compare), else
closes stdin; the runner exits if `hello` fails or no answer arrives in 10 s. A runner started without
`RUBI_RUN_TOKEN` exits at once.

Home -> runner (requests):

| Method | Params | Result |
|---|---|---|
| `session.list` | `{}` | `{sessions:[{id, name, created, last_activity, mode, busy}]}` (SDK `listSessions`, cwd `/work`) |
| `session.messages` | `{id, limit?}` | `{messages:[...]}` (SDK `getSessionMessages`) |
| `session.start` | `{prompt, name?, from?, mode, max_subagents?}` | `{id}`: runner picks the session id up front (uuid, `sessionId` option); `name` becomes the SDK `title` (no automatic title request) |
| `session.send` | `{id, text, from?, steer?, max_subagents?}` | `{queued:true, delivered}`: into the live query's input queue; `steer:true` = `priority:"now"`, `delivered:true`; without `steer` while a turn runs the runner holds the message and hands it to the SDK at the turn's `result` (`delivered:false`; the SDK never answers a mid-turn message of default priority); a session the runner does not hold -> `resume` |
| `session.rename` | `{id, name}` | `{ok}` (`renameSession`) |
| `session.fork` | `{id, name?}` | `{id}` (`forkSession`, then idle until sent to) |
| `session.interrupt` | `{id}` | `{ok}` |
| `session.mode` | `{id, mode}` | `{ok}` (`setPermissionMode`; Home only forwards what the plugin approved) |
| `session.close` | `{id}` | `{ok}` |
| `usage.probe` | `{}` | `{logged_in, account?, plan?, limits?}` (zero-cost probe, no tokens spent) |

Runner -> Home (requests the runner waits on):

| Method | Params | Result |
|---|---|---|
| `perm.ask` | `{ask_id, session_id, agent_id?, agent_type?, tool, tool_use_id?, via, input, cwd, mode}` | `{behavior:"allow"\|"deny", message?, updated_input?}` |
| `git.credential` | `{op:"get"\|"store"\|"erase", input, purpose:"read"\|"push"}` | `{output}` (git's text; empty for store/erase) |

`perm.ask` is sent from the `PreToolUse` hook for **every** tool call in every mode (the hook is installed for
the main session, subagents and forks; M0 showed deny works in bypass and inside subagents/forks). The hook's
own timeout is 300 s; no answer -> the SDK blocks the call (M0 exp 5). The runner also installs `canUseTool`
(for `AskUserQuestion` and modes stricter than bypass) and routes it through `perm.ask` with `tool` set.

Additions in M1: `via` is `"hook"` or `"can_use_tool"`; `tool_use_id` correlates the asks of one call. If the hook
already got a decision for a `tool_use_id`, `canUseTool` for the same call returns that decision without a second
`perm.ask`. `AskUserQuestion` is not asked by the hook, only by `canUseTool` (the real SDK calls it even in bypass),
after an `ask` event with the questions; `updated_input` carries the answers back. Anything but an explicit `allow`
is a deny. The runner stops waiting after 295 s (Home denies at 280 s) and denies. A `message` is shown to the model.

Runner -> Home (notifications, no reply): `event {session_id, kind, ...}` with `kind` one of
`assistant` (text), `tool` (name + short input summary), `result` (`{usage, model_usage, total_cost_usd,
num_turns, is_error}`), `ask` (`AskUserQuestion` payload), `rate_limit` (from `rate_limit_event` or
`api_error_params.rate_limit_info`), `background`, `status`, `error`. Field names (M1): `status` is
`{status:"busy"|"idle"|"closed"}` (busy while a turn runs or a background subagent lives; `closed` once, after
`session.close`); `background` is `{agent_id, subagent_type?, description?}` when a subagent keeps running after the
turn (its slot stays busy) and `{agent_id, done:true}` when it ends; `error` is `{message}` (also sent when held-back
messages or events had to be dropped, with `dropped:n` for events). `session_id` is always the id `session.start` returned.

Runner -> Home notification, not an event: `perm.cancel {ask_id, session_id}`, sent when the runner gives up on a
`perm.ask` (its own timeout, the SDK hook timeout, `session.interrupt`, `session.close`); Home drops the pending
item. The call is denied on the runner side. Not sent when Home is gone.

Git credentials: the runner listens on a unix socket in the VM (`RUBI_CRED_SOCK`, default `/tmp/rubi-cred.sock`,
mode 0600, removed on exit). `git-credential-rubi` (runner/container) writes the operation on the first line, then git's
`key=value` text up to the blank line (at most 8 KiB); the runner relays it as `git.credential` and writes `output`
back, then closes. On an error, a refusal or no answer within 295 s it closes with no output (git fails without
prompting). `purpose` is `"push"` only for 120 s after Home approved a `perm.ask` for a Bash command that contains
`git ... push`; otherwise `"read"`. Any process of the runner's VM user can connect to the socket; Home must treat the
purpose as the runner's claim and keep separate read and push credentials. The runner neither stores nor logs them.

Runner process: environment `RUBI_RUN_TOKEN` (required), `ANTHROPIC_BASE_URL` (the address of this start's auth gateway on the Mac, which holds the Claude token), `CLAUDE_CODE_OAUTH_TOKEN` (a per-start secret good only at that gateway; the VM never holds the real token, see home-setup-wizard.md, "Shared sign-in"), `NO_PROXY` (the gateway's host), `RUBI_WORK_DIR` (working directory, default `/work`;
sessions and config under `<dir>/.rubi/claude`), `HTTPS_PROXY`. The runner sets `CLAUDE_CONFIG_DIR` for itself too,
because `listSessions`/`forkSession` read the sessions of its own environment. `from` in `session.start`/`send` is
written as a first line `[message from <from>]` in the text. A session the runner does not hold (after a restart or a
close) is resumed in mode `default` until `session.mode` is sent; a fork inherits the mode of its source. Wire
transcripts for client tests: `runner/testdata/*.ndjson`, format in `runner/README.md`.

Runner options for every query: `cwd` (`/work`), `permissionMode` from Home, `allowDangerouslySkipPermissions:true`,
`settingSources:["project"]` (CLAUDE.md and skills; `RUBI_SETTING_SOURCES=none` ignores the project's `.claude` entirely),
and, so that a project cannot run code outside the `PreToolUse` gate (checked against the real SDK in M1):
`settings:{disableAllHooks:true, apiKeyHelper:""}` (flag-layer settings beat the project's files: no hooks from
`.claude/settings.json`, no project `apiKeyHelper`), `strictMcpConfig:true` with `mcpServers:{}` (no `.mcp.json` server),
and `disallowedTools` `Edit(/work/.rubi/**)`, `Edit(/work/.claude/**)`, `Edit(/work/.mcp.json)`, `Read(/work/.rubi/**)`
(`Edit(...)` covers Write and the other editing tools; a `Write(...)` rule does not block; deny rules run before hooks and a
hook's allow cannot override them; Bash can still write there, that goes through `perm.ask`). Known gap: `env` in the
project's `.claude/settings.json` still applies (it cannot be switched off key-wide); use `RUBI_SETTING_SOURCES=none` for
untrusted repositories. The hook never returns a permission `allow`: it returns a deny, or `updatedInput` without a decision
(the SDK applies it), or `{}`; the SDK's own rules and `canUseTool` still run afterwards. Exceptions in the hook or in
`canUseTool` deny. Child `env`: `CLAUDE_CONFIG_DIR=/work/.rubi/claude`, `DISABLE_TELEMETRY=1`, `DISABLE_ERROR_REPORTING=1`,
`CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, `HTTPS_PROXY`/`https_proxy` copied from the runner's env (Home's CONNECT
proxy; no `HTTP_PROXY` is set), plus `PATH HOME USER LOGNAME SHELL TMPDIR LANG LC_ALL TERM NO_PROXY` and, for tests and API-key
setups only, `ANTHROPIC_BASE_URL` and `ANTHROPIC_API_KEY` if present in the runner's env; nothing else. One long-lived
query per session fed by a queue (pattern from T3 Code, MIT, noted in the spike).

## Role mode (agent roles, R1)

Set `RUBI_ROLE_DIR` (e.g. `/rubi-role`, a read-only mount written by Rubi Home) to switch the runner into role mode;
unset (or empty) is the legacy mode described above, unchanged. Design and reasons: `agent-roles.md` (sections 1 and 4).
`RUBI_CLAUDE_DIR` (default `/rubi-claude`) is then the config dir (`CLAUDE_CONFIG_DIR`, for the runner process and its
children): sessions, `session.list`, `.messages`, `.fork` and `.rename` use it, nothing is written under `/work/.rubi`.

At the start of every query (`session.start`, a resume after `session.send` to a session the runner does not hold, a
restart of a dead query) the runner resolves `<RUBI_ROLE_DIR>/current` with `realpath`, requires the result to be a
directory strictly inside `RUBI_ROLE_DIR` (a `current` that points out via `../`, an absolute path or at the role dir
itself is refused), and pins that version for the query. A new version applies at the next query start; a running query
keeps its version. A missing or bad role refuses the call with error `-32603` ("role unusable, session not started: ...");
the runner never falls back to legacy mode and registers no session.

`<ver>/role.json` (written by Home; at most 64 KiB; `lstat`: a symlink or a non-regular file is refused; opened with
`O_NOFOLLOW`; unknown keys ignored):

| Key | Type | Rule |
|---|---|---|
| `role` | string | required, non-empty (the role id; logged) |
| `version` | integer | required, >= 1 |
| `tools` | string[] | required (may be empty); every name in the runner's known list (`Agent AskUserQuestion Bash CronCreate CronDelete CronList Edit EnterPlanMode EnterWorktree ExitPlanMode ExitWorktree Glob Grep ListAgents Monitor NotebookEdit Read ReportFindings ScheduleWakeup SendMessage Skill TaskStop TodoWrite ToolSearch WebFetch WebSearch Workflow Write`); the safe subset is Home's and the plugin's policy, not the runner's |
| `skills` | string[] | default `[]`; names match `^[a-z0-9][a-z0-9-]{0,47}$`, at most 100 |
| `deny` | string[] | default `[]`; permission rules appended to `disallowedTools`; non-empty, at most 256 characters, no control characters, at most 200 |
| `mcp` | object | default `{}`; at most 32 servers named `[A-Za-z0-9_-]{1,48}`; `{type:"http"\|"sse", url (http/https), headers?}` or `{type:"stdio" (default), command, args?, env?}`; other keys of a server are dropped |
| `instructions` | string | default `""`, at most 8 KiB |
| `project_instructions` | boolean | default `false` |
| `default_mode` | string | default `"default"`; one of the permission modes |

`<ver>/plugin/` must be a plain directory (not a symlink): an SDK local plugin named `role`
(`.claude-plugin/plugin.json`, `skills/<name>/SKILL.md`, `agents/<name>.md`).

Query options in role mode: `settingSources:[]` (the repository's `.claude` is never loaded; its `env` no longer applies;
`RUBI_SETTING_SOURCES` is ignored), `plugins:[{type:"local", path:"<ver>/plugin", skipMcpDiscovery:true}]`,
`skills:["role:<name>",...]` (`[]` when none), `tools:` the role's list without `Skill` and the plan tools, plus `Skill`
only when the role has skills, `AskUserQuestion` always, `EnterPlanMode`/`ExitPlanMode` when the session's mode is `plan`
at query start; `disallowedTools`: the usual four rules, `Edit(<claude dir>/**)`, `Read(<claude dir>/**)`, then the
role's `deny`; `strictMcpConfig:true` with `mcpServers` from `role.mcp`; `systemPrompt:{type:"preset",
preset:"claude_code", append: <instructions block>}`. The instructions block is the role's `instructions`, then, when
`project_instructions` is true, `/work/CLAUDE.md` and `/work/.claude/CLAUDE.md` under a heading that says the text was
written in the repository (each read with `lstat`, a symlink or a non-regular file is left out, 64 KiB together; `@import`
lines and CLAUDE.md files in subdirectories are not followed). Unchanged: `settings:{disableAllHooks:true, apiKeyHelper:"",
skipWebFetchPreflight:true}`, the `PreToolUse` gate and subagent gate, `canUseTool`, redaction, the child env
(`ANTHROPIC_BASE_URL`, the `CLAUDE_CODE_OAUTH_TOKEN` placeholder, `NO_PROXY`, the proxy).

Modes: the session mode Home passes in `session.start` always wins. In role mode `mode` is optional there; when Home gives
none, the role's `default_mode` is used (also for a session the runner resumes without a mode). A mode set later with
`session.mode` is not reflected in `tools`: the plan tools are decided when the query opens.

Checked against the real SDK and `claude` by `runner/test/roles.integration.test.ts` (`npm run test:integration`).

Behaviour of held-back messages: at most 50 per session (more: error `-32006` and an `error` event). If the query
dies, the first held-back message goes into a resumed query (at most 3 restarts without a `result`), the rest follow
turn by turn. A turn the SDK started itself (a background task woke the model) counts as active, so a message sent then
is held back too. `session.interrupt` drops held-back messages. Session ids in params must be UUIDs. Slow reader: when
Home reads slower than the runner writes and more than 8 MiB wait, the oldest `assistant` and `tool` events are dropped
and one `error` event with `dropped:n` follows; requests, responses and other events are never dropped.

The run token protects the handshake against a wrong peer, not against the VM's own user: any process of that user can
read it from the runner's environment, and then could only talk to the runner it already shares a VM with. It is not a
secret from the VM. Home's proxy and the exec channel, not the token, keep other VMs and hosts out.

Build: `npm run build` writes `runner/dist/main.js` (the image's entry point, Node >= 22; an older Node, or a missing
`node_modules`, ends with a plain message and exit 1). Tests compile to `dist-test/`.

## Network and git

- Allow-list proxy (CONNECT only, no TLS decryption) on a Home-owned port reachable only from the VM:
  Anthropic hosts from M0 (`api.anthropic.com`; login/refresh hosts to confirm with a real login:
  `claude.ai`, `console.anthropic.com`, `platform.claude.com` are the candidates), the project's git host(s),
  `registry.npmjs.org`, `pypi.org`, `files.pythonhosted.org`, SwiftPM package hosts from `Package.resolved`.
  Everything else: refused and logged. Telemetry hosts (Datadog, downloads.claude.ai) are refused.
- Git credential helper (`git-credential-rubi` in the VM, through the runner's credential socket and the `git.credential`
  request, see above): `get` for read always; for a push
  only while a matching mandate (`md_` with push scope) is live in the plugin (Home asks the plugin via
  `claude-agent.next` item `cred.ask`, plugin answers through `claude-agent.decide`).

## Plugin v0.4 (permissions)

`perm.ask` decision order: always-ask list (push, remote ops, `--force`, sudo, network outside allow-list,
paths outside `/work`, edits of `.rubi`/`.claude`, tags/releases) -> passkey approval; else in modes stricter
than bypass the existing `pt_`/`md_`/`classify.go` rules; else in bypass -> allow. Mode changes: to a stricter
mode no approval, to a looser one strong (passkey). Sisters: bind/send/replies as v0.3, delivery via
`claude-agent.send`; `rubi_reply`/`rubi_ask` become runner-side in-process MCP tools that emit events.

## Home-side additions (M2)

Runner -> Home request `git.credential {op: get|store|erase, input, purpose?: "push"}` -> `{output}` (the runner relays
the in-VM `git-credential-rubi`). Home -> plugin `next` items besides `perm.ask`: `cred.ask {host, path, op:"push"}` (a push
credential), `net.ask {host}` (a host not on the proxy's list), `queue {action, queue_id}`; all answered with `decide`.
Subagent cap. `max_subagents` (an integer, 0 or absent = no cap) is the most subagents one session may run at once
(Home sends the cap of the Bot that owns the session). The runner counts a session's running subagents as its
background tasks plus its `Agent`/`Task` calls that passed the gate and have not ended; a subagent started by a call
(`task_started.tool_use_id`) is counted once, whether or not the SDK says it runs in the background. A change is
reported with an event `subagents {count}`. The gate is its own `PreToolUse` hook, matcher `^(Agent|Task)$`, next to
the hook that asks Home about every tool (no matcher, `hookTimeoutSec` = 300 s, unchanged: it asks about the subagent
tools too, so a matcher that failed to match would lose the cap, never an ask; the integration test checks that every
tool is still asked about). The gate hook **waits** (never denies) while the count is at the cap: it sends
`waiting {reason, tool_use_id}` ("waiting: this session already has N subagents running, the limit"), repeats it every
`waitKeepaliveMs` (5 min, with `still: true`; Home reads any event as a sign of life, so its 30-minute idle watchdog
doesn't interrupt a session that waits on purpose), and when the call goes on sends `waiting {reason:"", done:true,
tool_use_id}`. The gate has the long SDK timeout: `hookTimeoutSec` plus the longest wait, `subagentWaitSec` (default
6 h). After the longest wait the call is denied with a reason the model sees (and an `error` event), never silently.
A call is also denied ("Stopped while waiting...") when the session is interrupted or closed while it waits, and a wait
stops when the call is denied by Home or ends some other way. The decision Home gave for such a call is remembered for
as long as the call may wait, so the user is not asked twice.

Places are released by the subagent's own end, not only by the tool result of its call: `task_notification` (which
carries the call's `tool_use_id`) ends the call and the task. **Checked against the real SDK** (integration test,
scripted API): the hooks of parallel `Agent` calls run together, the results of the calls stream one by one, a
background subagent's call returns at once ("Async agent launched") while its task goes on, and a subagent can start
subagents of its own (`spawn_depth` 2). **Rule for nested subagents:** a call made by a subagent is not held back by the
places that subagent and its parents hold (it would wait for itself for ever), so a tree can run deeper than the cap by
the depth of the chain; calls of other branches count in full. The count is approximate for a moment around a launch (the
tool result and the task start are two messages). The cap is read at `session.start`; a `session.send` to a session with
no turn, task or call running takes the cap it brings, a running one keeps its own. `session.fork` copies the cap of the
source. Lowering a mode kills nothing: running subagents finish, new ones wait.
Slot events (each session holds one slot; limits in `rubi-home.md`, "Slots"): `status {state}`, `background {tasks:[{task_id}]}` (the whole set), as the M1 runner sends them. See
`docs/design/rubi-home.md` ("Agents in a VM").
