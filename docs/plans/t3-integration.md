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

1. **`t3bridge` in Rubi Home** (Go, package `internal/t3`, host-neutral).
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
3. **Plugin "T3 Agents"** (`rubi-t3agents`, the successor of rubi-claude-code).
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

- **R5 Coexist with the user's own T3** (approved 2026-11-03). T3's desktop app never attaches to a running server: it always spawns
  its own bundled server, on the first free port from 3773 up (`apps/desktop/src/app/DesktopApp.ts:41,78-108`),
  with state in `T3CODE_HOME` or `~/.t3/userdata` (`app/DesktopStatePaths.ts:13-32`). A server holds an exclusive
  lock on its state dir (`server-owner.sqlite`, `apps/server/src/serverOwnership.ts:39-47,183-199`); a second
  server on the same dir exits with code 78 (checked live), and the app then shows "This T3 home is unavailable"
  and quits (`backend/DesktopBackendPool.ts:176-183`, `backend/DesktopBackendManager.ts:805,875-881`). So:
  - Rubi Home's managed T3 uses its own home inside Rubi Home's folder, never `~/.t3`, and a free port other than
    3773 (recorded in Home's config). The user's desktop app or `t3 service` keeps `~/.t3` and both run side by
    side: two servers, two states, no port clash, no shared sessions.
  - To watch Bot-started threads in the desktop app, the user pairs the app with Rubi's server: the Rubi panel
    shows a pairing link minted by Home (`t3 pair` / `t3 auth pairing create --base-url ...`) to paste into Add
    environment or open as `t3code://pair` (`docs/user/remote-access.md:84-98`), plus a hint: to use only Rubi's
    server, switch off Settings → Environments → Local environment (`remote-access.md:455-464`;
    `apps/desktop/src/app/DesktopApp.ts:190-196`). Rubi never edits the desktop app's settings.
  - Optional later ("use my own T3"): Home pairs with the user's own server instead, only if its version equals the
    pin; the role instances then live in the user's settings. Not in v1.
  - Tests (done): `TestServerStaysApartFromTheUsersT3` (a base dir that is or is inside `~/.t3`, also through a
    symlink, and the port 3773 are refused) and the contract test `TestContractBesideTheUsersT3` (the pinned T3
    owns `~/.t3` of a throwaway home; Rubi's server starts beside it; a second server on that home exits 78).

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
| S0 spikes | (a) The pinned nightly runs headless in a temp user-data dir with telemetry off. (b) A Go client completes the OAuth sign-in to localhost `/mcp` with a pairing code, then runs launch/send/wait/read on a fake-claude instance. (c) `binaryPath` = wrapper runs the real claude in Apple container with the cwd mount; history and fork work in T3. (d) `claude auth login` in the container stores credentials in the host-mounted config dir, and Rubi never reads them. (e) The PreToolUse hook in the VM reaches Rubi and a denial blocks the tool. Each spike gives yes or no, with logs. |
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
6. Naming: the core stays Rubi. The plugin's display name is "T3 Agents"; its package and repository id is
   `rubi-t3agents`.
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

Package `internal/t3` (Go; moved from `internal/home/t3` so a server host can use it too). No new module dependencies: the MCP client is the go-sdk already in `go.mod`
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
  `Authorization: Bearer` and `t3-mode-limit: <ceiling>/default` (the header is `<runtimeMode>/<interactionMode>`;
  `default` is the wider of `plan`/`default`). The bearer never leaves the base: no redirects are followed and the
  transport refuses any other host. Launch and Send always carry a request id (generated if the caller gives none),
  so the one retry can't duplicate a thread or message; after Launch the thread's mode is read back and checked
  against the ceiling. The session connects lazily and reconnects once if the
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

S1 notes (as built): `/oauth/mcp/register` answers 201; `Pair` follows no redirects and refuses endpoints outside the
base. `t3 serve` prints a pairing QR code at start, so the server's output goes only to `<base-dir>/server.log`
(0600), never into errors. The server runs in its own process group; Stop signals the group only while the leader
is not yet reaped, and a dead server is noticed (`Done`, `Alive`). The contract test merges its instance into the
`settings.json` T3 writes on first start.

## 11. S2 notes

Shared sign-in layout, checked behaviorally on the claude in the runner image (no network, a fake credential file):
- `auth status` with `CLAUDE_SECURESTORAGE_CONFIG_DIR=<dir>` holding `.credentials.json` says `loggedIn: true`;
  without the variable (same `CLAUDE_CONFIG_DIR`) it says `loggedIn: false`.
- With an expired credential and a `.oauth_refresh.lock` directory already held in that folder, claude refuses to
  refresh ("another Claude Code process is refreshing it") within seconds; without the held lock it tries the
  network. So the credential and the refresh lock both live in that folder.
- These two checks become the loud contract test for decision 4 (it runs on every image or pin bump).

Hosting (decided 2026-11-03): `t3.host` is a setting. The control plane may run on a server host, execution runs on
the user's PC when it is online, and provider logins never live on a machine shared with Grok Bots (they all have
sudo there). The options for running without the PC are in the hosting notes kept outside the repository.

## 12. S2 design: running T3's claude in a role VM on the user's PC

T3 runs one `claude` per provider session through the instance's `binaryPath`. Rubi Home owns every policy (VM,
egress allow-list, approvals, sign-in folder); the wrapper is a thin client.

- **Role instance.** For each role Home writes a T3 provider instance `claude-<role>` into its T3 server's
  `settings.json`: `binaryPath` = a per-role shim `<helper>/t3/bin/claude-<role>` (`exec <rubi-home> agent-vm --role
  <id> -- "$@"`), `homePath` = the role's claude dir (`agent-roles/<id>/claude`). No secrets in the instance env.
- **Session start.** `rubi-home agent-vm` dials `<helper>/agent-vm.sock` (0600, peer-checked like `claude.sock`) and
  sends `{op: "session.start", role, cwd, argv, t3_thread}`. Home checks: the role exists and is pinned to `cwd`
  (project trust, `DirHash`), `cwd` passes the same folder checks as a chosen folder, and `argv` is clamped
  (`--permission-mode` lowered to the role's ceiling; `--dangerously-skip-permissions` dropped below full access).
  Home then starts a detached VM for this session only (unique name `rubi-t3-<role>-<random>`; the netpipe exec
  started by Home), and answers with the container name and an env file (0600). The wrapper runs
  `container exec -i --user 10001 --workdir <cwd> --env-file <f> <name> <claude> <argv...>` with stdio passed
  straight through, keeps the socket open for the session, and Home stops and deletes the VM when the socket closes
  (or the exec ends). A wrapper that can't reach Home exits non-zero with a plain message; it never runs claude
  outside a VM.
- **Mounts (same paths, finding (c)).** `cwd` read-write at its host path; the role's claude dir read-write at its
  host path (T3's host-side history worker reads the session files there); the shared sign-in folder
  `<helper>/agent-signin` read-write at its host path; the role content read-only (as today); managed settings
  read-only as a single file at `/etc/claude-code/managed-settings.json` (a single-file bind worked in S0 with
  container 1.6.1); the hook script read-only.
- **Env (finding F1).** Only: `CLAUDE_CONFIG_DIR` (role claude dir), `CLAUDE_SECURESTORAGE_CONFIG_DIR` (sign-in
  folder), `HTTPS_PROXY`/`NO_PROXY` (the allow-list proxy through netpipe), `CLAUDE_CODE_*` and
  `CLAUDE_AGENT_SDK_*` from T3 minus names with TOKEN, OAUTH, ACCOUNT, ORGANIZATION, HOST_AUTH or API_KEY_HELPER,
  `T3_AGENT_THREAD`, and the image's `DISABLE_*` telemetry switches. A test pins the list.
- **netpipe services.** Today: proxy (1) and gateway (2). Added: hook (3) on `127.0.0.1:3130` and T3 MCP (4) on
  `127.0.0.1:<T3 port>` (finding F3: T3's injected `t3-code` MCP points at host loopback). The gateway service goes in S5.
- **Egress.** The proxy's allow-list for a T3 role VM adds `api.anthropic.com` (no gateway any more) to the login
  hosts, the base hosts, git and forge hosts and the role's hosts; unlisted hosts still ask.
- **Approvals.** Managed settings: `allowManagedHooksOnly: true` and one PreToolUse command hook (matcher `*`) that
  POSTs the hook input to `127.0.0.1:3130`. Home maps it to the same approval path as the runner's `perm.ask`
  (`Manager.Ask` / `waitDecision`, tiers and modes unchanged) and answers in the PreToolUse shape
  (`hookSpecificOutput{hookEventName, permissionDecision, permissionDecisionReason}`). An unreachable gate, a
  timeout or a malformed answer is a deny (checked in S0).
- **Concurrency.** One VM per T3 provider session; per-role and global caps from the existing limits; a session
  over the cap is refused with a plain error that T3 shows in the thread.

KVM access on a server host (decided 2026-11-03): some hosts give `/dev/kvm` to a group the service user is not in,
and a hand-set ACL is lost on reboot or system update. `internal/kvm.Ensure` checks that the device opens
read-write; with the opt-in install setting `GrantWithSudo` it re-applies `sudo -n setfacl -m u:<current user>:rw
/dev/kvm` at every start, logs it, and checks again; without the setting, or without passwordless sudo, it refuses
with a message that says what to do. No user or group name is hard-coded. The microVM backend calls it before it
starts a VM.

S2 part 1 notes (as built, `internal/home/agent/t3vm`):
- **Hook behaviour, checked with the real claude 2.1.314 in the runner image** (managed settings at
  `/etc/claude-code/managed-settings.json`, a Bash call, `bypassPermissions`): deny JSON with exit 0 blocks the tool;
  exit 2 blocks it (with or without JSON); exit 1, a throw, an unhandled rejection and non-JSON output with exit 0 let
  the tool **run**. So the hook script prints the deny JSON and exits 2 on every failure, and has crash handlers that
  do the same. A hook's `updatedInput` without a `permissionDecision` is honoured (the changed command ran, the
  original did not), so the gate returns it that way. The script has its address and timeout as constants and reads no
  environment: a repository's settings `env` block reaches hook processes.
- **Gate per VM.** One `Gate` per session VM, bound to that VM's hook listener and target; the session id and cwd in a
  hook input are claims from inside the VM and never route an ask.
- **MCP.** Sessions always run with `--strict-mcp-config` and only an inline T3 server (`http`/`sse` at
  `127.0.0.1:<T3 port>`) is accepted. The managed key `allowManagedMcpServersOnly` exists in this version but is not
  set: it would also gate T3's own server, which is not verified.
- **Residual.** The sign-in folder is mounted read-write because a refresh rotates the token, so agent code in the VM
  can read the credential. The managed deny rules for it are best effort (a shell can still reach it by a path the
  rule does not match); the boundary is that the VM has no network except the allow-list proxy.

T3's own MCP for a session (checked at the pin): T3 hands it to claude through the SDK control channel on stdin
(`setMcpServers`), not on argv, at `http://<server bind host>:<port>/mcp`; with Rubi's server bound to 127.0.0.1
that is `http://127.0.0.1:<T3 port>/mcp`, which the VM reaches through netpipe service 4. The argv check covers only
what arrives as flags.

S2 part 2 notes (as built):
- **Pieces.** `internal/home/agent/t3vm`: `Server` (the session socket `<helper>/agent-vm.sock`, 0600, same-user peer
  check), `Run` (what `rubi-home agent-vm --role ID -- ...` does: asks Home, then `exec`s `container exec` with the
  connection to Home inherited, so the VM lives exactly as long as the process), `SyncInstances` (role instances and
  shims in T3's `settings.json`). `internal/home/agentvm.go` starts the server and syncs the instances while
  `t3_port` and `t3_base_dir` are set in the configuration (there is no other T3 wiring in Home yet).
- **What the real T3 (0.1.7-nightly.20261102.4093) runs through `binaryPath`**, seen in the live test:
  - `claude --version` from T3's data folder, with no session: Home answers it (`claude.version`, a throwaway
    no-network VM with no mounts, cached per image) so the shim still prints the image's version;
  - the session: `--effort high`, `--allow-dangerously-skip-permissions`, a repeated `--thinking-display`,
    `--settings '{"showThinkingSummaries":true}'` (only that setting is accepted), `--add-dir` for the folder and
    `<base>/userdata/attachments`, `--session-id=...` or `--resume=...`; no `--mcp-config` in the launches seen;
  - a capability probe from T3's data folder with `--setting-sources=user,project,local --no-session-persistence
    --settings '{"disableAllHooks":true}'`: refused (its folder is not the role's, and it asks for hooks off). T3 goes
    on without it.
- **The shim names Home's directory** (`RUBI_HOME_DIR`): Home starts T3 with almost no environment, so `agent-vm`
  could not otherwise find the socket.
- **Instances** are recognised as Home's by `config.binaryPath` (inside `<helper>/t3/bin`, named `claude-*`), a
  known field, so nothing depends on T3 round-tripping unknown keys. The `environment` array is removed from them.
- **Mode ceiling.** A role's `default_mode` is its default, not a cap (the store fills in "default" for every role),
  so it does not lower the ceiling: the ceiling is full access, or auto while `agent_allow_bypass` is off. Every call
  still goes to the hook gate.
- **Caps.** T3 sessions count against the usage mode's per-role and per-computer limits, separately from the runner's
  slots (the default is one session per role).
- **Not done here.** Applying the role's skills, agents and MCP to the claude dir for a T3 session (the runner does
  that for its own sessions); instances follow the roles only at Home's start and on `SyncT3Instances`.

S2 part 2 review fixes (as built):
- **Start and crash sweep.** `Serve` takes `<helper>/t3vm/sessions.lock` for its whole life (a second Home on the
  directory is refused, so it can't remove live VMs or take over the socket), removes every `rubi-t3-*` container
  (sessions and version probes) and clears `t3vm/env/`. A VM that outlives a crashed Home is fail-closed anyway: its
  hook gate is unreachable (a deny) and its proxy is gone (no network). Nothing in the VM exits by itself: the
  forwarder does when its stdin closes, but the container's process is `sleep infinity`, so the next start's sweep
  (or the connection closing, for a shim that died) is what removes it.
- **Mounts are checked after the run.** The image is made ready first, then the paths are resolved and the container
  is run; `container inspect` must then show exactly the planned mounts (source, target, read-only), and every source
  must still be its own real path. Anything else removes the VM and refuses the session. Residual: the tool follows
  a link swapped in between the resolve and the run for the moment before the check removes the VM.
- **Callers.** With `T3Pid` known (Home runs T3) the socket serves only descendants of that process; otherwise it
  serves the same user, which is all that can be said of a T3 Home did not start. The process-tree walk is a small
  copy of the claude hook's (`internal/home/peer*.go`), because `home` imports `t3vm`.
- **Caps.** A start waits up to 10 s for a place and a session frees its place as soon as its connection is gone.
  T3 sessions are counted separately from the runner's slots (`Manager.slots` has no shared counter); the two
  kinds can together exceed the computer-wide limit.
- **Instances.** An entry is Home's only if its key is `claude-*` and is the base name of its shim in
  `<helper>/t3/bin`; the shim is removed by that base name. The settings file is written where a symlink points,
  re-read and compared just before the rename (and recomputed on a change, five times at most), synced, with the
  folder synced after.
- **Version probes** run one at a time per image, are named `rubi-t3-ver-*`, and a failure is remembered for 30 s.
- **Answers carry an absolute container path**, and the shim runs only a program named `container` with `exec`.

## 13. S3 design: the T3 Agents plugin on the same op surface

The fastest way to carry every concept over is to keep the plugin-to-Home contract and change what is behind it.

- **Home: op family `t3-agent.*`** with the same names, arguments and results as `claude-agent.*`: `start`, `send`,
  `messages`, `next`, `status`, `close`, `interrupt`, `fork`, `mode`, `rename`, `sessions`, `projects`, `limits`,
  `usage_mode`, `decide`, `role.*`, `ssh.*`. They are backed by T3 instead of the Agent SDK runner:
  - start → `t3_thread_launch` on instance `claude-<role>` in the T3 project for the approved folder (created on first
    use), with the mode clamped (role ceiling and bridge ceiling). The session id is T3's thread id.
  - send → `t3_thread_send` (request id from the plugin's message id).
  - messages / next → `t3_thread_read` (after position) plus the approval queue. Tool asks arrive through the VM hook
    gate into `Manager.AskTool`, the same queue `claude-agent.next` serves, so `decide` is unchanged.
  - status, interrupt, fork, rename, close → read, interrupt, transfer fork, update title, archive.
  - mode → `t3_thread_configure` runtime mode, clamped.
  - role.*, ssh.*, limits, usage_mode → the existing Home stores, unchanged. Role changes re-sync the T3 instances.
  - A turn-finished signal is a `t3_thread_wait` long-poll per running thread inside Home. It turns into the same item
    `next` already returns for a finished turn, so Bots are woken by events.
- **Home: the T3 lifecycle.**
  - Install the pinned T3 and Node from `t3/upstream.json` into Home's folder, with integrity checked.
  - Start the server (its own home, a free port other than 3773, R5).
  - Pair the bridge with a pairing code minted on the host, keeping the token 0600.
  - Sync the role instances and serve `agent-vm.sock`.
  - Report all of it in `agent-setup.status`-style ops, `t3-setup.*`, for the setup page.
- **Core.**
  - Capability `t3-agent` in the Home capability list.
  - The same attestation rules as for `claude-agent` (role widen, project add, SSH register).
- **Plugin `rubi-t3agents` ("T3 Agents").** It is a copy of the latest claude-code plugin (roles, mandates, project
  trust, library, SSH, briefs, distinct binding errors R1, bindings that survive restarts R2, project add by approval
  R3, the setup page R4), with:
  - tool prefix `t3_`;
  - the Home op prefix `t3-agent.`;
  - capability `t3-agent`;
  - the setup page's steps: container, image, T3 installed and pinned, T3 paired, provider sign-in per role, self-test.
  - The desktop hook path is not part of it.
