# Rubi Home: first-time setup of the agent path

The agent path (agent-runner.md) needs Apple's `container` tool, its service and kernel, a host-only network,
the runner image, a Claude sign-in inside the VM and a working proxy. This page is the button-only setup of
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
| 6 | Claude sign-in (per project) | `claude auth status` in that project's VM says logged in | Sign in / Sign in again | none |
| 7 | Self-test (per project) | the last self-test passed (kept in memory only) | Run self-test | none |

"Set up everything" runs steps 1 to 4, then 6 and 7 for the first project, in order. It stops at the first step
that needs the user: an approval, the macOS administrator dialog, a folder choice, or the browser sign-in.

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
  job?: {step, project?, started}, login?: {project, url, wants_code}}`. Progress keeps the last 20 lines of the
  running job, each at most 200 characters, with anything that looks like a token removed. `login.url` is present
  only while a sign-in waits for the browser.
- `agent-setup.run {step, project?}` -> `{started: true}` or an error (`busy`, `needs step N first`, unknown step).
  `step` is one of `container`, `service`, `network`, `image`, `login`, `selftest`, `all`.
- `agent-setup.login.code {project, code}` -> `{sent: true}`: writes the code and a newline to the running sign-in's
  stdin. At most 512 printable ASCII characters, no spaces; refused when no sign-in for that project waits.
- `agent-setup.login.open {project}` -> `{opened: true}`: opens the sign-in link in the Mac's default browser
  (`/usr/bin/open`), only when it is an `https://` link to `claude.com`, `claude.ai`, `platform.claude.com` or
  `console.anthropic.com`.
- `agent-setup.project.add {}` -> `{added: dir}` or `{cancelled: true}`: shows the macOS folder chooser on the
  Mac and adds the chosen folder to `agent_projects` (refused: the home folder itself, `/`, system folders, a
  folder inside Rubi Home's directory, more than 20 projects). `agent-setup.project.remove {dir}` removes one.
- `agent-setup.cancel {}` stops the running job (a sign-in, a build).

Only the plugin calls these, and only from its page's buttons (the user's own request) or from the execute step
of an approval. Bots get no tool for them.

## Privilege path (step 1)

Rubi Home pins releases in code: version, download URL on `github.com/apple/container/releases`, size and
SHA-256 of `container-<version>-installer-signed.pkg`, and Apple's Developer ID team (`UPBK2H6LZM`, the team
that signs the installed `container` binary). Install:

1. Download into `<helper dir>/setup/` (0700) over HTTPS from `github.com` (redirects only to
   `objects.githubusercontent.com` / `release-assets.githubusercontent.com`), at most the pinned size, then check
   size and SHA-256. A mismatch deletes the file and fails the step.
2. `pkgutil --check-signature` must report a Developer ID Installer certificate of team `UPBK2H6LZM`, trusted by
   Apple, and the package must be notarized (`spctl --assess --type install`).
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
- **Sign-in.** Starts the project's VM (as a session start would, with the proxy), then runs the bundled
  `claude auth login` in it with stdin and stdout as pipes (no terminal). Rubi Home reads stdout until the
  sign-in link appears, keeps it in memory, and the page shows "Open the sign-in page" (`login.open`) and the link
  as text. If Claude asks for a code, the page's "Paste the code" button (a panel prompt) sends it with
  `login.code`; Rubi Home writes it to stdin and keeps no copy. The job ends when `claude` exits; the row then
  probes `claude auth status`. The sign-in waits at most 15 minutes. Two sign-in methods, both Anthropic's own
  flow inside the VM: "Anthropic Console account" (`--console`, billed per token) and "Claude subscription"
  (`--claudeai`); the page notes that a subscription is for the user's own personal use under Anthropic's terms.
  The credentials stay in the project's `.rubi/claude` (the VM's config directory); Rubi Home never reads them.
- **Self-test.** In the project's VM: the isolation probe (the runner's user writes `/work/.rubi`, direct
  connections and name lookups fail, an allowed host is reachable through the proxy, a host not on the list is
  refused), then one small turn asking for a Bash command that the self-test denies through `perm.ask`. Pass needs
  every check plus a `result` after the deny. The prompt and answers are fixed; it never runs the user's prompt.

## Secrets

The sign-in code, Claude's credentials and the run and proxy tokens never appear in progress lines, events,
logs, errors or URLs. Progress lines pass through a filter that drops lines with `sk-ant`, `Bearer`, `token`,
`code=`, `refresh` (any case) and long base64-like words. The sign-in link is shown (it carries a PKCE challenge
and state, not a secret) but not logged. The code goes from the panel's prompt to the plugin op, from the plugin
to Rubi Home in an encrypted call, and to the VM's stdin; the plugin neither stores nor audits it (the audit says
"sign-in code sent").

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
