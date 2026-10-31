# Rubi Home: first-time setup of the agent path

The agent path (agent-runner.md) needs Apple's `container` tool, its service and kernel, a host-only network,
the runner image, one shared Claude sign-in for all projects and a working proxy. This page is the button-only setup of
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
| 3 | Host-only network | the network inspects as one network with mode `hostOnly` and a private gateway in its subnet | Create | strong |
| 4 | Runner image | the image tagged with the hash of the runner bundled in this Rubi Home exists | Build / Update | none |
| 5 | Projects | `agent_projects` has at least one folder | Add a folder / Remove | strong / none |
| 6 | Claude sign-in (one for the computer) | the shared token is stored and not past its end of life | Sign in / Sign in again / Sign out | none |
| 7 | Self-test (per project) | the last self-test passed with the shared sign-in (kept in memory only) | Run self-test | none |

"Set up everything" (`all`) runs the service (2) and image (4) steps, then 6 and 7 for the first project, in order.
It never runs the two privileged steps, installing the container tool (1) and creating the network (3): each has
its own approval in the plugin. When either isn't done as `all` starts, or stops being done while it runs, `all`
stops with `needs_user` "needs step N (asks for approval)" on that step. It also stops at the first step that needs
the user: a folder choice or the browser sign-in.

States: `todo` (not done), `working` (a job runs it; the row shows progress lines), `done`, `failed` (with the
reason), `needs_user` (waiting for the user: the reason says what to do). A step whose earlier step isn't done is
`todo` with the reason "Needs step N first" and no button.

## Idempotent and resumable

Rubi Home stores no setup state. Each status call probes the machine: the installed version, the service, the
network, the image, the projects, the sign-in. A job that was running when Rubi Home stopped is simply gone; the
next status shows what it left behind, and pressing the button again continues from there (every step checks
before it acts: an installed release isn't reinstalled, an existing network isn't recreated, an existing image
isn't rebuilt). One job runs at a time; a second press gets "busy". Self-test results live in memory.

## Operations (Rubi Home)

All are `agent-setup.*`, listed in the hello as capability `agent-setup`.

- `agent-setup.status {}` -> `{steps: [{id, title, state, reason, progress?: [line], action?, projects?: [...]}],
  job?: {step, project?, started}, login?: {url?, wants_code}}`. A project step (`projects`, `selftest`) carries
  `projects: [{id, dir, state, reason?, progress?, action?}]`; `project` in `job` is the project id. The
  sign-in (`login`) is one row for the whole computer, with no project rows, `action` and `action2` (two buttons). `action` is the one button of a row: `install`, `upgrade`, `start`, `create`,
  `use_auto_address`, `build`, `add`, `check`, `login`, `login_again`, `sign_out` (step rows); `remove`, `selftest`
  (project rows); `cancel` while it works. Progress keeps the last 20 lines of the
  running job, each at most 200 characters, with anything that looks like a token removed. `login.url` is present
  only while a sign-in waits for the browser.
- `agent-setup.run {step, project?, method?, option?}` -> `{started: true}` or an error (`busy`, `needs step N
  first`, unknown step). `step` is one of `container`, `service`, `network`, `image`, `login`, `signout`, `selftest`, `all`.
  `project` is a project id (`selftest`; `all` takes the first project when it is empty; `login` ignores it). `method` is
  `claudeai` (a Claude subscription) for `login`; without it `login` only checks the sign-in (and ends `needs_user` when
  the project isn't signed in); with it, it signs in even when already signed in ("Sign in again"). `all` skips
  a signed-in project. `option` is `auto-address` for `network`: it never creates the network (error "creating the network needs its own
  approval" when it is missing, nothing changed); it only clears, when the network exists and is host-only,
  `agent_proxy_listen` and `agent_proxy_vm_host`.
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
- **Network.** Creates `agent_network` (default `rubi-agent`) with `container network create --internal`, then
  runs the same host-only check as every VM start (`checkHostOnly`). The proxy listens on that network's
  gateway by default (agent-runner.md); when `agent_proxy_listen` or `agent_proxy_vm_host` are set and don't name
  the gateway, the row says so and offers "Use the automatic address", which clears them. A network that exists
  but isn't host-only is `failed` with "Remove the network <name> or set another agent_network"; Rubi Home never
  deletes a network.
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
  `decide` can answer it (or be mistaken for the answer), and the self-test's events stay with it. In the project's VM: the isolation probe (the runner's user writes `/work/.rubi`, direct
  connections and name lookups fail, an allowed host is reachable through the proxy, a host not on the list is
  refused), then one small turn asking for a Bash command that the self-test denies through `perm.ask`. Pass needs
  every check plus a `result` after the deny. The prompt and answers are fixed; it never runs the user's prompt.

## Shared sign-in

All projects (and later roles) use one Claude sign-in; only the credential is shared, never the rest of the
config directory: `CLAUDE_CONFIG_DIR` stays `/work/.rubi/claude` in each project (skills, settings, history,
MCP).

- **Credential.** A long-lived token from `claude setup-token`: a Claude subscription, one year, scope
  `user:inference` only, no refresh (so no refresh race between projects). The setup-token command needs a
  terminal, so the image carries a helper, `/usr/local/bin/rubi-claude-token` (`runner/container/`, embedded with
  the runner), that runs it under `script` with a wide pty, strips the terminal escapes and tells Home on stdout
  only `URL <link>`, `CODE?`, `SAVED` or `FAILED <reason>`. It writes the token atomically (mode 0600) to
  `/rubi-auth/token/oauth-token` with a non-secret `oauth-token.meta` (`created`, `expires`, `method`). It never
  prints the token, and drops any output line with `sk-ant`.
- **Storage.** A directory of Rubi Home on the Mac: `<helper dir>/agent-auth/` (config `agent_auth_dir`; 0700,
  made by Home; the token in `agent-auth/token/`). It is a bind mount, not a container volume: an Apple
  container volume is a block device and can't be attached to two running VMs, a bind mount (virtiofs) can, and
  `readonly` is enforced there. Every project VM mounts it **read-only** at `/rubi-auth`, outside `/work` and so
  outside git; only the sign-in container mounts it read-write. The token file is therefore on the Mac's disk in
  Rubi Home's directory (0600, as the Mac user), and Home never reads it. No project may be, contain or sit
  inside it or the helper's directory. A VM started before the first sign-in needs a restart to see the mount.
- **Sign-in container.** Short-lived, with no project mounted, the directory read-write, the host-only network, and
  a proxy started by Home that allows only `claude.com`, `claude.ai`, `platform.claude.com`,
  `console.anthropic.com` and `api.anthropic.com`. It is removed (with the helper, claude and the proxy) when the
  sign-in ends, is cancelled or times out.
- **Runner.** At each query start the runner reads `$RUBI_AUTH_DIR/token/oauth-token` (default `/rubi-auth`) and,
  if it is non-empty, puts it into the SDK child's environment as `CLAUDE_CODE_OAUTH_TOKEN`: only there, never
  in an event, a log (claude's own stderr is redacted) or a tool argument; the Read tool is denied on the
  directory. A sign-in or sign-out applies to the next query; `usage.probe` `logged_in` reflects it. The shared
  token wins when present.
- **Status.** Home never reads the token. It stats the token file (a plain file, size above zero: signed in) and
  reads the metadata file, which has no secret in it; the answer is reused for a minute. The row says "Signed in
  until <date>", "expired on <date>" or "Not signed in".
- **Sign out** removes the token and the metadata files. It cannot revoke the token: for a full revoke, also
  remove it in the Claude account settings at claude.ai.
- **Leftovers.** A sign-in container left by a crash (named `rubi-agent-auth`, labelled `rubi.role=auth`) is
  removed when Rubi Home starts.
- **API key.** Claude Code prefers `ANTHROPIC_API_KEY` to an OAuth token when both are set. When a shared token is
  stored, the runner does not pass `ANTHROPIC_API_KEY` to the SDK child (the token wins); without a token the key
  is passed as before.
- **Console accounts.** The wizard offers only the subscription sign-in (the simpler path). A console (per
  token billing) account can still sign in inside one project with `claude auth login --console`, whose
  credentials stay in that project's `.rubi/claude`; it is used only when no shared token is stored.

**Trade-off, stated plainly.** The token is readable by anything that runs as the agent's user in a project VM:
a tool call (Bash, `cp`, a script, a hook, a package's install script) can copy it into `/work`, into a commit, to
a push to an allowed forge, into a tool result, or send it to any host the VM may reach. A hostile repository or
prompt can therefore try to exfiltrate it to any allowed host (the proxy allows the model API, the package
registries, the forges and what the owner added or approved). Claude Code installs have the same property for
their own login. What limits the damage:

- egress only through the allow-list proxy (hosts not on the list are asked about, never silently allowed);
- the token's scope (`user:inference` only: no account, organization or billing access) and its one-year life;
- the read-only mount outside `/work` and git, so nothing writes the token by accident and a commit doesn't
  contain it unless a tool copied it;
- every tool call is approved (or denied) by the user, and the Read tool is denied on `/rubi-auth`;
- best-effort redaction: the runner replaces anything shaped like `sk-ant-…` in every event, answer and question
  before it leaves, and Home does the same before it hands items to the plugin. This does not stop a tool that
  encodes or splits the token;
- sign-out deletes the files, and revoking the token at claude.ai ends it everywhere.

Sharing it means one project's compromise reaches every project's inference access (not their files), which is
the owner's decision for one account. Per-project sign-ins remain possible (`claude auth login` in a project),
used only when no shared token is stored.

## Secrets

The sign-in code, Claude's credentials and the run and proxy tokens never appear in progress lines, events,
logs, errors or URLs. Progress lines pass through a filter that drops lines with `sk-ant`, `Bearer`, `token`,
`code=`, `refresh` (any case) and long base64-like words. The sign-in link is shown (it carries a PKCE challenge
and state, not a secret) but not logged. The code goes from the panel's prompt to the plugin op, from the plugin
to Rubi Home in an encrypted call, and to the VM's stdin; the plugin neither stores nor audits it (the audit says
"sign-in code sent"). The token itself never passes through Home, the plugin or the panel: it goes from
Anthropic to the helper in the sign-in container and into the sign-in directory.

## Failure modes

| Failure | Shown as |
|---|---|
| No network to GitHub, wrong size or hash, bad signature, not notarized | step 1 `failed`, file deleted |
| Administrator dialog cancelled | step 1 `needs_user` |
| Service doesn't start in 60 s | step 2 `failed` with the last lines of `container system status` |
| Network exists with another mode | step 3 `failed`, nothing changed |
| Build fails | step 4 `failed` with the last progress lines |
| Folder chooser cancelled, or folder refused | step 5 unchanged, reason says why |
| Sign-in: link never appears, 15 minutes pass, `claude` exits non-zero | step 6 `failed` |
| Self-test: any check fails | step 7 `failed` naming the check |
| Rubi Home restarted mid-job | the job is gone; the row shows what the probe finds |
