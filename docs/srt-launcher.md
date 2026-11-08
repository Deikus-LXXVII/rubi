# Provider CLI sandbox launcher (slice 3H building block)

`cmd/rubi-agent-exec` accepts `--config /absolute/trusted/session.json --`
followed by provider protocol arguments. It does not contact a live daemon or
T3 server. The session resolver must construct `agentsandbox.Launch` and either
call `Run`, or save its JSON to the trusted config path. T3 binaryPath wiring and
session/project resolution are not implemented here.

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
and SHELL are supplied explicitly. No API keys, proxy variables, loader hooks,
SSH sockets or Node options are inherited. Provider authentication must use its
explicit login directory. SearchPath is trusted installation input, never the
agent's PATH. A missing srt, node, rg, Linux bwrap/socat or macOS sandbox-exec
fails closed. Unsupported platforms and no-sandbox mode fail closed as well.
No UI or approval bypass is added. Exit codes and byte streams are preserved;
this version uses a child process, not Unix process replacement.

## Verification

```
go vet ./internal/agentsandbox ./cmd/rubi-agent-exec
go test -race ./internal/agentsandbox ./cmd/rubi-agent-exec
PATH=/workspace/srt-spike/bin:/workspace/srt-spike/node_modules/.bin:$PATH \
  RUBI_SRT_NETWORK_TEST=1 go test -v ./internal/agentsandbox \
  -run TestSRTIntegration -count=1
```

The integration test skips only when srt is absent or the OS is unsupported;
missing dependencies otherwise fail. Without RUBI_SRT_NETWORK_TEST it checks
local filesystem, environment, Unix sockets (Linux), streams and exit code only.
All fixtures use a disposable HOME. No real provider credentials are used.

## Follow-up integration decisions

- Install and pin `@anthropic-ai/sandbox-runtime` (spike tested 0.0.81), Node and
  OS dependencies in Rubi Home releases for Linux and macOS. Keep the install
  tree separate from projects and login/tmp folders. No auto-install is added.
- Wire the trusted session resolver and generated per-provider binaryPath config
  into T3. Do not accept policy, runtime paths or config from provider arguments.
- Exercise a real provider CLI and macOS sandbox-exec on disposable fixtures.
- Define signal forwarding/process-group cleanup before daemon supervision.
- The requested `citri/t3-cloud-agents` plan branch was not available from
  origin during implementation; reconcile this building block with the plan
  before wiring it in.
