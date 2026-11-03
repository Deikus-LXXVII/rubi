# Grok Bot × T3 Code integration (plan)

Status: plan approved on 2026-11-03; stage S0 (spikes) done, results in section 9. Decisions on the open
questions are in section 8.

Sources:
- t3code `main` at a036489 (2026-11-02), cited by file below.
- The latest nightly: `v0.1.7-nightly.20261102.4093` at 84537c4.
- Anthropic's terms: code.claude.com/docs/en/legal-and-compliance.

## 0. Facts the plan rests on (checked in the source)

- **License.** MIT, "Copyright (c) 2026 T3 Tools Inc." (`LICENSE`). If we ship or install T3, the notice goes with it.
- **Nightly builds.**
  - Built by `.github/workflows/release.yml`. The schedule runs every 30 minutes but releases at most every 6 hours,
    and only when something changed (`.github/scripts/check-nightly-release.cjs`).
  - Tags look like `vX.Y.Z-nightly.YYYYMMDD.<run>`. They are GitHub prereleases and the npm package `t3` with dist-tag
    `nightly`. Stable releases (`v*.*.*`) re-ship a commit a nightly already built.
  - There were 118 nightlies and 7 stables in the last 30 days.
- **Process model.**
  - The server (`t3`, TypeScript on Effect, Node 26) runs on the user's Mac, port 3773, with state in `~/.t3/userdata`.
  - Web, desktop and mobile clients use WebSocket RPC `/ws`, with pairing and scoped sessions.
- **Providers.**
  - Each provider is a driver that returns a `ProviderAdapterV2`: open session, start/steer/interrupt a turn,
    answer approvals, rollback, fork, and an event stream.
  - Drivers are a hardcoded list (`apps/server/src/provider/builtInDrivers.ts:33-45`). There is no plugin directory.
  - Multiple instances per provider are supported, each with its own `binaryPath`, `CLAUDE_CONFIG_DIR` and env.
- **Claude.**
  - T3 spawns the user's unmodified `claude` through the Agent SDK, with `pathToClaudeCodeExecutable = binaryPath`,
    `cwd` set to the thread workspace, and the host env plus the instance env.
  - Sign-in is the user's own `claude auth login`. Credentials stay in Claude Code's own store.
  - T3 sets no `ANTHROPIC_*` variables by default.
- **Outside agents** (`docs/user/outside-agents.md`).
  - An MCP server is served at `<env>/mcp`, signed in with OAuth + PKCE and approved with a pairing code.
  - Scopes: read-only (the default), supervised / approval-required, auto-accept-edits, auto, full-access. A thread an
    outside agent starts can't run with more permissions than its scope.
  - Tools include `t3_thread_launch`, `_send`, `_wait`, `_read`, `t3_project_*`, `delegate_task` and `schedule_task`.
- **Gaps.**
  - Approvals of tool calls can't be answered over MCP. That needs the WebSocket `runtime-request.respond` with a
    broader `orchestration:operate` token.
  - Events are pushed over WebSocket only; MCP can only block (`t3_thread_wait`) or poll.
  - New threads default to Full access (`bypassPermissions`).
  - T3 has no sandbox of its own.
- **Telemetry.** On by default: hashed ids and metadata go to PostHog via ingest.t3.codes, and traces to Axiom.
  `T3CODE_TELEMETRY_ENABLED=false` or the managed policy `telemetry: false` turns it off.
- **Upstream policy.** The project is "very very early" and "(mostly) not accepting contributions"; features need
  prior maintainer approval. So any patch we carry stays ours for good.

## 1. Architecture (zero upstream patches)

```
Grok Bot ──MCP──> Rubi core (VPS) ──relay──> Rubi Home (Mac) ──OAuth MCP (localhost)──> T3 server (Mac, :3773)
   ^  events                  plugin "rubi-t3agents"      t3bridge (Go)                     │ Claude/Codex instances
   └──────────────── Rubi events <── thread_wait/read ────┘                                 │ binaryPath = rubi-agent-vm
                                                                                            v
                                                       Apple container VM per role (cwd mounted at the same path,
                                                       role config dir, egress only via Rubi Home's allow-list proxy,
                                                       hooks -> Rubi approvals, ssh -> Rubi Home)
```

The upstream T3 server stays unmodified, pinned to one nightly. Everything Rubi adds lives outside it, in three
pieces:

1. **`t3bridge` in Rubi Home** (Go, new package `internal/home/t3`).
   - It is an OAuth MCP client of T3's `/mcp` on `localhost`. The user approves its sign-in once with a pairing code,
     and the scope they choose there is the ceiling. The default is Full access: Rubi's role modes, approvals, the
     container and the egress allow-list still apply on top.
   - It offers Rubi core the ops `t3.projects`, `t3.thread.launch/send/read/interrupt` and `t3.thread.wait` (a
     long-poll per active thread).
   - It turns results into Rubi events: turn finished, a question asked, a thread waiting for an approval. Bots are
     woken by events and never poke a session.
   - It never takes the broader WebSocket operate token.
2. **`rubi-agent-vm`, the wrapper set as a T3 instance's `binaryPath`.**
   - It is a small binary from Rubi Home, called with the arguments T3 passes to claude.
   - It runs the unmodified `claude` in the role's Apple container VM: `cwd` is mounted at the same absolute path, the
     role's `CLAUDE_CONFIG_DIR` is a host folder mounted read-write, the network is none plus Rubi Home's egress
     pipe, and stdio is relayed.
   - Claude's session files stay on the host, because T3 reads history and forks in a host-side worker.
   - T3's injected MCP bridge points at the host. The wrapper relays it through the runner channel, the same way
     rubi-netpipe works today.
   - Codex gets the same treatment later with its own binary.
3. **Plugin `rubi-t3agents`** (the successor of rubi-claude-code).
   - It gives Bots tools for start, send, status, sessions and bind, built on t3bridge.
   - It carries over roles, modes, mandates, project trust, the skill library, SSH and briefs (section 3).

There is one T3 instance per role, for example `claude-<role>`, each with its own `CLAUDE_CONFIG_DIR` (skills, MCP,
settings, the role's managed settings). T3 already supports multiple instances.

**Sign-in, as in T3.** The user signs in to each provider with its own flow. For Claude that is
`claude auth login`, run in the role's container. The link and code are relayed as plain terminal text, through
the Rubi panel or T3's own terminal. Rubi never stores, reads, proxies or injects a subscription token. The current
auth gateway, `rubi-claude-token` and the token handling are deleted. The proxy remains only as the egress
allow-list (api.anthropic.com, claude.ai, claude.com, platform.claude.com and the chosen hosts).

## 2. Easy upstream updates

- **Pin file** `t3/upstream.json` in our repo: the nightly tag, commit SHA, npm version and integrity, and the date
  checked. Rubi Home installs exactly that (`npm i t3@<version>` into its own prefix), or uses the user's own
  install if its version matches.
- **No fork.** The patch directory `t3/patches/` starts empty. The policy: a patch only if there is no other way,
  each one under 50 lines, each with its own test, each rebased automatically by the bump script. We do not plan
  any patch for v1 (see section 5 for the candidates we avoid).
- **Contract tests** (`t3/contract/`, run against the pinned server in a temp user-data dir with telemetry off):
  - the MCP `tools/list` snapshot (names and input schemas of the tools we use);
  - launch/send/wait/read with a fake claude;
  - the instance config schema (`binaryPath`, env);
  - the wrapper's argv and env passthrough;
  - fork and history with session files on the host.
- **`scripts/t3-bump`.** It finds the latest nightly (GitHub releases API, tag pattern above), updates the pin, runs
  the contract tests and the integration tests, and writes a diff report of tool-schema changes.
- **CI.** A scheduled workflow does the same and opens a PR. Merging stays manual; nothing auto-merges.

## 3. Concepts carried over from the Agent SDK version

| Concept | Where it lives with T3 |
|---|---|
| Bots wake sessions / give tasks (`claude_code_start/send/bind`) | Plugin tools map onto t3bridge: start = `t3_thread_launch` in a role instance; send = `t3_thread_send` (labelled as the Bot's, as today); bind = a Bot bound to a T3 thread id; replies and turn-finished arrive as events from `thread_wait` (no poking). |
| Roles/profiles (skills, MCP, tools, mode) | One T3 instance plus one VM per role. The role's config dir carries skills, MCP, tool allow-list and mode ceiling as managed settings that the user can't loosen from inside the VM. Rubi Home writes them (existing roles code). |
| Maximum/Medium/Light, session and subagent caps (computer cap 4) | Enforced in t3bridge (concurrent threads per Bot, project and role) and in the wrapper (VM count, computer cap). Subagent limits go through the role settings, as today. |
| Two-tier approvals | Tier 1 (self-service, audited, revertible) and tier 2 (strong) stay in the plugin and core. Tool-call approvals inside a thread stay **ours**: claude's PreToolUse hook in the VM asks Rubi, as the hook path does today. T3's own approval UI isn't needed for Bot-started threads. Mode ceiling = the min of the bridge scope (user-chosen) and the role ceiling. |
| Shared skill library with curator | Unchanged (core plus Rubi Home). The curator is a setting, never a name in code. |
| SSH via Rubi Home | Unchanged: an `ssh` wrapper in the VM over the runner channel; keys stay in Home. |
| Apple container sandbox + egress allow-list | The wrapper (`binaryPath`) is the containment. T3 itself runs on the host and touches only the workspace and its own data. |
| Mandates / project trust | Unchanged in the plugin. A project is a T3 project (`t3_project_*`) whose folder the user approved (claude_code_project_add carries over). |
| Briefs/reports (poteto) | Start requires a typed brief (goal, scope, acceptance, verify, timebox, forbidden, report). Standing orders are added on start and resume. Reports have a fixed shape (PASS/ISSUES/BLOCKED + commit SHA + evidence). A verifier role in its own VM is optional. |
| Session-finished events instead of pokes | t3bridge wait loop to Rubi events. |

### Requirements carried over from citri/wizard-fixes (not released; the branches are kept as reference)

- **R1 Distinct binding errors.** Every Bot tool that needs a binding names itself in the error and says which case
  applies, plus which binding is current:
  - missing bind_code
  - unknown bind_code
  - superseded (replaced on <date>)
  - unbound on <date>
  - another Bot's code
  - no binding in saved state

  Old codes are kept only as SHA-256 hashes, at most 32. Reference: plugin 18f17bd.
- **R2 Bindings survive restart and update.** A test restarts the plugin from its saved state. Before and after a
  rebind and an unbind, it checks that the session list's `bound_to` equals what send and status accept.
  Reference: `TestBindingSurvivesARestartAndTheListMatches`.
- **R3 A Bot adds a project folder only through approval.** A tool like `claude_code_project_add {dir}` raises one
  strong approval that shows the folder and the Bot. Core forwards Home's `project.add {dir}` only while the
  approval for exactly that dir is executing, and refuses folded duplicate keys. Home applies the same folder checks
  as for a chosen folder. The setup itself never asks for a folder. With T3, a project is created
  (`t3_project_*`) only after that approval. Reference: core 754248d, plugin 6fd05ee.
- **R4 Setup page.** One refresh timer, no redraw when nothing changed, and focus and errors survive a redraw
  (core 20ba8b4). The provider sign-in relay always shows the code box as soon as the link appears (plugin 687f00f).
  The relay must survive TUI drawing changes such as cursor-move gaps (core ad69e81).

## 4. Where things live; what happens to rubi-claude-code

- **T3 server** runs on the Mac as its background service (`docs/user/background-service.md`) or under Rubi Home's
  supervision, bound to localhost only. The user may also open T3's own desktop or web UI to watch threads.
- **Rubi panel** keeps approvals, roles, library, modes and setup. Setup becomes:
  1. Apple container;
  2. the runner image;
  3. install/pin T3;
  4. pair Rubi Home with T3 (the pairing code is approved in T3);
  5. sign in to each provider in a role container (plain relay);
  6. self-test.
- **Bots** still reach everything through Rubi core and the relay. T3 Connect is not needed, and T3 is not exposed
  to the internet.
- **rubi-claude-code.**
  - The Agent SDK runner path and the auth gateway are deprecated by the new plugin. The hook path for the user's
    own desktop Claude Code sessions can stay in rubi-claude-code (maintenance only) until rubi-t3agents covers it.
  - Migration: roles, mandates, trusts and the library carry over (same Home stores). Bindings to agent sessions
    end (new T3 threads), and Bots get a one-time event saying how to rebind.
  - The v0.14 rc stays as is for the current release; rubi-t3agents ships as a new plugin, with the core at v0.15.

## 5. Patch candidates we avoid (and the fallback if forced)

1. A provider-rubi driver in `builtInDrivers.ts`. Not needed, because `binaryPath` already gives containment.
2. Answering approvals over MCP. Not needed, because approvals go through our hook in the VM.
3. An outbound event webhook. Not needed, because of the long-poll.
4. Making `AgentScope.wrap` container-aware. Not needed, because of the wrapper.

If a contract test shows `binaryPath` or the instance env can't carry what we need, the fallback is a single patch
of under 50 lines, with a test, kept in `t3/patches/`.

## 6. Stages (each ends with a check that passes or fails)

| Stage | Done when |
|---|---|
| S0 spikes (≈2 days) | (a) The pinned nightly runs headless in a temp user-data dir with telemetry off. (b) A Go client completes the OAuth sign-in to localhost `/mcp` with a pairing code, then runs launch/send/wait/read on a fake-claude instance. (c) `binaryPath` = wrapper runs the real claude in Apple container with the cwd mount; history and fork work in T3. (d) `claude auth login` in the container stores credentials in the host-mounted config dir, and Rubi never reads them. (e) The PreToolUse hook in the VM reaches Rubi and a denial blocks the tool. Each spike gives yes or no, with logs. |
| S1 | The `t3bridge` ops and the contract tests are green against the pinned nightly. |
| S2 | The wrapper plus role instances work. Live: two roles have isolated skills and MCP on one repo, and the egress allow-list holds (the existing isolation probe). |
| S3 | Plugin rubi-t3agents: start/send/bind/status, events, briefs. Live: a Bot starts a thread, gets turn-finished, and sends a follow-up. |
| S4 | Approvals tiers, modes and caps, mandates, project trust, library, SSH ported. The existing tests are re-pointed and green. |
| S5 | Auth cleanup: gateway, token helper and token code deleted. The docs and ToS note are updated. A test proves no token path is left (grep plus a live check that Home never opens the credential). |
| S6 | `scripts/t3-bump` and the CI workflow. A bump to a newer nightly runs the whole suite. |
| S7 | Migration from rubi-claude-code: the deprecation notice and the Bot rebind event. |

## 7. Risks

- **Upstream churn.** There are several nightlies a day and the project calls itself very early: MCP tool names or
  schemas may change. Mitigations: the pin, the contract tests, the bump script; we never auto-update.
- **Credentials readable by agent Bash in the VM.** Same as T3. The mitigation is managed settings that deny reading
  the credential, plus Claude Code's sandbox `denyRead`. Whether bubblewrap works inside Apple container is
  untested, so it becomes spike S0(d) and an extra check.
- **Shared sign-in for all roles.** One sign-in per role is the clean model. Sharing one through a common volume is
  untested.
- **The gateway's body/beta policy (M1) goes away.** Without intercepting TLS, which would itself be intermediating,
  we can't filter Anthropic server tools. WebSearch and WebFetch move to permission rules in managed settings; egress
  stays host-allow-listed.
- **The Agent SDK note on claude.ai login** applies to T3 too. The owner's decision is to follow T3; full certainty
  would need Anthropic.
- **T3 defaults.** Full access is the default, so the bridge always launches with an explicit mode at or below the
  role ceiling, and the scope caps it anyway. Telemetry is on by default: Rubi turns it off when it manages T3.
- **Instance config.** Writing T3's instance settings may not be a public API. If it isn't, the user adds an instance
  once in T3's UI, guided by the setup page, or we use a managed-policy file if T3 supports one for instances
  (to check in S0).
- **Session files on the host.** They hold conversation history readable on the Mac, which is expected and the same
  as T3.

## 8. Decisions (2026-11-03)

1. T3 access level for Rubi Home: Full access by default. Role modes, Rubi approvals, the container and the
   egress allow-list still apply on top.
2. Telemetry: off by default whenever Rubi installs or manages T3.
3. Rubi Home's installer installs T3, pinned to `t3/upstream.json`.
4. Subscription sign-in: **shared by all roles** through `CLAUDE_SECURESTORAGE_CONFIG_DIR` (S0 result: section 9),
   with each role keeping its own `CLAUDE_CONFIG_DIR`. Conditions: a contract test that fails loudly if the
   variable or the folder layout (`.credentials.json`, `.oauth_refresh.lock`) changes, and a live token refresh
   check in S2. One sign-in per role stays a documented fallback.
5. Codex comes after v1, but the provider layer is generic from the start.
6. Plugin name: `rubi-t3agents`.
7. The hook path for desktop Claude Code sessions stays in rubi-claude-code for now.

## 9. S0 results (2026-11-03)

Setup: T3 `0.1.7-nightly.20261102.4093` from npm (`t3/upstream.json`), Node 26, a throwaway `--base-dir`, its own
port on 127.0.0.1. Apple container 1.6.1 and the existing runner image (claude 2.1.314 inside). "Fake claude" is
the real, unmodified `claude` pointed at the scripted Messages API stand-in from `runner/test/support/mock.mjs`
through the instance's environment, so no subscription or API key was used.

| Spike | Result |
|---|---|
| (a) Headless nightly, temp data dir, telemetry off | **Yes.** `t3 serve --base-dir <tmp> --port <p> --host 127.0.0.1 --no-browser` with `T3CODE_TELEMETRY_ENABLED=false` and `OTEL_SDK_DISABLED=true`. No socket to anything but loopback while it ran. The npm package is a single bundled package (no install scripts needed). |
| (b) Go OAuth client + thread ops | **Yes.** Dynamic registration, then `POST /oauth/mcp/decision` with `{_tag: "pairing-code", access, code}`, then the PKCE token exchange. The pairing code comes from `t3 auth pairing create --scope ...` on the host, so Rubi Home can pair itself once the user approves it in the Rubi panel. The token lasts 30 days. Then `t3_project_create`, `t3_thread_launch`, `t3_thread_wait`, `t3_thread_read` and `t3_thread_send` (idempotent with `clientRequestId`) worked; 68 tools are listed. |
| (c) `binaryPath` wrapper, claude in Apple container | **Yes.** A wrapper runs `container run --rm -i` with the cwd and `CLAUDE_CONFIG_DIR` mounted at the same paths; stdout stays clean and a start takes about 1 s. Launch, a second turn (resume) and `t3_thread_transfer {action: "fork"}` all worked; the fork's first turn saw the earlier history. Session files are written to the host folder. |
| (d) Sign-in in the container | **Partly; the rest needs a real sign-in.** `claude auth login --claudeai` in the container prints the sign-in link and the "Paste code here if prompted" box (the relay case of R4) and writes its state to the host-mounted config dir. Completing it needs the owner's own sign-in. Rubi only mounts the folder. |
| (e) PreToolUse hook in the VM reaches Rubi | **Yes.** A managed-settings hook (`/etc/claude-code/managed-settings.json`, mounted read-only, `allowManagedHooksOnly`) posts the hook input to a gate on the host. In **Full access** (`bypassPermissions`): deny blocks the tool and the model gets the reason; allow runs it; a gate that can't be reached is a deny. A project's `.claude/settings.json` with `disableAllHooks: true` does not turn the managed hook off. |

Shared sign-in (decision 4): **safe enough to build, with a live check later.**
- claude reads `.credentials.json` and takes its refresh lock (`.oauth_refresh.lock`, mkdir-based, stale after
  60 s, refreshed every 5 s, owner record aware of PID namespaces) in the folder named by
  `CLAUDE_SECURESTORAGE_CONFIG_DIR`, not in `CLAUDE_CONFIG_DIR`. So each role keeps its own config dir (skills,
  settings, MCP) and all roles share one sign-in folder.
- Two VMs on one shared host folder: a mkdir lock held in one VM gives `EEXIST` in the other; its mtime updates
  are seen; an atomic rename of the credential file is seen at once; the lock can be taken after release.
- Risks: the variable is undocumented, so a contract test checks it on every bump; the concurrent refresh with a
  real account is checked live in S2. If either fails, we fall back to one sign-in per role.

Findings that become requirements:
- **F1 Clean environment.** T3 hands its own environment to the provider process. Rubi Home starts T3 with a
  minimal environment, and the wrapper passes only an allow-list into the VM (`CLAUDE_CONFIG_DIR`,
  `CLAUDE_CODE_*`/`CLAUDE_AGENT_SDK_*` without token, OAuth, account or organization names, `T3_AGENT_THREAD`).
  A test in S2 checks the list.
- **F2 Unknown keys are ignored.** `t3_project_create` with a wrong key (`path`) silently created a project in
  T3's default folder. The bridge sends typed requests only, and a contract test pins the input schemas.
- **F3 T3's own MCP from the VM.** The injected `t3-code` MCP points at host loopback; the wrapper relays it. Its use
  from inside the VM is checked in S1.
- **F4 Custom home drops user settings.** With `homePath` set, T3 loads only project and local settings, so role
  policy (hooks, permissions) goes into managed settings in the VM, which also can't be changed from inside.
- **F5 Credential reads.** Agent Bash in the VM can read the shared credential. Managed settings deny reading it;
  whether Claude Code's sandbox (bubblewrap) works inside Apple container is checked in S2.

## 10. S1 design: the bridge package and contract tests

Package `internal/home/t3` (Go). No new module dependencies: the MCP client is the go-sdk already in `go.mod`
(`mcp.NewClient` + `mcp.StreamableClientTransport`, standalone SSE off). The pin and the contract snapshot are
embedded from a small root package `t3` (`t3/embed.go`: `upstream.json`, `contract/*.json`).

- **Modes** (`mode.go`). `Mode` is one of `approval-required`, `auto-accept-edits`, `auto`, `full-access`, in that
  order. `Clamp(want, ceiling)` returns the lower one; an unknown mode is treated as `approval-required`.
  `PairingScopes(access)` gives the scopes a pairing code needs for an MCP access level (read-only:
  `orchestration:read`; full-access: adds `orchestration:operate`, `settings:write`, `source-control:write`;
  the others: read and operate).
- **Sign-in** (`oauth.go`). `Pair(ctx, hc, base, code, access, clientName) (Token, error)`: discover
  `/.well-known/oauth-authorization-server` (issuer must equal base), register a client with a loopback redirect,
  PKCE S256, `POST /oauth/mcp/decision` with `{authorization, decision: {_tag: "pairing-code", access, code}}`,
  read `redirectTo` (state must come back; `iss` must equal the issuer), then the form token exchange with
  `resource = base + "/mcp"`. `Token{Access, Scopes, Expires, ClientID}` never prints the access token (its
  `String`/`GoString`/`MarshalText` redact it except for saving to disk, which uses an explicit method). Errors
  carry T3's message, never the pairing code.
- **Client** (`client.go`). `New(base string, tok Token, ceiling Mode) *Client`. Every request carries
  `Authorization: Bearer` and `t3-mode-limit: <ceiling>`. The session connects lazily and reconnects once if the
  server dropped it. A result with `isError` becomes `*ToolError{Tool, Message}`. Results are read from
  `structuredContent` into typed structs.
- **Ops** (`ops.go`), typed requests only (finding F2):
  - `Projects(ctx) ([]Project, error)` (follows `nextCursor`);
  - `CreateProject(ctx, root, title) (Project, error)`: sends `workspaceRoot`; if T3 answers with another root,
    that is an error;
  - `Launch(ctx, LaunchIn{ProjectID, Title, Instance, Model, Mode, Message, RequestID}) (Run, error)`: the mode is
    required and clamped to the ceiling;
  - `Send(ctx, threadID, message, requestID) (Run, error)`; `Wait(ctx, threadID, timeout) (WaitResult, error)`;
  - `Read(ctx, threadID, after int) (Thread, []Item, error)`; `Interrupt(ctx, threadID) error`;
  - `Fork(ctx, threadID, title) (string, error)` (`t3_thread_transfer` with `action: "fork"`).
  - Terminal run states: completed, failed, cancelled, interrupted, rolled_back.
- **Server** (`server.go`). `Server{Node, Bin, BaseDir, Port}` runs the pinned `t3 serve --base-dir <dir>
  --port <p> --host 127.0.0.1 --no-browser` with a minimal environment (finding F1): `PATH` (the node's folder,
  `/usr/bin`, `/bin`), `HOME`, `T3CODE_TELEMETRY_ENABLED=false`, `OTEL_SDK_DISABLED=true`, nothing else. Start waits
  for `userdata/server-runtime.json` (matching pid) and an answer on the port, and reports `serverVersion`.
  `PairingCode(ctx, scopes, ttl, label)` runs `t3 auth pairing create ... --json` and returns the credential
  (never logged). Stop ends only the process it started.
- **Tests that always run** use an `httptest` T3 stand-in (OAuth endpoints that verify PKCE, state and resource;
  an MCP server from the go-sdk with fake tools that record the headers and arguments): sign-in, wrong code (the
  error does not contain the code), mode clamping (a `full-access` launch under an `approval-required` ceiling
  sends `approval-required`), the `CreateProject` root check, `ToolError`, reconnect, and the exact server
  environment.
- **Contract tests** (`contract_test.go`), skipped unless `RUBI_T3_CONTRACT=1`, with `RUBI_T3_BIN` (the pinned
  `t3`) and `RUBI_T3_NODE`; the run part also needs `RUBI_T3_CLAUDE` (an unmodified claude) and uses the scripted
  Messages API mock from `runner/test/support/mock.mjs`, so no account is used:
  - the server version equals the pin;
  - pairing with a minted code gives the expected scopes;
  - `tools/list`: the names and input schemas of the tools we use equal `t3/contract/tools.json`
    (`RUBI_T3_UPDATE=1` rewrites it);
  - with a mock-backed Claude instance written into `settings.json`: create a project, launch, wait, read the
    reply, send twice with one request id (one message), fork, run a turn in the fork and see the history.
