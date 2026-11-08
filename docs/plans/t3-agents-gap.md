# T3 Agents end-to-end gap audit

Base: origin/main 2fbae6c (#20 08eeb0d, #21 0586503, #22 2fbae6c), inspected locally 2026-11-08 (Europe/Berlin).
No live instance/config/account was used. The checkout has a narrow fetch refspec;
`git fetch origin main` alone left origin/main stale. Explicitly updating the
tracking ref fixed this before any implementation.

| Target step | Exists on main | Missing/broken / required change |
|---|---|---|
| Request from MCP/plugin/panel | Official t3agents capability, plugin home relay, Home `t3-agent.start`, generic approval engine and plugin pages | Official plugin source/manifest is not in this repository. Core currently gates role widening/SSH/folder enrollment, not a repository task mandate. Need one exact, core-attested task confirmation covering provider, project, brief and publish scope; no per-tool approval loop. |
| Resolve project/role/provider | `t3agent.go`, Manager.T3Role, canonical DirHash, per-project instance sync; sync failures refuse before launch | Starts directly in the enrolled working folder. No owned task ID, branch, independent copy, durable task lifecycle, or trusted worktree binding. |
| Create isolated repo copy | No task implementation (`shield.go` only recognizes existing .git/worktree shapes) | A linked git worktree points to the original repo's common gitdir. Merely granting that directory would expose other branches/main and break isolation. Use an independent non-hardlinked object database, task-only branch and no remotes inside the sandbox. Preserve dirty source files; base explicitly on captured committed HEAD. |
| Launch through T3/srt | Nine CLI shims, OpenCode pipe bridge, Cursor project-private unmodified T3 server; settings generated from trusted role/config; pinned release runtime | Need per-task instances/bindings/cwd and Cursor environment keys; do not grant the original repo/common gitdir to make git commit work. Fail if metadata, role, source binding or workspace integrity changed. |
| Edit/test/commit | Provider tools operate inside policy writable roots; real srt isolation tests; T3 supports full-access modes subject to ceilings | Task approval must authorize autonomous tool use inside that task, not bypass the sandbox. Git identity, hooks/config/environment and object-store isolation require explicit provisioning/tests. |
| Append/stop | `send` (queue or steer), `interrupt`, `close`, running-thread persistence/watchers | Must retain task binding for every operation, serialize completion vs append/stop, prohibit publish after stop and recover unknown launch outcomes without duplicate tasks. |
| Finish/diff/tests | `finishTurn` forwards bounded assistant text to existing agent queue (`to`, `in_reply_to`) | No trusted task result, commit/diff/test summary or artifact card. A completed conversational turn is not necessarily a completed task. Need explicit terminal task state and idempotent finalization. |
| Push branch only | Legacy VM `git.credential` + local 0600 FileCredSource with `cred.ask` | Not wired to srt or RubiPod/vault. Do not hand push token to provider. Trusted finalizer must pin destination and exact refspec, refuse main/default/protected refs and changed heads, disable hooks/config execution, avoid secret argv/logs, and support safe retries. |
| Open PR | Generic plugin HTTP/account brokerage exists | No T3 task GitHub PR integration. Bind repo/base/head to the confirmed mandate; token supplied only by vault broker; idempotent lookup/create; no merge API. |
| Phone card + requester message | Generic plugin pages/pads/cards, agent result event queue and recipient routing | No task-specific card/actions/result schema in Rubi or bundled plugin source. Need requester-only routing for start failures and final events, with work/diff/tests/PR content. |
| Setup/old VM paths | `startSessions` still starts ServeAgentVM; t3host comments/capability text and legacy setup diagnostics describe VMs; old tagged t3live tests exercise VM route | Remove obsolete VM service from srt hot path only after compatibility tests; do not delete generic legacy infrastructure blindly. Setup must validate installed native/provider paths and sign-ins rather than declaring VM probes sufficient. |
| Verification | Real bwrap+srt filesystem/network/ingress probes and native T3/OpenCode readiness; no real model turns | No full worktree -> provider -> commit -> authorized publish -> PR -> card/message e2e. Build disposable Home/T3 and fake provider protocol; bare local remote and mock GitHub only. Mac bridge remains unsupported/fail-closed. |

## Dependency order
1. Owned independent task workspaces and trusted task sandbox/session binding (first e2e blocker).
2. Core-attested single task mandate, completion/cancellation/restart state machine and trusted exact-branch publishing/PR vault broker.
3. Official plugin/tool/panel wiring and full disposable e2e through the real entrypoint, including append/stop/failures; remove legacy VM hot-path service and misleading setup gates.

Each slice must start at the then-current origin/main. Dependent slices cannot be
stacked over an unmerged bundle without violating that instruction; the parent
applies/merges each reviewed green slice before the next dependent branch.
No claim of a fully working account-backed flow is justified until these gaps
and the final e2e are closed. Release/live deployment and provider login will
still require the owner-approved rollout, not changes to either live instance.

## First slice implemented (citri/t3-agents-task-worktree)

- Owned independent task copies, immutable source/project/role binding and per-task
  provider shims/Cursor environment keys. Single-use, exact device-bound core
  execution grant (not yet an official plugin action). Session listings and
  requester result events retain task/copy/branch. Fork preserves the task;
  damaged Git metadata blocks append but not an owned stop.
- Native-only bugs reproduced and fixed: settings-file watch debounce rejected a
  newly enabled instance; the legacy filename-ID validator rejected native
  `mcp:launch:...` thread IDs, breaking append/read/stop. Tests cover both.
- Real disposable Home RPC -> unmodified pinned T3 Nightly -> generated shim ->
  real bwrap+srt -> synthetic Codex app-server -> file edit and Git commit passes.
  Host source/project-B/generated HOME fixtures remain unchanged and unreadable;
  denied networking, mid-turn phone append/steer, interrupt, and a separate
  requester-only provider failure are exercised. No owner account/model used.
- This proves the **Home task/session substrate**, not the complete requested
  plugin/panel-to-PR lifecycle. No trusted publisher/vault/PR/card implementation
  or bare-remote/mock-GitHub publishing proof exists yet. The official t3agents
  plugin source is outside this repository and still needs its task action.
- Remaining: durable task request idempotency/restart recovery; terminal task vs
  conversational turn; stop/fork/append-vs-publish serialization; exact-refspec
  vault-backed publication and PR; card/message schema/tool rollout; record/copy
  retention cleanup; original concurrent session-cap check/launch race; legacy
  VM service/setup gates on the default startup path. These must be closed before
  calling T3 Agents fully working or deploying to live accounts.

### Verification caveats

Final `go vet ./...` and `go test ./... -count=1` pass. All touched-package
race suites pass on rerun and no race detector warning occurred. A core race run
hit `TestAsksOfDifferentBotsDoNotMerge`'s TempDir cleanup failure; the same
failure reproduced 5/40 times on an untouched 2fbae6c worktree. That pre-existing
asynchronous approval-cleanup flake is not fixed here. One initial full run also
hit the unchanged VM sweep test; its 20-repeat check and the final full run pass.
Native task smoke passes three consecutive runs; real srt/bridge integration
passes. No Mac or owner-account model turn was run. No live daemon/settings,
GitHub push, PR, release, or publication was performed.
