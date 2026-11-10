# Provider CLI sandbox launcher and T3 wiring

`cmd/rubi-agent-exec` accepts `--config /absolute/trusted/binding.json --`
followed by provider protocol arguments. The JSON is a `Binding`, not a caller-
supplied `Launch` or policy. It contains Rubi Home's directory, role ID, project
ID, the role's directory hash and provider driver. It does not contact a live daemon.

## Resolution and T3 settings

`Helper.SyncT3Instances` uses the existing manager's active, directory-bound
roles. It writes fixed shims in `<Home>/t3/bin` and bindings in
`<Home>/t3/sandbox/<role>/<driver>/binding.json`. Each shim executes the installed
`rubi-agent-exec --config <fixed binding> -- "$@"`; provider arguments cannot
replace the binding. Existing `claude-<role>` IDs migrate in place. Other IDs
are `rubi-<lowercase driver>-<role>`. Personal T3 instances are not modified.

T3's project override schema supports only `enabled`, not `binaryPath`. Therefore
Rubi creates distinct instances, globally off and enabled only in the T3 project
whose root matches the manager-resolved folder. Stale project switches are reset.
A conflicting foreign instance ID or unreadable settings fails sync; the existing
session-start gate returns that error to the requester before creating a thread.

On every invocation the adapter re-reads only `agent_projects`, `agent_sandbox`
and `t3_base_dir` from Rubi's own config, plus the committed current role record.
It uses the same project resolution/filtering as Manager, checks the active role,
project ID and directory hash, and rejects missing/reassigned or overlapping
configured projects. Neither provider arguments, cwd, environment nor T3's
workspace root supplies filesystem grants. A cwd inside the assigned tree is
preserved; T3's out-of-tree status probes run from the assigned project root.
All configured projects are denied read, then only the assigned one is allowed.
The adapter checks that config, role records, shims, bindings and runtime remain
outside writable trees before launching. Missing tools/provider installation
fail with exit 125; no VM or unconfined fallback is used by these shims.

### Repository task copies

Repository tasks require Linux: confirmed stop needs bubblewrap's private PID
namespace teardown plus an owned process group. macOS and Windows refuse before
project lookup, workspace creation or native launch. Standalone macOS srt sessions
remain supported; Cursor/OpenCode project bridges already require Linux. This is
an intentional fail-closed task-only restriction until strong macOS descendant
supervision is implemented, not an unconfined fallback.

Home `t3-agent.start` may opt into `task: true`. The response adds `task`,
`branch` and `base_commit`. An ordinary session keeps its existing behavior.
Core permits the task option only during the official `t3agents.task_start`
confirmed execution. Its payload must be `{device: <stable Home ID>, args:
<exact Home start arguments>}`. Provider, model, source project, role, prompt,
requester and every other argument are compared; the mandate is consumed once.
A device alias cannot redirect an approval to another paired computer. This
execution contract is groundwork: the official plugin's source/manifest is
outside this repository and does not yet expose the new task action.

`internal/agenttask` makes an **independent clone**, not a linked git worktree:
`clone --no-local --no-hardlinks --no-checkout --single-branch`. A linked worktree
would require granting the original common Git directory and its main/other
refs. The copy uses captured committed HEAD, branch `rubi/task_<random ID>`, a
local task identity, no remote, no inherited global/system Git configuration,
no source hooks/templates, no credentials and no network during preparation.
Dirty and untracked source files are neither copied nor changed. The enrolled
folder must be the root of an independent repository with a committed HEAD;
linked worktrees, shared/alternate object stores and symlinked Git metadata
fail closed. Git must be installed at `/usr/bin/git`.

Private immutable task records are `<Home>/t3/tasks/<ID>.json`; writable copies
are `<Home>/t3/workspaces/<ID>`. The task ID in the trusted binding resolves the
copy; neither cwd, environment, prompt nor caller-supplied paths do so. Every
launch checks project/role/source hash, private directory modes, fixed branch
name, Git configuration hash and an independent regular Git metadata tree.
Changing Git configuration or replacing metadata prevents host-side reuse.
The original enrolled project stays denied. Task shims/settings have separate
ID-qualified paths, and Cursor's environment key hashes the copy rather than
the source. Other providers keep their shared T3 server. Fork/append retain the
task binding; stopping uses only the already-owned authenticated client so a
damaged copy cannot disable the stop control. Provider login storage remains
role-scoped; task GitHub publishing credentials are never supplied to providers.

Native T3 watches settings with a debounce. A freshly written instance switch
can therefore briefly be rejected. Rubi retries only the exact native
`invalid_request` disabled-instance **preflight** for the same project/instance,
for at most two seconds, with unchanged arguments and command-receipt ID.
Other failures and ambiguous transport errors are not treated as permission
probes; there is no fallback or global enablement.

Session listings identify the task copy and branch, not the source checkout.
Completed-turn events retain task/branch and requester identity. Native opaque
`mcp:launch:...` thread IDs are accepted by phone controls without treating them
as legacy runner filesystem names; exact persisted ownership is still checked. A completed
turn is **not** a trusted finished repository task: explicit finalization seals
it. The trusted backend below now implements durable state and publishing;
task-specific official plugin/panel actions and automatic completion wiring
remain the next slice. Records/copies are retained; cleanup is not automatic.
See the maintained gap audit in `docs/plans/t3-agents-gap.md`.

The opt-in `TestNativeT3TaskCopyCommitsThroughSandboxedCodex` uses
`RUBI_REAL_T3` (unmodified pinned native executable), `RUBI_REAL_RELAY` (locally
built launcher), and real srt/Node/socat/rg on PATH. It creates only disposable
Home/T3 state and generated fake secret fixtures, using a synthetic Codex
app-server rather than an account/model. Denied directories can be private
writable tmpfs shadows; confinement assertions check inaccessible host contents
and unchanged host files, not merely whether a shadow write returned success.

### Upstream providers

Inspected T3 source: `pingdotgg/t3code` revision `e11c2fdc` (Nightly).

| Driver | T3 launch setting | Rubi-managed route |
| --- | --- | --- |
| Claude Agent | `binaryPath` | shim; role's Claude config directory |
| Codex | `binaryPath`, `setupMode: existing` | shim; isolated CODEX_HOME |
| Grok, Fx, Kiro, Pi, Muse, Antigravity | `binaryPath` | shim |
| ACP Registry | `source: local`, `commandPath` | shim; installed local command only |
| Cursor | in-process SDK; legacy `binaryPath` ignored | separate project T3 server under srt; inherited-pipe ingress |
| OpenCode v1/v2 | local server via `binaryPath` | shim → authenticated frontend → inherited pipes → confined server; remote URL cleared |
| Unknown future driver | unknown | resolver refuses |

The existing nine CLI routes keep their shared T3 server unchanged. Cursor is
never installed there: its SDK executes in that server's process, so a shim
cannot constrain its tools. OpenCode adds a tenth shim route. Personal instances
remain untouched and are **not** a Rubi confinement promise. No T3 fork/patch is used.

### Cursor: project T3 environment (Linux)

`t3-agent.start` accepts `provider: "cursor"` (or `"opencode"`); non-Claude
providers require an explicit `model`. Default remains `claudeAgent`. Cursor
selects a server by the role's trusted project directory hash, not prompt input.
The active role, configured project and persisted environment hash are checked
on every operation, including cached-server access. Send/read/fork/mode/interrupt/
watch operations remain on that server; a missing binding never falls back to
shared T3. Refusal precedes thread creation and returns only to the caller.

Rubi starts its pinned native T3 executable directly inside srt, alongside the
unmodified shared installation. It grants read access only to that runtime's
version directory, installed Node, the relay and explicitly configured assets.
Writable state is `<Home>/t3/environments/<directory hash>/state`, separate from
settings/hidden-home/tmp. Native settings are `state/t3/userdata/settings.json`.
Host-side state access is rooted (`os.OpenRoot`); an agent-created symlink cannot
copy host secrets into settings. HOME/config/data/cache/state directories are distinct.

There is **no host listener** for project T3. Rubi exchanges a native one-time
pairing credential and then OAuth/MCP through a fixed-target inherited-pipe
multiplexer. The relay verifies distinct PID/network/mount namespaces and a fresh
proc mount before executing any backend. No host-network or Unix-socket escape
is enabled. The lifetime pipe uses srt's supported `--control-fd`; EOF/cancellation
kills/reaps the sandbox. A backend crash closes streams and the next request
recreates it. Servers stop after five idle minutes (30-second sweep); active HTTP
responses and running Cursor turns prevent idle termination. Bindings are also
revalidated by the sweep. Changed runtime/grants/credential-path metadata prevents
cache reuse. State survives stop/restart; credentials changed in-place require a
server restart. Other routed CLIs stay shared to avoid changing their login/runtime
semantics or requiring project T3 duplication for every provider.

### OpenCode: authenticated streaming bridge (Linux)

Each managed OpenCode instance has its own fixed role/project shim. Sync forces
`serverUrl` empty, preventing a remote URL from bypassing `binaryPath`, and
provisions/preserves a strong random `serverPassword` in private native settings
for v1. v2 supplies its own generated `OPENCODE_PASSWORD`, which takes precedence
over v1's `OPENCODE_SERVER_PASSWORD`. Empty/short passwords refuse with exit 125;
there is no unauthenticated fallback. T3 owns server borrowing/30-second idle
shutdown; Rubi ties the confined backend lifetime to that shim process.

The real server binds only its private namespace's loopback. A host loopback
frontend requires Basic `opencode:<password>` before forwarding, and carries
HTTP, SSE, binary bodies and bidirectional upgrades over inherited pipes to that
one inner port. It cannot dial arbitrary targets or serve CONNECT. Other projects'
network namespaces cannot reach the frontend, even knowing its URL. Authentication
also rejects unauthenticated host requests; same-user unsandboxed host processes
remain outside this isolation claim. `--hostname/--port` from T3 cannot change the
inner bind. Native authenticated readiness (`/global/health` v1; `/api/info` v2)
is retried through warm-up; no discovery endpoint is advertised before readiness.
Only the T3-compatible listening URL is emitted. Backend diagnostics are discarded
because upstream versions may print passwords.

### Explicit provider credentials

Provider metadata may contain `credentials_file` or a `project_credentials`
map keyed by the **Rubi project ID**. Each names an owner-provisioned absolute
0600 regular JSON file mapping supported credential environment names to strings.
Cursor requires `CURSOR_API_KEY`; OpenCode model-provider keys are optional for
server readiness. No shared/live T3 credential store is read or copied. The files
must be outside all agent-writable/read-exception trees. Every configured file,
including another project's file outside HOME, is added to denyRead; automatic T3
runtime read grants are checked against them too. Loader, Rubi/T3 control variables,
malformed/null maps and writable aliases refuse. Credentials pass only through the
inherited bootstrap pipe and enter the backend environment **inside** srt, never
argv, outer srt environment or logs. The assigned sandbox can use its own credentials.
Native T3/OpenCode state/credentials remain within its private writable state.

The earlier [boundary findings](design/t3-srt-provider-boundaries.md) are historical;
the executable mechanisms above supersede their "not connected" status.

### Release runtime and installation metadata

Linux and macOS release tarballs now include `rubi-agent-exec` and
`srt-runtime/<pin>/<os>-<arch>/` beside `rubi-home`. The universal Mac app includes
both CPU runtimes and a universal launcher in `Contents/MacOS`. The existing
signed archive checksums authenticate the entire distribution; no runtime
network installation, npm lifecycle scripts or unpinned `curl | sh` is used.
Windows archives remain single-executable: sandbox routes stay unsupported.

Pinned inputs are in `internal/agentsandbox/runtime/`: srt **0.0.81**, its complete
npm lockfile (including transitive integrity hashes), Node **24.22.2**, ripgrep
**15.1.0**, socat **1.8.1.3**, and SHA-256s for every native source/asset and the
build/signing recipes. The install pin hashes the lockfile and these source
pins. Changes to either recipe require updating its checksum; a build refuses a
stale recipe pin. Old pin directories are retained, not overwritten while in use.
Node/rg licences, npm licences and socat's GPL source archive plus exact build
instructions accompany the binaries. Socat disables optional TLS/readline/libwrap
support; it provides srt's plain socket transport, not a general TLS client.
It links system glibc dynamically (no statically linked LGPL libc). Release
Linux builds use Ubuntu 22.04: **glibc 2.35+**, Linux amd64/arm64. Node also requires
its normal system libraries. Linux **bwrap remains a system dependency**; missing
bubblewrap or unusable namespaces refuse with no unconfined fallback. macOS uses
system `/usr/bin/sandbox-exec`; Node's supported macOS baseline still applies.

`ResolveRuntime` follows the real executable's installation path, never cwd,
project metadata or an agent-controlled PATH. A present bundle must have the
compiled pin, completion marker and required regular executable files. Broken,
missing-current-pin or symlinked runtime trees refuse rather than choosing a
system srt. A complete runtime supplies absolute Node/srt paths, trusted bundled
rg/socat first on PATH, and a narrow read-only runtime grant. Existing project,
private-state and credential-file overlap checks apply to that grant too; placing
the runtime in agent-writable folders refuses. Install-time manifests verify every
file and reject checksum changes, unlisted files, symlinks and traversal paths.
Manifests are integrity checks, not a substitute for the signed outer archive.

The shell installer copies companions only after the existing signature/checksum
verification; the Mac app self-installer copies its adjacent companions too.
Both use the new binary's `runtime-install SOURCE DEST` helper (no config loading,
pairing or daemon access), stage immutable runtime trees, and atomically replace
the launcher before the main binary. Missing/corrupt companions refuse the main
binary replacement. A previously valid tree is reused only after verification.
Mac signing preserves nested helpers' entitlements/runtime flags (including
Node JIT), refreshes runtime checksums **after** signing, then seals the app;
the Mac release job smoke-checks its sealed native Node.

`Config.AgentSandbox` (`agent_sandbox`) still supplies absolute provider CLI paths,
trusted extra `search_path`, narrow extra `runtime_read` and provider
`allowed_domains`. `agent_exec` defaults to sibling `rubi-agent-exec` when writing
shims. The bundled srt/Node/rg/socat need no per-user path entries. A source build
with **no adjacent runtime tree** can still explicitly provision `srt` and its
trusted dependencies; without either a bundle or explicit srt it refuses. Provider
CLI/T3 installations, credentials and network domains remain explicit: this does
not import sign-ins, enable providers or change any live settings.

The caller provisions an existing 0700 settings directory, project directories,
provider login directory, and a private temporary directory. The settings
directory, launcher, configuration, real CLI, runtime trees and trusted PATH
entries must be outside all agent-writable trees. Symlinks are resolved before
checking overlaps. Temporary settings are mode 0600 and removed after exit.

`Policy.RuntimeRead` names narrow installed runtime trees hidden under HOME or
other denied roots (including the npm dependency tree, not just the srt symlink).
Do not grant a read exception for their parent installation directory if it
contains a writable project or temporary directory. srt can remount nested
writes read-only; both policy checks and a real integration test cover this.

HOME and existing `/home`, `/Users`, `/workspace`, `/cursor`, `/root` roots are
hidden, with only project/login/runtime exceptions. Callers must supply any
additional sensitive roots specific to the host. System paths remain readable;
this is not whole-host read isolation. Normal srt limitations apply, including
same-user process-memory access and provider login credentials visible inside
the sandbox. The private tmp is writable; this does not promise that every
other system temporary directory is unreadable.

Only locale, terminal and timezone variables are inherited. HOME, TMPDIR, PATH
and SHELL are supplied explicitly. Claude/Codex config-directory variables and
XDG state-directory variables are derived from the resolved login directory.
srt itself uses a short private `/tmp/rs-*` directory for its proxy sockets to
avoid the Unix socket pathname limit; `/usr/bin/env` restores the provider
TMPDIR inside the sandbox. The outer socket directory is not agent-writable and is removed on exit.
It must stay readable for srt's proxy transport. No API keys, proxy variables, loader hooks,
SSH sockets or Node options are inherited. Ordinary CLI authentication uses its explicit login directory; the two new
bridged routes additionally support the private credential pipe above. SearchPath is trusted installation input, never the
agent's PATH. A missing srt, node, rg, Linux bwrap/socat or macOS sandbox-exec
fails closed. Unsupported platforms and no-sandbox mode fail closed as well.
No UI or approval bypass is added. Exit codes and byte streams are preserved;
this version uses a child process, not Unix process replacement.

## Verification

```
go vet ./...
go test ./... -count=1
go test -race ./internal/agentbridge ./internal/agentsandbox ./internal/home \
  ./internal/home/t3instances ./internal/t3 ./cmd/rubi-agent-exec -count=1
PATH=/workspace/srt-spike/bin:/workspace/srt-spike/node_modules/.bin:$PATH \
  go test -v ./cmd/rubi-agent-exec -run TestBoundShimSRTIntegration -count=1
PATH=/workspace/srt-spike/bin:/workspace/srt-spike/node_modules/.bin:$PATH \
  RUBI_SRT_NETWORK_TEST=1 go test -v ./internal/agentsandbox \
  -run 'TestSRTIntegration|TestInProcessHostBoundarySRTProbe|TestOpenCodePrivateLoopbackBoundarySRTProbe' -count=1
```

The integration test skips only when srt is absent or the OS is unsupported;
missing dependencies otherwise fail. Without RUBI_SRT_NETWORK_TEST it checks
local filesystem, environment, Unix sockets (Linux), streams and exit code only.
All fixtures use a disposable HOME. No real provider credentials are used.

New real-srt regressions exercise project B/HOME read denial and no write-through,
network denial, other-project ingress denial, authentication refusal, missing
projects, crash/idle cleanup, large frames, concurrent SSE and HTTP upgrades.
OpenCode's actual serve entrypoint is exercised with faithful HTTP CLI fixtures
for both password contracts, including delayed readiness. These are **fake model
providers**, not authenticated model turns.

Disposable official binaries were also smoke-tested under real bwrap+srt:
T3 `0.1.7-nightly.20261102.4093` (native pairing/OAuth/MCP and the actual Rubi project
pool, reuse/stop/restart/persisted-state/refusal), OpenCode v1 `1.19.5` and v2 `2.2.6`
(authenticated native health through the pipes). OpenCode initially timed out or
returned warm-up 503; bounded semantic-readiness retries fix this without relaxing
the sandbox. Smoke fixtures explicitly allow public catalog/npm hosts; production
network domains still come only from installation metadata. Tests are opt-in via
`RUBI_REAL_T3`, `RUBI_REAL_OPENCODE`, `RUBI_REAL_OPENCODE2`; Home pool smoke additionally
uses a freshly built `RUBI_REAL_RELAY`. No account credentials were used. Cursor's
real in-process SDK is included in T3, but no authenticated Cursor tool/model turn
was exercised; fake in-process tests prove the process boundary, not SDK account
compatibility.

## Follow-up integration decisions

- Validate authenticated real CLIs and each provider's login/state conventions.
  Per-role login directories are not automatically populated from shared credentials.
  Ambient environment-based keys and T3 environment overrides are not inherited;
  the explicit private bridge credential mechanism above is the exception.
  This slice uses explicit installation domain lists; the legacy dynamic `net.ask`,
  shared sign-in and git credential/hook transport are not yet migrated to srt.
- The setup wizard's VM probes/sign-in flow remain legacy diagnostics; they do not
  validate this srt route. Its progress now says so. Migrate that flow separately.
- Attachments/worktrees outside the configured project tree remain inaccessible.
  Add trusted resolution for them only with an explicit isolation design.
- Exercise authenticated Cursor/OpenCode model/tool turns and legacy real CLIs.
- Cursor/OpenCode bridges require Linux namespaces and fail closed on macOS;
  design/test an equivalent sandbox-exec ingress/lifetime boundary separately.
- Bridge cleanup is cancellation/control-pipe/process-group based; SIGINT/SIGTERM
  stops OpenCode supervision, but arbitrary signal fidelity/graceful provider
  shutdown and crash-time removal of outer srt temporary directories remain open.
- Native macOS codesigning/sandbox-exec and Linux arm64 packaging still need CI/
  platform execution; the local native dry-run is Linux amd64 only.
- External plugin/UI provider-selection schemas may need their own rollout; the
  Rubi Home operation accepts the selector without patching T3 Code.

Directory bindings hash the canonical existing absolute directory, matching
project discovery across macOS temporary-root aliases. Missing paths refuse; a
retargeted project symlink invalidates the old binding.

### Disposable release dry-run

```
python3 scripts/srt-runtime/build.py --platform linux-amd64 --dest /tmp/rubi-stage
# Build both Go executables into /tmp/rubi-stage; never use a live install directory.
/tmp/rubi-stage/rubi-home runtime-install /tmp/rubi-stage /tmp/rubi-install/bin
RUBI_BUNDLED_RUNTIME_TEST=/tmp/rubi-stage/rubi-home \
  go test ./internal/agentsandbox -run TestBundledRuntimeSRTIntegration -count=1
```

The builder needs Python with tar extraction filters, curl, npm, a C compiler and
make; Linux arm64 uses `--cc aarch64-linux-gnu-gcc`. It never publishes. The local
Linux amd64 dry-run built the real pinned closure, assembled a release archive,
installed into a disposable directory, and ran real bwrap+srt probes with bundled
Node/rg/socat. Providers in isolation probes remain fixtures; this is not an
additional authenticated Cursor/OpenCode model turn. Runtime resolution/install
regressions also cover executable aliases, missing dependencies, tampering and
agent-writable runtime refusal. No macOS signing execution is claimed on Linux.


## Durable repository-task finalization and publication

A task start with publication needs an explicit `project`, `role`, `from`, stable
`message_id`, and `publish: {repository: "owner/repo", base: "main", account:
"connected-account-id"}` inside the **same exact confirmation** as `task: true`.
Core adds a device-bound HMAC mandate; a caller cannot supply a proof or token.
The account must be its stable connected-account ID, not an ambiguous label.
Core/RubiPod resolves that account in the fixed `t3agents` vault namespace, secret
`github_token`, at each publication attempt. A disabled integration, revoked or
rotated token, altered target/requester/device or missing enrollment fails
closed. The token needs only repository Contents and Pull requests write
permissions (plus GitHub's implicit metadata read); no merge is requested.
Connecting that account in the official plugin UI is not implemented here.

Official-plugin Home operations:

- `t3-agent.task.status {id}` returns redacted state: no start/append prompt,
  signature or credential.
- `t3-agent.task.stop {id}` durably ends the mandate and interrupts every known
  task fork, retaining watchers to deliver native cancellation to the latest
  requester. If T3 is unavailable, interruption remains retryable after restart;
  a successful stop response is not fabricated for an unavailable provider.
- `t3-agent.task.publish {id}` accepts **only the task ID** at the Core broker.
  It re-inspects the immutable mandate, resolves the current vault credential,
  and calls trusted Home over end-to-end encrypted RPC. The internal inspection
  and credential-bearing operation cannot be called through plugin forwarding.
  A task without a publish target can be finalized locally without a credential.

`<Home>/t3/task-state/<id>.json` is private, fsynced and atomically replaced under
an interprocess lock. States are `starting`, `running`, `waiting`, `publishing`,
`pushed`, then immutable `finished`/`failed`/`stopped`. Completing a conversational
turn means **waiting**, not task completion. Launch/send intent precedes native
RPC and recovery reuses the same `clientRequestId`. T3 transfer/fork has no such
receipt; an unknown fork outcome fails closed and is never replayed. Native
thread outcomes are also persisted, closing the crash window between a completed
turn and its task-journal update. All task forks must be idle before sealing;
append/fork/stop and publication serialize on the same lock. After publication
seals, stop/append are refused instead of pretending an already pushed change
was undone. Recoverable network/auth errors retain sealed progress for an
explicit retry by the broker; they are not misreported as completed tasks.

Home never runs publishing Git against an agent-owned checkout. It reads bounded
metadata through `os.Root`, rejects changed config, symlinks, devices/FIFOs,
alternates/shared databases and foreign/detached HEAD, then freezes objects into
`<Home>/t3/task-snapshots/<id>` outside provider write grants. A private bare Git
repo must pass strict fsck, approved-base ancestry and nonempty-commit checks.
Limits: 512 MiB compressed object bytes, 100,000 entries and bounded traversal.
Snapshot and parent directories are synced before journal progress is saved;
a retry uses the frozen commit even if the provider later damages its checkout.
A sealed snapshot is never refreshed from agent changes. There is no automatic
resource cleanup or project-code test verification/attestation yet.

Production destinations are fixed to `https://github.com/<approved-repo>.git`
and `https://api.github.com`. Git uses no user/system config, hooks, custom
transports, source remotes, interactive authentication or redirects. It can
conditionally **create only** `refs/heads/rubi/<task-id>`; the expected-absent
lease (`--force-with-lease=<exact-ref>:`) is a compare-and-create, not an overwrite
permission, and even a fast-forward over an existing ref is refused. An existing
identical SHA is a retry receipt and is not pushed again. No other refs/tags,
main/default branch, arbitrary refspec or merge endpoint is accepted.

The installed adjacent `rubi-agent-exec` (or explicitly configured trusted path)
implements Git's credential helper via inherited FD 3. It releases a credential
only for HTTPS `github.com`; helper paths overlapping enrolled projects or
provider-writable storage are refused. No token is written in the sandbox,
Git config, snapshot, journal, argv or environment. HTTP/Git response bodies and
credential-bearing errors are suppressed. The GitHub PR must match the approved
repository/base, exact task branch/SHA and canonical GitHub URL. A lost PR POST
response is reconciled by querying **all** PR states with exact head/base before
creation, avoiding duplicates; publication never calls a merge endpoint.

Terminal state transitions are exactly-once/idempotent; event transport is
**at-least-once**, with stable `<task-id>:terminal` identity. Home re-emits ended
task results during recovery. The official companion plugin polls durable task
status, emits only to the original requester, then acknowledges the exact terminal
event through Home. A fsynced per-task delivery sidecar survives plugin/Home
restart without changing the immutable outcome or growing one plugin-state map.
A crash between emit and acknowledgement can still duplicate transport delivery;
consumers deduplicate the stable device/task event identity.

Verification in this slice uses real Git + a disposable bare remote and a mock
GitHub API (including lost POST response), fake vault credentials and a mock Home
broker, plus the real Git/helper FD exchange. An opt-in unmodified native T3 +
real srt/bwrap run uses an account-free fake Codex for edits/commits, confinement,
network denial, append/stop, failure, and durable/idempotent local finalization.
No real provider account, GitHub token/repository or live daemon is used.
macOS/Windows Home sources are cross-vetted; real sandbox-exec/signed-app tests
and authenticated provider/GitHub runs remain deployment verification.

Pre-journal task records from the earlier groundwork cannot be appended or
published implicitly: they lack a durable mandate. They remain stoppable through
the already-owned client; start a new explicitly confirmed task to continue.
Stopped/failed Cursor recovery never provisions a replacement project server
just to observe an ended run. Stop also serializes with admission, so an
inflight native launch cannot finish after cancellation without being interrupted.

### Supervised cancellation without a native receipt

New tasks require an adjacent/configured `rubi-agent-exec` that answers
`--task-supervision-version` with `1`. Home probes it before creating a copy.
The plugin probes `t3-agent.task.capabilities` before approval **and** execution;
it never falls back to an old Home that might ignore `task:true` and edit source.
Core, Home, helper and companion plugin must be updated together.

Every actual task CLI launch obtains a private, fsynced OS-lock lease outside
sandbox read/write grants. Cursor holds a lease for its entire project server;
OpenCode's shim holds it for its whole bridge/backend lifetime. Admission and
stop share a gate separate from the journal lock: save the stop/publication fence,
then drain all already-admitted launchers; subsequent native launches refuse.
A guardian polls the atomic journal even when Home does not know a native thread
ID. A lease is acknowledged only after runtime process cleanup, never just when
the cancellation context fires. Publication drains leases before snapshotting,
so an idle CLI's background work cannot race the snapshot.

Task runtimes additionally become PID 1 in an outer private Linux PID/user
namespace, mapping **only** the current host UID/GID and preserving that identity
(no introduced root UID; root-refusing CLIs must continue to work). Fixed system
util-linux `unshare` mounts private namespace-local procfs; otherwise bwrap's
`/proc/PID/ns` lookup addresses the wrong namespace. Temporary namespace-only
mount/setpcap ambient capabilities are cleared by fixed system util-linux
`setpriv` (inheritable/ambient/bounding all dropped) **before** srt exec. Provider
UID preservation and zero effective/permitted/inheritable/ambient capabilities
are checked in the real srt boundary test. Linux finishes
reaping this PID 1 only after every namespace process exits, including `setsid`
and double-fork descendants. A process-group signal alone would not prove that.
Missing util-linux or disabled user/mount namespaces refuse; no weaker fallback.
The creator OS thread stays locked until reaping; a kernel parent-death signal
also kills the namespace if its guardian crashes (the incomplete receipt still
remains unconfirmed, never fabricated). Standalone sessions retain process-group
cleanup without this task-only condition.
A non-reaping `waitid(WNOWAIT)` retains child identity until final signaling,
avoiding recycled-PID signals. SIGTERM of the shim reaps before its receipt.

`stopped` means the mandate ended. Status separately exposes `execution_stopped`;
Stop succeeds only after cleanup receipts confirm quiescence. Native interrupt
bookkeeping may remain pending after all execution is proved stopped, reported
as `native_cleanup_pending`. No cancelled start is replayed just to discover an ID.
An abandoned incomplete receipt (trusted guardian killed/crashed before cleanup)
or a pre-supervision legacy task remains **interruption unconfirmed**: an unlocked
file or absent PID is not proof. Those exceptional cases still need trusted
operator/owned-instance reconciliation; no API silently discards that evidence.

### Official actions, cards and automatic retry

Companion source: `Deikus-LXXVII/rubi-t3agents`, branch
`citri/t3-agents-task-ui` (a separate repository/bundle, not a T3 fork).

- `t3agents_task_start`: one locked strong `t3agents.task_start` confirmation for
  exact device/project/role, brief/provider/model, private copy/new `rubi/task_*`
  branch and optional `{repository,base,account}` publication target. Agent text
  cannot alter that scope. Starts are limited per verified requester and globally.
  The approval also explicitly includes native `bypassPermissions` **inside the
  existing project srt boundary**. Home refuses before creating work if existing
  host/role/global ceilings do not allow full-access. It never silently downgrades
  an unattended task into a native permission dialog, and never changes settings
  or relaxes filesystem/network sandbox policy.
- `t3agents_task_status`, `_append`, `_stop`: original verified requester only;
  panel actions belong to the authenticated user. Appends have stable request IDs,
  bounded text and rate limits; ended/sealed tasks cannot restart.
- `t3agents_task_accounts`: stable account IDs/labels only. Optional fine-grained
  `github_token` is entered in the masked panel account setup and kept in the
  encrypted vault. Select only the approved repos; Contents/Pull requests
  read/write. The plugin never places it in settings, task args or provider state;
  Core's existing broker resolves it at use time. Empty setup supports local tasks.
- Native task cards show work, frozen Git diff-stat, **agent-reported** test evidence,
  branch/commit, cleanup status and a canonical PR link; append/stop work on phone.
  Plugin text uses the existing declarative text-node renderer, never agent HTML.
  Core mints full `https://rubi-panel.com/` setup links (no relative/bare fragments).
- Home periodically reconciles durable intents. The plugin retries finalization/
  publication from durable Home status, including `publishing`/`pushed` after a
  lost reply, rather than caching tasks only in memory. Only an approved
  `auto_finish` mandate with an explicit final `STATUS: PASS`, `COMMIT: ...` and
  `EVIDENCE: ...` report auto-seals an idle task. Ordinary answers/questions do not;
  `ISSUES`/`BLOCKED` stay waiting. A new append clears the old report. Evidence is
  labelled as an agent claim, not independent test attestation.

Security: existing E2E/ticket authentication, replay checks and narrow setup/view
permissions remain. Browser RPC additionally rejects foreign/null Origins before
processing; non-browser encrypted transports remain supported. No cookie auth or
CSRF bypass/new unauthenticated task endpoint is introduced. PR URLs must match
exact approved GitHub owner/repo/pull/number; executable URLs/HTML are never used.
The panel static check has 0 FAIL; its unchanged pre-existing warnings concern
pins-only storage, existing CSP, demo URLs and cosmetic randomness.

This is not yet one combined phone→MCP→native T3→mock-GitHub acceptance run.
Native Home tests use real unmodified T3/srt/bwrap/Git plus an account-free fake
Codex; publication tests use real bare Git and a mock GitHub API. Plugin tests use
fake Home/vault data. No real account or live daemon/settings are touched.
Obsolete VM/microVM setup/session-server startup cleanup is deliberately deferred:
that path still serves legacy callers and should be removed in its own slice.

Home shutdown cancels and joins every watcher (including Cursor's independent
project-context watchers) before returning or removing project servers. Watcher
generation ownership prevents an older cancelled watcher from deleting a newer
watch. The regression test also repeats the damaged-copy stop case under the
race detector, which exposed the earlier write-after-shutdown cleanup race.


## VM retirement and rollout

Home startup no longer launches/sweeps Apple containers or microVMs, serves VM session sockets, or embeds the VM runner. Retired `agent-vm`, `vm-confine`, `microvm-confine`, VM sign-in and VM self-test paths refuse; there is no unsandboxed fallback. Project/role settings and legacy protocol fixtures remain readable for migration/testing only.

Native macOS sessions keep the existing sandbox-exec path; repository tasks explicitly require Linux descendant supervision. Windows and platforms without a pinned native T3 build refuse execution. Native provider credentials must be provisioned explicitly: the removed VM login wizard is not a native login mechanism. No installed Home, live daemon, settings, VM or provider login was changed during this work. See `docs/releases/t3-agents-rollout.md` for the pending rollout checklist.
