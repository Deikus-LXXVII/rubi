# Cursor and OpenCode: execution boundaries, not additional CLI shims

> Historical investigation at c1057e0. The project-server and authenticated-pipe
> mechanisms are now implemented; see [current launcher documentation](../srt-launcher.md)
> for the supported routes, verification evidence and remaining limitations.

Status: **finding / design only; neither provider is connected**. Both remain
absent from Rubi-managed instance settings, and the launcher refuses them even
if their CLIs are installed. Personal T3 providers are unchanged. No T3 code,
live daemon, live settings, or provider credential was changed/read for this work.

## Source scope

Reviewed `/tmp/t3c`, `pingdotgg/t3code` revision
`e11c2fdca7d1e67b0a9ada3bdbd87314efa78557`, including:

- `packages/provider-cursor/src/settings.ts`, `server/driver.ts`,
  `server/adapter.ts`, `server/CursorAgentSdk.ts`, and `server/CursorSdk.ts`;
- `packages/provider-opencode/src/server/{driver,OpenCodeRuntime,OpenCodeServerOwner}.ts`
  and `server/v2/{OpenCode2Server,OpenCode2Client}.ts`;
- `packages/shared/src/AgentScope.ts` and `docs/internals/remote.md`.

[Browse this exact upstream revision](https://github.com/pingdotgg/t3code/tree/e11c2fdca7d1e67b0a9ada3bdbd87314efa78557).
The dependency is `@cursor/sdk` **1.2.0**.
[Its exact local-agent option types](https://cdn.jsdelivr.net/npm/@cursor/sdk@1.2.0/dist/cjs/options.d.ts)
and [the public SDK documentation](https://cursor.com/docs/sdk/typescript.md)
help distinguish SDK capabilities from what T3 actually exposes.

## Cursor: where confinement has to start

T3 requires `@cursor/sdk` in its own Node process. `CursorAgentSdk.ts` calls
`Agent.create` / `Agent.resume` with `runtime: "local"`; local workspace
prewarming also happens there. There is **no cursor-agent subprocess** in this
execution path. `binaryPath` and `apiEndpoint` survive as hidden V1 legacy
settings, not execution overrides. `AgentScope.wrap` only wraps spawned commands;
it cannot confine file reads/edits performed by the already-running SDK host.

The SDK does have `local.resourceProviderOptions` executor overrides and a
`local.fs` alternative. It is incorrect to say that the SDK has no such options.
However, T3's `makeCursorAgentOptions` does not supply them, and no T3 setting
or provider-worker process hook was found for injecting them. The structural
executor interfaces are unpublished; subagents need their own inheritance
configuration. Virtual `fs` also drops Shell, ReadLints, SemanticSearch, host git,
hooks, skills and other local features. It is not a full-function substitute.
The SDK's own enabled sandbox is not a project-only read boundary.

Consequently, replacing PATH, adding a binary shim, wrapping only shell hooks,
or enabling the SDK's shell sandbox would leave native file tools outside srt.
A Node preload/module substitution would alter T3 at runtime, not constitute a
supported connection, and is not proposed.

### Whole-server option: supported in principle, not a settings-only change

Rubi currently has **one T3 server and paired client**, not one process per
project. `internal/home/t3home.go` owns singular `srv` / `cl`; configuration has
one `T3Port` / `T3BaseDir`. `t3vm/instances.go` and `sandbox_instances.go` create
logical provider settings in that server. Wrapping that shared server for A
would remove B's folders; granting A+B would let Cursor A read B directly.
Neither is acceptable.

An **isolated T3 environment per project** is the viable non-fork direction.
Upstream's remote-environment design explicitly makes an environment own its
projects, provider execution and persisted state. Peer MCP links are not a
transparent way to relocate an existing provider session to another worker.
This option therefore requires, together:

1. A trusted Rubi manager resolving the configured project before starting a
   separate server with only that project's folders, its own state/login/tmp,
   port, pairing and lifecycle. Supervisor settings/shims and other projects'
   state must remain outside its write/read grants.
2. Project-keyed server/client routing throughout session start, send, resume,
   fork, approvals, project listing, status, restart/adoption and teardown. Old
   shared-environment thread IDs/state need an explicit migration policy.
3. A controlled HTTP/WebSocket ingress boundary to each sandboxed environment;
   its private loopback is not reachable by the existing host client. Preserve
   MCP callbacks, terminal streams, approvals, auth/login, updates and every
   provider's functionality, rather than forwarding only chat requests.
4. Revisiting the existing per-CLI shim route: nested srt/user namespaces may
   fail, and the trusted Rubi Home resolver must not become visible/writable
   merely to make nested launchers work. The outer project boundary could
   instead confine all providers in that environment.

This does **not** establish that a T3 fork is necessary. It establishes that
there is no safe per-provider drop-in for Cursor in the current shared-server
architecture. A project-server fleet is a larger architecture/lifecycle change,
not implemented by this findings commit. It would also affect all other providers
in those environments and must be tested as such before claiming compatibility.

## OpenCode: a bridge is feasible, but not a binaryPath-only route

T3's v1 runtime spawns `serve --hostname=... --port=...`, reads a listening URL,
then connects over HTTP/SSE. It uses HTTP Basic (`opencode` username). V1 uses a
configured `serverPassword` or `OPENCODE_SERVER_PASSWORD`; **it does not generate
a password by default**. V2 differs: `OpenCode2Server` creates a random 32-byte
password for a spawned server, passes it via `OPENCODE_PASSWORD`, verifies
`server.info`, and also uses HTTP Basic. Do not generalize v1's empty-password
behavior to v2. Both have a supported external `serverUrl` path. Both lifecycle
and CLI maintenance/probes matter, not just `serve`.

On Linux, srt's bwrap network namespace has its own loopback. A fake server can
bind exactly the same numeric address/port as a host server without collision.
T3 on the host reaches the host server, not the sandbox server. Another project's
sandbox reaches neither. `allowLocalBinding` is ignored on Linux; Unix-socket
path allowlists are also not enforced there. `--share-net`, blanket localhost/
Unix-socket permission, or disabling network isolation is not a safe bridge.
On macOS the localhost allowance has different, broader semantics; the Linux
conclusion must not be claimed as a tested macOS guarantee.

### Minimal bridge design to implement and verify separately

- Resolve role/project/binding through Rubi's existing trusted resolver before
  starting anything. Keep shims, settings and bridge control files outside agent
  writes. Run the real OpenCode server and a small inner relay **inside the same
  srt sandbox**, bound only to its private loopback, under that project's folder
  and network policy. Never allow a session argument to choose another policy.
- Connect the inner relay to a Rubi-owned outer bridge using inherited pipes,
  not a network escape or a globally accessible Unix socket. Multiplex fixed-
  target byte streams; do not accept arbitrary dial targets/CONNECT requests.
  Do not inherit control descriptors into provider tool subprocesses.
- Expose a host-loopback frontend only with a fresh per-project high-entropy
  authentication secret. Provision T3 through its supported `serverUrl` and
  secret-setting mechanism, not a token in argv/URLs/logs/project files. Enforce
  auth before opening inner streams and prevent another project's sandbox from
  reaching the frontend, including through the srt domain proxy. V1's default
  empty password cannot be accepted; v2's generated credential is a different
  launch contract, not permission to reuse one password across projects.
- Keep outer auth credentials outside the sandbox; if the inner server needs
  auth, use separate inner credentials. Guard host process metadata/memory too;
  same-user `/proc`/ptrace exposure cannot be waved away by a 0600 token file.
  A weaker PID/proc sandbox mode is not acceptable for cross-project secrecy.
- Preserve streaming, cancellation, backpressure, concurrent requests, SSE,
  WebSocket upgrades, headers and v1/v2 health/auth semantics. Redact all startup
  output/errors that might contain generated credentials. Rubi must own child
  groups, crash/restart cleanup, ports, timeouts and any external-server lifecycle
  T3 no longer owns. CLI probes/update commands need their own confined path.

No bridge, authentication provisioning or protocol-compatibility claim is made
here. This design is viable without changing T3; it is deliberately not replaced
by an unauthenticated toy HTTP proxy. It can be implemented independently, or
folded into the project-environment transport needed for Cursor.

## Evidence and remaining acceptance gates

`TestInProcessHostBoundarySRTProbe` uses real Node and bwrap+srt with fake direct
SDK-style file operations and a spawned shell: A can write A, cannot read the B
or HOME fixture secrets, cannot write through to those host paths, and denied
network access gets a proxy rejection. It tests the necessary **whole-process**
boundary, not the real Cursor SDK or a connected T3 session.

`TestOpenCodePrivateLoopbackBoundarySRTProbe` uses a fake Python HTTP server in
real bwrap+srt. It proves private self-health, A-only reads/writes, the identical
host/inner port distinction, B's inability to reach A directly, and a proxy
rejection when B explicitly tries the host endpoint via the srt proxy. Default
srt `NO_PROXY` bypasses the proxy for loopback, so that attempt must be explicit.
It does not test OpenCode authentication, SSE or a bridge (none is installed).

Regression tests also require refusal even when fake Cursor/OpenCode CLIs are
configured. Existing missing/reassigned-project, immutable-binding/shim and
cross-project filesystem tests remain applicable. No real cursor-agent or
opencode executable was installed; no real provider/account was exercised.

Before enabling either route: implement the chosen boundary/transport; exercise
real disposable T3 sessions and provider binaries, authentication isolation,
network denial, concurrency, full protocol functionality, lifecycle and signals.
Package/pin srt 0.0.81, Node, bwrap/socat/rg; validate macOS sandbox-exec separately.
Until then both providers fail closed; the nine existing CLI routes are unchanged.
