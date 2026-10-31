# Rubi Home: first-time setup of the agent path

The agent path (agent-runner.md) needs Apple's `container` tool, its service and kernel, the runner image, one shared Claude sign-in for all projects and a working proxy. This page is the button-only setup of
all of it: the user never types a command in a terminal. It is a page of the `claude-code` plugin in the Rubi
panel, backed by `agent-setup.*` operations of Rubi Home. Rubi Home has no web server of its own and gets none
for this: the page uses the panel's existing sign-in, encryption and approvals, and the operations reach Rubi
Home the way every other operation does (paired, end-to-end encrypted, `rubi-home.md`).

## Steps

One row per step. Each row shows a state, a one-line reason in plain language, and at most one or two buttons.

| # | Step | Done when | Button | Approval |
|---|------|-----------|--------|----------|
| 1 | Apple container | `container --version` reports the release Rubi Home pins, or a newer one | Install / Upgrade | strong |
| 2 | Container service | `container system status --format json` says `running` and the default kernel exists | Start | none |
| 3 | VM network (none needed) | always done, with the reason "Not needed: VMs run with no network at all" (VMs run with `--network none`; nothing is created or checked). The id `network` and its place stay so that the plugin's list of steps is unchanged | none | none |
| 4 | Runner image | the image tagged with the hash of the runner bundled in this Rubi Home exists | Build / Update | none |
| 5 | Projects | `agent_projects` has at least one folder | Add a folder / Remove | strong / none |
| 6 | Claude sign-in (one for the computer) | the shared token is stored and not past its end of life | Sign in / Sign in again / Sign out | none |
| 7 | Self-test (per project) | the last self-test passed with the shared sign-in (kept in memory only) | Run self-test | none |

"Set up everything" (`all`) runs the service (2) and image (4) steps, then 6 and 7 for the first project, in order.
It never runs the privileged step, installing the container tool (1), which has its own approval in the plugin. When
it isn't done as `all` starts, or stops being done while it runs, `all`
stops with `needs_user` "needs step N (asks for approval)" on that step. It also stops at the first step that needs
the user: a folder choice or the browser sign-in.

States: `todo` (not done), `working` (a job runs it; the row shows progress lines), `done`, `failed` (with the
reason), `needs_user` (waiting for the user: the reason says what to do). A step whose earlier step isn't done is
`todo` with the reason "Needs step N first" and no button.

## Idempotent and resumable

Rubi Home stores no setup state. Each status call probes the machine: the installed version, the service, the
image, the projects, the sign-in. A job that was running when Rubi Home stopped is simply gone; the
next status shows what it left behind, and pressing the button again continues from there (every step checks
before it acts: an installed release isn't reinstalled, an existing image
isn't rebuilt). One job runs at a time; a second press gets "busy". Self-test results live in memory.

## Operations (Rubi Home)

All are `agent-setup.*`, listed in the hello as capability `agent-setup`.

- `agent-setup.status {}` -> `{steps: [{id, title, state, reason, progress?: [line], action?, projects?: [...]}],
  job?: {step, project?, started}, login?: {url?, wants_code}}`. A project step (`projects`, `selftest`) carries
  `projects: [{id, dir, state, reason?, progress?, action?}]`; `project` in `job` is the project id. The
  sign-in (`login`) is one row for the whole computer, with no project rows, `action` and `action2` (two buttons). `action` is the one button of a row: `install`, `upgrade`, `start`,
  `build`, `add`, `check`, `login`, `login_again`, `sign_out` (step rows); `remove`, `selftest`
  (project rows); `cancel` while it works. Progress keeps the last 20 lines of the
  running job, each at most 200 characters, with anything that looks like a token removed. `login.url` is present
  only while a sign-in waits for the browser.
- `agent-setup.run {step, project?, method?, option?}` -> `{started: true}` or an error (`busy`, `needs step N
  first`, unknown step). `step` is one of `container`, `service`, `network`, `image`, `login`, `signout`, `selftest`, `all`.
  `project` is a project id (`selftest`; `all` takes the first project when it is empty; `login` ignores it). `method` is
  `claudeai` (a Claude subscription) for `login`; without it `login` only checks the sign-in (and ends `needs_user` when
  the project isn't signed in); with it, it signs in even when already signed in ("Sign in again"). `all` skips
  a signed-in project. `option` is `auto-address` for `network` only; it is accepted for older plugins and has no effect (there is no network
  and no address). Running `network` does nothing and ends `done` ("Not needed"); it never runs a network command.
- `agent-setup.login.code {project, code}` -> `{sent: true}`: writes the code and a newline to the running sign-in's
  stdin. At most 512 printable ASCII characters, no spaces; refused when no sign-in waits; the `project` argument is ignored.
- `agent-setup.login.open {}` -> `{opened: true}`: opens the sign-in link in the Mac's default browser
  (`/usr/bin/open`), only when it is an `https://` link to `claude.com`, `claude.ai`, `platform.claude.com` or
  `console.anthropic.com`.
- `agent-setup.project.add {}` -> `{added: dir}` or `{cancelled: true}`: shows the macOS folder chooser on the
  Mac and adds the chosen folder to `agent_projects` (refused: the home folder itself or a folder holding it,
  its private folders such as `.ssh` and `Library`, `/`, system folders, a folder that is or holds Rubi Home's
  directory, a folder already added, more than 20 projects).
- `agent-setup.project.remove {dir}` -> `{removed: true}`; refused while a job works on that project.
- `agent-setup.cancel {}` stops the running job (a sign-in, a build) -> `{stopped: bool}`.

Only the plugin calls these, and only from its page's buttons (the user's own request) or from the execute step
of an approval. Bots get no tool for them.

## Privilege path (step 1)

Rubi Home pins releases in code: version, download URL on `github.com/apple/container/releases`, size and
SHA-256 of `container-<version>-installer-signed.pkg`, and Apple's Developer ID team (`UPBK2H6LZM`, the team
that signs the installed `container` binary). Install:

1. Download into `<helper dir>/setup/` (0700) over HTTPS from `github.com` (redirects only to
   `objects.githubusercontent.com` / `release-assets.githubusercontent.com`), at most the pinned size, then check
   size and SHA-256. A mismatch deletes the file and fails the step.
2. `pkgutil --check-signature` must report "signed by a developer certificate issued by Apple for distribution",
   "Notarization: trusted by the Apple notary service" and a leaf certificate (1.) "Developer ID Installer: ...
   (UPBK2H6LZM)"; `spctl --assess --type install -vv` must say `accepted`, `source=Notarized Developer ID` and an
   `origin=` Developer ID Installer of the same team.
3. Run `osascript` with `do shell script ... with administrator privileges`: macOS shows its own password or
   Touch ID dialog on the Mac. The page never has a password field and Rubi Home never runs `sudo`. The root
   script copies the package into a fresh root-owned directory, checks the pinned SHA-256 again there (so a
   same-user process can't swap the file after step 1), checks the signature again, then runs
   `/usr/sbin/installer -pkg <copy> -target /` and removes the copy. The script's only inputs are the pinned hash
   and a path inside Rubi Home's directory, passed as quoted arguments.
4. Cancel in the dialog -> `needs_user` "The administrator dialog was cancelled". Upgrade is the same with a
   newer pinned release; Rubi Home never downgrades.

Because the plugin asks for a strong approval before it calls `run {step: container}`, a Bot can't start an
installation, and the user confirms twice: in the panel (passkey) and on the Mac (administrator dialog).

## Other steps

- **Service.** `container system start --enable-kernel-install` (no prompt), then checks the status again.
- **Network.** Nothing to do (R13): VMs are started with `container run --network none`, so there is no host-only
  network to create or to check, and the proxy and the auth gateway need no address. The step keeps its id and its
  position and reports `done` with the reason "Not needed: VMs run with no network at all. They reach the proxy and the
  Claude gateway through Rubi Home." The settings `agent_network`, `agent_proxy_listen` and `agent_proxy_vm_host` are
  obsolete and ignored. Rubi Home never creates, changes or deletes a container network.
- **Image.** The runner's sources are embedded in Rubi Home (`runner/` package, `go:embed`). The step writes them
  to `<helper dir>/runner-build/<hash>` and builds `rubi-agent-runner:<hash12>` with the bundled Dockerfile (base
  image pinned by digest; a builder stage installs the npm packages from the lockfile with `--ignore-scripts` and
  compiles the runner). Progress lines come from `container build --progress plain`. The VM uses that tag, so
  an updated Rubi Home builds its new runner once and older images stay until the user removes them.
- **Sign-in.** One for the whole computer, see "Shared sign-in" below. `run {step: login, method: claudeai}` starts
  the sign-in container; the page shows "Open the sign-in page" (`login.open`) and the link as text, and a
  "Paste the code" prompt that sends `login.code`. The sign-in waits at most 15 minutes; cancel or the limit
  removes the container. `run {step: signout}` deletes the stored token for every project. `run {step: login}`
  without a method only checks.
- **Self-test.** Home runs it itself, against its own agent manager, and needs no plugin. It takes the manager's
  slot (refused while a session runs or waits) and, while it exists, the manager denies every `perm.ask`,
  `cred.ask` and `net.ask` of that project internally: the question is never queued for the plugin, so no
  `decide` can answer it (or be mistaken for the answer), and the self-test's events stay with it. In the project's VM: the isolation probe (the runner's user writes `/work/.rubi`; the VM has no route and
  no interface but `lo`; direct connections to a public address and to four gateway addresses fail with `ENETUNREACH`;
  name lookups fail; an allowed host is reachable through the proxy on `127.0.0.1:3128`, a host not on the list is
  refused), then one small turn asking for a Bash command that the self-test denies through `perm.ask`. Pass needs
  every check plus a `result` after the deny. The prompt and answers are fixed; it never runs the user's prompt.

## Shared sign-in

All projects (and later roles) use one Claude sign-in. The boundary: **no project VM ever holds the token.** It
stays in Rubi Home on the Mac; each project start gets an auth gateway that adds it to the model requests. Only
the credential is shared, never the rest of the config directory: `CLAUDE_CONFIG_DIR` stays `/work/.rubi/claude`
in each project (skills, settings, history, MCP).

- **Credential.** A long-lived token from `claude setup-token`: a Claude subscription, one year, scope
  `user:inference` only, no refresh (so no refresh race between projects). The setup-token command needs a
  terminal, so the image carries a helper, `/usr/local/bin/rubi-claude-token` (`runner/container/`, embedded with
  the runner), that runs it under `script` with a wide pty, strips the terminal escapes and tells Home on stdout
  only `URL <link>`, `CODE?`, `SAVED` or `FAILED <reason>`. It writes the token atomically (mode 0600) to
  `/rubi-auth/token/oauth-token` with a non-secret `oauth-token.meta` (`created`, `expires`, `method`). It never
  prints the token, and drops any output line with `sk-ant`.
- **Storage.** `<helper dir>/agent-auth/` on the Mac (config `agent_auth_dir`; 0700, made by Home; the token in
  `agent-auth/token/oauth-token`, 0600, as the Mac user). Only two things touch it: the short-lived **sign-in
  container**, which mounts it read-write at `/rubi-auth` (bind mount; an Apple container volume is a block
  device and can't be attached to two VMs) and is destroyed when the sign-in ends, and Home's **auth gateway**.
  No project VM mounts it. No project folder may be, contain or sit inside it or the helper's directory.
- **Sign-in container.** No project mounted, no network (`--network none`; it reaches the proxy through the same `rubi-netpipe` pipe as a project VM), and a proxy started by Home that allows only
  `claude.com`, `claude.ai`, `platform.claude.com`, `console.anthropic.com` and `api.anthropic.com`. It is removed
  (with the helper, claude and the proxy) when the sign-in ends, is cancelled or times out; a leftover of a
  crash (named `rubi-agent-auth`, labelled `rubi.role=auth`) is removed when Rubi Home starts.
- **Auth gateway.** Every project start runs its own: a plain HTTP reverse proxy served on a stream listener of the VM's
  pipe (no socket on the Mac), alive as long as that runner. The runner gets
  `ANTHROPIC_BASE_URL=http://127.0.0.1:3129`, `CLAUDE_CODE_OAUTH_TOKEN=<per-start secret>` (random, useful
  only at this gateway and only while this runner lives) and `NO_PROXY=127.0.0.1` (claude would otherwise
  send even this plain-HTTP traffic to the CONNECT proxy). The gateway:
  - requires `Authorization: Bearer <per-start secret>` (constant-time compare) and refuses anything else (401);
  - serves only `POST /v1/messages` and `POST /v1/messages/count_tokens` (query `beta=true` or none); every other
    path or method is refused (404/405), see the spike;
  - forwards only an allow-list of headers (content type, accept, `anthropic-version`, `anthropic-beta`,
    `anthropic-dangerous-direct-browser-access`, `user-agent`, `x-app`, `x-claude-code-session-id`,
    `x-stainless-*`): every client credential (`Authorization`, `x-api-key`, cookies, proxy headers) is dropped;
    it sets `Authorization: Bearer <real token>` and adds the OAuth beta if it is missing;
  - sends to `https://api.anthropic.com` only, never follows a redirect (the token goes nowhere else), reads the
    token file at each request (a sign-in or sign-out applies at once) and answers 401 "Rubi Home isn't signed in
    to Claude" when there is no token, which claude shows as its error;
  - streams the answer (server-sent events) chunk by chunk, and never logs a header, a body or a token (it logs
    the reason of a refusal, each reason at most once per 10 seconds with a count of the rest);
  - bounds what a guest can hold: at most 64 requests in flight (more get 429 with `Retry-After`), 64 KiB of
    headers, 10 s for the headers, 5 minutes for a request to arrive with its body, 64 MiB of body, a 60-second
    deadline for each chunk written back (a guest that stops reading is cut off and the upstream stream with it),
    a 2-minute wait for the upstream's headers and a 5-minute stall limit on the upstream's stream;
  - passes on every `anthropic-beta` line (joined) plus the OAuth beta: whichever beta the agent asks for is
    forwarded, by design; the gateway injects a credential, it is no policy on the API's features.
  The listener is a `netpipe.Listener`, not a socket, so there is no address to bind and nothing to refuse (the socket form,
  which tests use, still refuses a wildcard address or a name). The upstream is fixed to `https://api.anthropic.com` in code: the override used by
  tests is an unexported field, set only by the package's own tests or by the build-tagged hook of the live check
  (`-tags gatewaytest`); no configuration can reach it.
  Requests to any other host never get the token: they go through the CONNECT proxy untouched, and
  `api.anthropic.com` is no longer on its allow-list.
- **Runner.** It holds no token and reads no token file. It passes the base URL, the secret and `NO_PROXY` to the
  SDK child, and drops `ANTHROPIC_API_KEY` when an OAuth token is set (Claude Code prefers a key over a token, which
  would take requests around the gateway). As defense in depth it replaces anything shaped like `sk-ant-…` in every
  event, answer and question before it leaves the VM, and Home does it again before the plugin sees an item.
- **Status.** Home never reads the token for the page. It stats the token file (a plain file, size above zero:
  signed in) and reads the metadata file, which has no secret in it; the answer is reused for a minute. The row
  says "Signed in until <date>", "expired on <date>" or "Not signed in".
- **Sign out** removes the token and the metadata files; the gateways refuse the next request. It cannot revoke the
  token: for a full revoke, also remove it in the Claude account settings at claude.ai.
- **Console accounts.** Not supported with the gateway: it adds a subscription token; a console (per token
  billing) account's `claude auth login` in a project is not used, because the runner always has the per-start
  secret as its OAuth token.

**What this protects, and what it doesn't.** A project VM, and anything running in it (a tool call, a hostile
repository, a package's install script), never sees the token: not in a file, an environment, a process
listing or a tool result (the live check greps the VM for it). The token cannot be copied into `/work`, a commit
or a push, and a stolen per-start secret is useless outside that gateway and after that runner ends. What a VM can
still do is **spend the subscription through the gateway while its session runs**: any code in the VM can send
model requests with the per-start secret, up to the subscription's own rate limits. Each tool call is still
approved by the user, and the secret dies with the runner. The token is on the Mac's disk in Rubi Home's directory
(0600); any process of the Mac user can read it, as with Claude Code's own login file.

**Spike (what claude sends).** Run with the real bundled `claude` 2.1.314 (Agent SDK 0.3.314, Node 26) against a
recording local server, `ANTHROPIC_BASE_URL=http://127.0.0.1:<port>`, `CLAUDE_CODE_OAUTH_TOKEN=<placeholder>`, no
`ANTHROPIC_API_KEY`; `runner/test/support/oauth-traffic.mjs` repeats it. With the image's environment
(`DISABLE_TELEMETRY`, `DISABLE_ERROR_REPORTING`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`):

- All model traffic goes to the base URL: `POST /v1/messages?beta=true` (the query, a title request in the same
  session, the SDK's model list needed no request at all). No other path, and no connection to any fixed host was
  attempted (a refusing CONNECT proxy saw nothing).
- Auth is `Authorization: Bearer <token>` only (no `x-api-key`), and claude adds the `oauth-2025-04-20` beta to
  `anthropic-beta` itself because the token is an OAuth token. Other headers: `anthropic-version: 2023-06-01`,
  `anthropic-dangerous-direct-browser-access`, `x-app: cli`, `x-claude-code-session-id`, `user-agent:
  claude-cli/2.1.314 (external, sdk-ts, agent-sdk/0.3.314)`, `x-stainless-*`, `content-type`, `accept`.
- A placeholder token passes: claude does no local format check, and reports `tokenSource:
  CLAUDE_CODE_OAUTH_TOKEN`. The account probe (`initializationResult`) works without a request; the usage probe
  returns `rate_limits_available: false` for it.
- Without the image's variables claude also calls fixed hosts that ignore the base URL: `POST
  https://api.anthropic.com/api/event_logging/v2/batch` (telemetry), a bootstrap fetch and an MCP-registry fetch.
  Those are off with the variables the image sets, and would be refused by the proxy list anyway.
- **WebFetch** (checked with the integration harness: scripted API, a recording CONNECT proxy that refuses
  everything, `NO_PROXY` for the base URL, the image's environment): by default it first asks
  `api.anthropic.com` about the domain (`/api/web/domain_info`, "Domain blocklist check") with a direct connection,
  outside the base URL; behind the VM's proxy, which does not allow that host, the check fails and WebFetch
  refuses the fetch (`DomainCheckFailedError`). The runner therefore sets the flag-layer setting
  `skipWebFetchPreflight: true`; the integration test then sees no connection to any Anthropic host and only the
  fetch itself (`example.com:443`) at the proxy. `api.anthropic.com` is never put back on the CONNECT list.
- With `HTTPS_PROXY` set, claude sends even a plain-HTTP base URL through that proxy; `NO_PROXY=127.0.0.1`
  makes it go direct (to the gateway on the VM's own loopback).

## Secrets

The sign-in code, Claude's credentials and the run and proxy tokens never appear in progress lines, events,
logs, errors or URLs. Progress lines pass through a filter that drops lines with `sk-ant`, `Bearer`, `token`,
`code=`, `refresh` (any case) and long base64-like words. The sign-in link is shown (it carries a PKCE challenge
and state, not a secret) but not logged. The code goes from the panel's prompt to the plugin op, from the plugin
to Rubi Home in an encrypted call, and to the VM's stdin; the plugin neither stores nor audits it (the audit says
"sign-in code sent"). The token itself never passes through Home, the plugin or the panel: it goes from
Anthropic to the helper in the sign-in container and into the sign-in directory; afterwards only the gateway reads it.

## Failure modes

| Failure | Shown as |
|---|---|
| No network to GitHub, wrong size or hash, bad signature, not notarized | step 1 `failed`, file deleted |
| Administrator dialog cancelled | step 1 `needs_user` |
| Service doesn't start in 60 s | step 2 `failed` with the last lines of `container system status` |
| (no row: the network step cannot fail; VMs have no network) | step 3 `done`, "Not needed" |
| Build fails | step 4 `failed` with the last progress lines |
| Folder chooser cancelled, or folder refused | step 5 unchanged, reason says why |
| Sign-in: link never appears, 15 minutes pass, `claude` exits non-zero | step 6 `failed` |
| Self-test: any check fails | step 7 `failed` naming the check |
| Rubi Home restarted mid-job | the job is gone; the row shows what the probe finds |
