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

### Upstream providers

Inspected T3 source: `pingdotgg/t3code` revision `e11c2fdc` (Nightly).

| Driver | T3 launch setting | Rubi-managed route |
| --- | --- | --- |
| Claude Agent | `binaryPath` | shim; role's Claude config directory |
| Codex | `binaryPath`, `setupMode: existing` | shim; isolated CODEX_HOME |
| Grok, Fx, Kiro, Pi, Muse, Antigravity | `binaryPath` | shim |
| ACP Registry | `source: local`, `commandPath` | shim; installed local command only |
| Cursor | in-process SDK; legacy `binaryPath` is ignored | **not exposed**; isolated environment or execution hook needed |
| OpenCode | local HTTP/SSE server or remote server URL | **not exposed**; no safe host-to-srt transport bridge yet |
| Unknown future driver | unknown | resolver refuses |

No Rubi-managed Cursor/OpenCode instance is created: a disabled SDK instance
could otherwise be enabled without passing any shim. Personal instances still
work normally and are **not** a Rubi confinement promise. This is a route-level
implementation, not proof of real-provider compatibility. No fork of T3 is used.

**Cursor/OpenCode follow-up:** investigation confirmed that these “instances” are
settings in one shared T3 process, not project-isolated servers. Cursor requires
a whole-process boundary; OpenCode requires an authenticated bridge across srt's
private loopback. Both still refuse to launch, with provider-specific errors.
See [execution-boundary findings and options](design/t3-srt-provider-boundaries.md)
for the supported per-project environment direction, OpenCode v1/v2 authentication
differences, bridge design and explicitly fake-provider test evidence. No actual
Cursor/OpenCode session is connected by this follow-up.

### Installation metadata (not installed by this slice)

`Config.AgentSandbox` (`agent_sandbox`) supplies an absolute `agent_exec`, `srt`,
trusted `search_path`, narrow `runtime_read` trees, and `providers[driver]` entries
with absolute `cli`, `runtime_read` and `allowed_domains`. `agent_exec` defaults to
the sibling `rubi-agent-exec` when writing shims; all other dependencies must be
explicitly provisioned. These fields contain installation paths/domains, never
credentials. This slice does not modify any live config or install dependencies.

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
SSH sockets or Node options are inherited. Provider authentication must use its
explicit login directory. SearchPath is trusted installation input, never the
agent's PATH. A missing srt, node, rg, Linux bwrap/socat or macOS sandbox-exec
fails closed. Unsupported platforms and no-sandbox mode fail closed as well.
No UI or approval bypass is added. Exit codes and byte streams are preserved;
this version uses a child process, not Unix process replacement.

## Verification

```
go vet ./...
go test ./... -count=1
go test -race ./internal/agentsandbox ./internal/home ./internal/home/agent/t3vm -count=1
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

## Follow-up integration decisions

- Install and pin `@anthropic-ai/sandbox-runtime` (spike tested 0.0.81), Node and
  OS dependencies in Rubi Home releases for Linux and macOS. Keep the install
  tree separate from projects and login/tmp folders. No auto-install is added.
- Validate authenticated real CLIs and each provider's login/state conventions.
  Per-role login directories are not automatically populated from shared credentials.
  Environment-based API keys and T3 environment overrides are deliberately not inherited.
  This slice uses explicit installation domain lists; the legacy dynamic `net.ask`,
  shared sign-in and git credential/hook transport are not yet migrated to srt.
- The setup wizard's VM probes/sign-in flow remain legacy diagnostics; they do not
  validate this srt route. Its progress now says so. Migrate that flow separately.
- Attachments/worktrees outside the configured project tree remain inaccessible.
  Add trusted resolution for them only with an explicit isolation design.
- Exercise a real provider CLI and macOS sandbox-exec on disposable fixtures.
- Define signal forwarding/process-group cleanup before daemon supervision.
- Choose the isolated per-project T3 environment design (or wait for an upstream
  execution hook) for Cursor, and implement/authenticate OpenCode's bridge before
  exposing either as a Rubi-managed provider. See the linked findings.
