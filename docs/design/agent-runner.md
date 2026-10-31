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
  tests), runner client, agent slot, disk queue, allow-list proxy, git credential helper, `run_token`.
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
| `session.start` | `{prompt, name?, from?, mode}` | `{id}`: runner picks the session id up front (uuid, `sessionId` option) |
| `session.send` | `{id, text, from?, steer?}` | `{queued:true}`: into the live query's input queue; `steer:true` = `priority:"now"`; idle session -> `resume` |
| `session.rename` | `{id, name}` | `{ok}` (`renameSession`) |
| `session.fork` | `{id, name?}` | `{id}` (`forkSession`, then idle until sent to) |
| `session.interrupt` | `{id}` | `{ok}` |
| `session.mode` | `{id, mode}` | `{ok}` (`setPermissionMode`; Home only forwards what the plugin approved) |
| `session.close` | `{id}` | `{ok}` |
| `usage.probe` | `{}` | `{logged_in, account?, plan?, limits?}` (zero-cost probe, no tokens spent) |

Runner -> Home (requests the runner waits on):

| Method | Params | Result |
|---|---|---|
| `perm.ask` | `{ask_id, session_id, agent_id?, agent_type?, tool, input, cwd, mode}` | `{behavior:"allow"\|"deny", message?, updated_input?}` |

`perm.ask` is sent from the `PreToolUse` hook for **every** tool call in every mode (the hook is installed for
the main session, subagents and forks; M0 showed deny works in bypass and inside subagents/forks). The hook's
own timeout is 300 s; no answer -> the SDK blocks the call (M0 exp 5). The runner also installs `canUseTool`
(for `AskUserQuestion` and modes stricter than bypass) and routes it through `perm.ask` with `tool` set.

Runner -> Home (notifications, no reply): `event {session_id, kind, ...}` with `kind` one of
`assistant` (text), `tool` (name + short input summary), `result` (`{usage, model_usage, total_cost_usd,
num_turns, is_error}`), `ask` (`AskUserQuestion` payload), `rate_limit` (from `rate_limit_event` or
`api_error_params.rate_limit_info`), `background` (a subagent kept running after the turn: the slot stays busy
until its own result), `status` (`idle`/`busy`/`closed`), `error`.

Runner options for every query: `cwd:"/work"`, `settingSources:["project"]`, `allowDangerouslySkipPermissions:
true`, `permissionMode` from Home, `env` with `CLAUDE_CONFIG_DIR=/work/.rubi/claude`, `DISABLE_TELEMETRY=1`,
`DISABLE_ERROR_REPORTING=1`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, `HTTPS_PROXY` = Home's proxy;
`disallowedTools` for edits under `/work/.rubi/**`; one long-lived query per session fed by a queue (pattern
from T3 Code, MIT, noted in the spike).

## Rubi Home ops (relay, capability `claude-agent`)

| Op | Params | Result |
|---|---|---|
| `claude-agent.projects` | `{}` | `{projects:[{id, dir, vm_state, slot_busy, queue}]}` |
| `claude-agent.sessions` | `{project}` | runner `session.list` + names, sisters' markers |
| `claude-agent.messages` | `{project, id, limit?}` | runner `session.messages` |
| `claude-agent.start` | `{project, prompt, name?, from?, mode}` | `{id}` or `{queued:true, position}` when the slot is busy |
| `claude-agent.send` | `{project, id, text, from?, steer?}` | `{queued:true, position, delivered?}` |
| `claude-agent.rename` / `.fork` / `.interrupt` / `.close` | `{project, id, ...}` | as runner |
| `claude-agent.mode` | `{project, id, mode}` | `{ok}` (plugin decides; Home forwards) |
| `claude-agent.next` | `{wait_seconds}` | `{items:[...]}`: `perm.ask` (with Home's `ask_id`), `event`s, queue changes |
| `claude-agent.decide` | `{ask_id, behavior, message?, updated_input?}` | `{ok}` |
| `claude-agent.status` | `{}` | `{login, slot:{busy, project, session}, queue:[...], usage_today}` |

Rules in Home: exactly one top-level query on the Mac at a time (the slot); subagents/forks inside it are
unlimited; background subagents keep the slot busy. Starts/sends for a busy slot wait in a disk queue
(`<helper dir>/agent-queue.json`, 0600, survives restarts, TTL 24 h). A `perm.ask` with no decision in 280 s,
or while Rubi is locked (no poll for 15 s), is answered `deny`. Home never decides a `perm.ask` itself.

## Network and git

- Allow-list proxy (CONNECT only, no TLS decryption) on a Home-owned port reachable only from the VM:
  Anthropic hosts from M0 (`api.anthropic.com`; login/refresh hosts to confirm with a real login:
  `claude.ai`, `console.anthropic.com`, `platform.claude.com` are the candidates), the project's git host(s),
  `registry.npmjs.org`, `pypi.org`, `files.pythonhosted.org`, SwiftPM package hosts from `Package.resolved`.
  Everything else: refused and logged. Telemetry hosts (Datadog, downloads.claude.ai) are refused.
- Git credential helper (`git-credential-rubi`, over the exec channel to Home): `get` for read always; for a push
  only while a matching mandate (`md_` with push scope) is live in the plugin (Home asks the plugin via
  `claude-agent.next` item `cred.ask`, plugin answers through `claude-agent.decide`).

## Plugin v0.4 (permissions)

`perm.ask` decision order: always-ask list (push, remote ops, `--force`, sudo, network outside allow-list,
paths outside `/work`, edits of `.rubi`/`.claude`, tags/releases) -> passkey approval; else in modes stricter
than bypass the existing `pt_`/`md_`/`classify.go` rules; else in bypass -> allow. Mode changes: to a stricter
mode no approval, to a looser one strong (passkey). Sisters: bind/send/replies as v0.3, delivery via
`claude-agent.send`; `rubi_reply`/`rubi_ask` become runner-side in-process MCP tools that emit events.
