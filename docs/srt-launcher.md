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

### Repository task copies (groundwork, not yet phone-to-PR)

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
turn is **not** a trusted finished repository task: durable finalization,
stop-vs-publish serialization, vault-backed exact-branch push, PR creation,
task-specific panel cards and cleanup remain separate work. Records/copies are
retained; do not use this groundwork as an unattended publishing service.
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
  ./internal/home/agent/t3vm ./internal/t3 ./cmd/rubi-agent-exec -count=1
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
