# Rubi agent runner

TypeScript (ESM, strict) process that sits between Rubi Home and the Claude Agent SDK. It speaks
line-delimited JSON-RPC 2.0 on stdin/stdout and runs one long-lived SDK query per session.
The contract (methods, params, events, options) is `docs/design/agent-runner.md`; this file only
covers what is specific to the code and the test fixtures.

## Layout

| File | What |
|---|---|
| `src/protocol.ts` | JSON-RPC peer: ids, notifications, per-line size limit, malformed-line handling |
| `src/runner.ts` | handshake, session registry, queue-fed queries, `PreToolUse` hook + `canUseTool` -> `perm.ask`, events |
| `src/sdk.ts` | the small interface over the SDK that the runner uses (tests inject a fake) |
| `src/sdk-real.ts` | the real SDK behind that interface, including the zero-cost `usage.probe` |
| `src/main.ts`, `src/start.ts` | entry point with a Node/SDK preflight, then the wiring (stdout is the protocol; logs go to stderr) |
| `src/cred.ts` | the git credential unix socket |
| `test/` | unit tests against a fake SDK (`fake-sdk.ts`, `harness.ts`) and an opt-in integration test |
| `testdata/*.ndjson` | wire transcripts for the Go client (below) |

## Build and test

```
npm ci
npm run build               # runner/dist/main.js, the image's entry point
npm test                    # tsc, then node --test against the fake SDK (no network, no claude)
npm run test:integration    # real SDK + real bundled claude against a scripted local API (opt-in)
```

The integration test sets `RUBI_RUNNER_INTEGRATION=1` itself (via the script). It points the child at a
scripted Messages API on 127.0.0.1 through `ANTHROPIC_BASE_URL` with a placeholder key, uses a throwaway
`HOME` and working directory, spends no tokens and needs no login.

## Environment of the runner

| Variable | Meaning |
|---|---|
| `RUBI_RUN_TOKEN` | required; the runner exits at once (code 2) without it. Read once, then removed from the environment, never passed to `claude` |
| `RUBI_WORK_DIR` | working directory of every session, default `/work`. Sessions and config live in `$RUBI_WORK_DIR/.rubi/claude` (`CLAUDE_CONFIG_DIR`) |
| `HTTPS_PROXY` | the allow-list proxy; passed on to `claude` |
| `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY`, `NO_PROXY`, `PATH`, `HOME`, ... | passed through to `claude` (small explicit allow-list; nothing else from the runner's environment) |

Role mode (docs/design/agent-roles.md, docs/design/agent-runner.md "Role mode"): `RUBI_ROLE_DIR` (read-only role directory; unset = legacy mode) and `RUBI_CLAUDE_DIR` (the role's config dir, default `/rubi-claude`).

Also: `RUBI_CRED_SOCK` (git credential socket, default `/tmp/rubi-cred.sock`), `RUBI_SETTING_SOURCES=none` (ignore the project's `.claude`).

Exit codes: 0 stdin closed after a good handshake; 2 no token; 3 hello refused, unanswered in 10 s, or stdin closed before it.

## Behaviour worth knowing (decided in M1)

- A `session.send` while a turn is running is held in the runner and handed to the SDK when the turn's
  `result` arrives (the SDK never answers a mid-turn message of default priority). The reply is
  `{queued:true, delivered:false}`. With `steer:true` it goes in at once with priority `now` (`delivered:true`).
- `session.send` for an id the runner does not hold opens a query with `resume`. The mode of such a session is
  `default` until `session.mode` says otherwise; a fork inherits the mode of its source.
- `from` is written into the message as a first line `[message from <from>]`.
- `perm.ask` has two extra fields beyond the contract: `tool_use_id` and `via` (`"hook"` or `"can_use_tool"`).
  In modes stricter than bypass the CLI may also consult `canUseTool` for a call the hook already decided; the runner
  then returns the hook's decision for that `tool_use_id` without a second `perm.ask`.
  `AskUserQuestion` is asked only via `canUseTool` (the real SDK does call it in bypass mode, checked by the integration test),
  after an `ask` event with the questions.
- Anything but an explicit `allow` is a deny. The runner stops waiting after 295 s (Home itself denies after 280 s;
  the SDK blocks the call after the 300 s hook timeout). When the runner gives up on an ask (its own timeout, the SDK
  hook timeout aborting the hook, `session.interrupt`, `session.close`) it sends the notification
  `perm.cancel {ask_id, session_id}` so Home can drop the pending item, and the call is denied. Nothing is sent
  if Home itself went away.
- `status` is `busy` while a turn runs or background tasks (`background_tasks_changed`, `task_started`/`task_notification`)
  are alive; `background` events list those tasks. `closed` is sent once by `session.close`.
- Security hardening (details in the contract): project hooks, `apiKeyHelper` and `.mcp.json` servers are switched off, `.rubi`,
  `.claude` and `.mcp.json` are not editable and `.rubi` not readable by tools, the hook never returns `allow`, and any exception
  in a permission callback denies. In `dontAsk` mode the SDK itself denies `AskUserQuestion`; nothing reaches Home.
- Git credentials: unix socket `RUBI_CRED_SOCK` (0600) relayed as `git.credential`; `purpose` is `push` only for 120 s after an
  approved `git ... push` command.
- Session names: `session.start` with `name` passes it as the SDK `title` (no automatic title request for it).
- The runner process sets `CLAUDE_CONFIG_DIR` for itself too: `listSessions`/`forkSession` read the sessions of the
  runner's own environment, not the child's.

## Wire transcripts for the Go client

`testdata/*.ndjson` are recorded from the runner talking to a scripted Home (the unit tests regenerate and compare
them: `UPDATE_FIXTURES=1 npm test` rewrites them, otherwise a difference fails the test). One JSON object per line:

```
{"from":"runner"|"home","msg":{...a JSON-RPC message...}}      a message on the wire
{"from":"home","raw":"this is not json"}                      a raw line sent as is (malformed input)
{"from":"home","action":"close_stdin"}                        Home closed the runner's stdin
{"from":"runner","action":"exit:3"}                           the runner exited with that code
```

- Lines are in the order they happened. `from` is the sender. Request ids are per direction: the runner's
  `hello` is id 1 and Home's first request is also id 1; match a response to a request of the other side.
- Ids the runner generates (session ids, `ask_id`) are written `<uuid-1>`, `<uuid-2>`, ... in order of first appearance.
  A replaying client treats them as opaque values: take the real value from the first line where it appears and
  substitute it for the placeholder when comparing or when sending. The `hello` token in the files is a dummy.
- To replay: feed every `"from":"runner"` line to the client under test (or run a fake runner that prints them
  when the preceding `"home"` lines have been received) and compare what the client writes with the `"from":"home"` lines.
- Do not depend on the order of an `event` notification relative to a nearby `perm.ask` or response: they come from
  different tasks inside the runner. The files show one order that happened to be stable.
- Files: `handshake-ok`, `handshake-refused`, `malformed-lines`, `start-perm-allow-deny-subagent`,
  `queue-and-steer`, `ask-user-question`, `background-subagent`, `git-credential`.

## Attribution

Patterns and the usage probe are adapted from T3 Code (MIT, Copyright (c) 2026 T3 Tools Inc.); see `LICENSE-T3`.
`test/support/mock.mjs` is the scripted API from the M0 spike.
