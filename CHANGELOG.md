# Changelog

## Unreleased

- **Rubi Home: button-only setup of the agent path (`agent-setup.*`, capability `agent-setup`).** Home probes the
  machine on every status call (Apple's `container` tool at the pinned release, its service and kernel, the
  host-only network, the runner image, the projects, each project's Claude sign-in, a self-test) and runs one
  step at a time as a job with a progress list (20 lines, secrets dropped). Operations: `status`, `run`,
  `login.code`, `login.open`, `project.add`, `project.remove`, `cancel`. The container tool is installed from a
  release pinned in code (URL on github.com, size, SHA-256, Apple's Developer ID team): HTTPS download with
  checked redirects, size and hash, `pkgutil` and `spctl` checks, then the package is copied by a fixed root
  script into a fresh directory, re-checked and installed behind macOS's own administrator dialog; Home never
  runs `sudo` and never downgrades. The sign-in runs `claude auth login` in the project's VM with pipes; the
  link is shown only when it is an https link to a Claude or Anthropic sign-in host, the code goes straight to
  the process's stdin. The self-test holds a slot of its project (no other session of it starts meanwhile) and the manager itself denies every question of that
  project, so no plugin decision is involved. The runner's sources are now embedded in the binary: the image is
  tagged `rubi-agent-runner:<hash12>`, built from `<helper dir>/runner-build/<hash>` (multi-stage Dockerfile that
  compiles the runner with `npm ci --ignore-scripts`) when `agent_runner_dir` is not set.
- **Rubi Home: Agent SDK sessions run per project, not one at a time.** The single slot ("one top-level query on
  the computer") is now a slot per running top-level session: `agent_max_sessions_per_project` (default 1, at
  least 1) per project and `agent_max_sessions` (default 16; 0 = no cap) for the whole computer, on top of the per-Bot limits of the usage modes below. Subagents and forks
  inside a session take no slot (their number is capped separately, below). A start (or a send to a session without a slot) over a limit waits in the
  disk queue, first in, first out, except that an item waiting on a full project or a Bot at its limit doesn't hold up
  one behind it that can run (items of one project and Bot keep their order); each queued item carries a plain `reason`. Every slot keeps the old rules
  (busy while a turn or a background subagent runs; freed on idle, close, runner exit or crash; the 30 minute
  idle interrupt, per slot). `claude-agent.status` has `slots` and `limits` (and still `slot`, the first busy
  one, for older plugins); `claude-agent.projects` rows add `sessions_running`, and `slot_busy` means the
  project's slots are full.
- **Rubi Home and runner: usage modes per Bot, and a cap on subagents.** Each Bot has a mode, `maximum` (no cap),
  `medium` (the default: 4 sessions, 4 subagents per session) or `light` (1 session, 2 subagents per session); its
  limits apply to the sessions it started only. `agent_default_mode` is for new Bots and for the sessions the user
  starts from the panel; `agent_bot_modes` holds each Bot's; `agent_max_sessions_per_project`, `agent_max_sessions`
  (now the cap on the whole computer, default 16) and the new `agent_max_subagents_per_session` override. Only the
  user changes a mode: `claude-agent.usage_mode {mode, bot | default | all}` is for the plugin's panel and has no Bot
  tool. A change applies to new starts; nothing running is killed. `claude-agent.start` and `send` take `bot`;
  `status` reports `usage` with each Bot's mode and live counters. The runner takes `max_subagents` in
  `session.start`/`send` and makes the `Agent`/`Task` gate wait (event `waiting`, count in `subagents`) while a session
  has its cap running, with a longer SDK hook timeout; after 6 h of waiting the call is denied with a reason.

- **Rubi Home: `claude-agent.*` operations and the Mac side of the Agent SDK runner (milestone M2).** A new Home
  capability, `claude-agent` (shown on the install screen as running Claude agents in a virtual machine on
  that computer, deciding each tool call and allowing git pushes). Operations: `projects`, `sessions`,
  `messages`, `start`, `send`, `rename`, `fork`, `interrupt`, `close`, `mode`, `next`, `decide`, `status`.
  Behind them (`internal/home/agent`): a `VM` interface with an Apple `container` implementation (argv-tested
  here, not run) and a fake for tests; the runner client with a per-start `run_token` handshake; one top-level
  query per slot (background subagents keep it busy; limits per project and in total, see above), with a disk queue
  (`agent-queue.json`, 0600, 24 h) for starts and sends that find no free slot; tool-call questions from the runner
  reach the plugin through `next` and wait for `decide`: no answer in 280 s, or no `next` poll for 15 s, is a
  denial; an allow-list CONNECT proxy (no TLS decryption) and a git credential helper (reads always, a push
  only after the plugin approves). Configuration: `agent_projects`, `agent_git_hosts`, `agent_proxy_listen`,
  `agent_proxy_vm_host`, `agent_runner_dir`, `agent_allowed_hosts`, `agent_no_default_forges`,
  `agent_allow_bypass`. The proxy takes its hosts only from this configuration and from approvals (`net.ask`),
  never from a project's files; refuses non-public addresses; and needs a per-start proxy password. Nothing starts a VM until a project is configured and an
  operation uses it.
- **Plugin pages: drill-down, chat section.** A page action may carry `open {..}` (show the page the plugin returns for `view` with those args; the page's `up` is the way back), a section may have `actions`, an item `warn`, and a section of `kind: "chat"` shows a message list (roles, sender, "untrusted text" label) with an input that sends the typed text to a plugin op. Text only, clipped, no new connections. Foreign chat lines (a sender field, role sister, or the plugin's untrusted flag) are always marked untrusted; authors that pose as the user or Rubi become "Bot"; lists tolerate null and non-array data and are capped. Used by the Agent SDK pages of the claude-code plugin (milestone M6).

## v0.13.0 (2026-10-31)

- **claude-code plugin: names for Claude Code sessions (`claude_code_rename`).** Claude Code has no documented
  way for another process to rename a session (`/rename`, `--name` and the picker work from inside it; the
  `~/.claude/sessions` and desktop-store files are undocumented), so a name is an alias the plugin keeps in its
  encrypted state, per computer and session; Claude Code itself is not changed. 1 to 64 plain characters (no
  invisible or control characters, not like an id), unique per computer ignoring case. `claude_code_sessions`
  shows `name` beside `title`, `claude_code_bind` accepts `name` instead of `session_id` (unknown or ambiguous
  names are clear errors). A Bot renames the session it is bound to without approval (audited), a session inside
  a trusted project it is bound in without approval (audited with the `pt_…` id), any other with a locked tap
  approval; the plugin's panel page lists sessions with Name / Rename / Clear name and works directly. Names
  survive restarts and are dropped after the session has been missing for 30 days from a whole list. Core:
  `claude-sessions.list` adds `truncated: true` (and `total`) when the list was cut at 200, a store reached its cap, or
  the scan was partial or old, so a plugin doesn't read a missing session as an ended one. A plugin page's `view`
  action may carry `prompt {arg, label, value}`: the panel asks in a box it draws, titled "<plugin> asks:", label as
  text (200 characters), refusing labels that mention a password, passkey, PIN or Rubi, and passes the answer as
  `args[arg]`. Plugin repo, branch `citri/session-rename`; plan in `docs/plans/session-rename.md`.
- **Rubi Home: `rubi-home claude-send`, a documented way for a local script to message a Claude Code session.**
  `printf '%s' "$text" | rubi-home claude-send --name "Remote control" --from Citri --wait-reply 300` (or
  `--session ID`) queues the message in Rubi Home's inbox, where the waiter hook (`asyncRewake` Stop hook, in the
  managed settings) wakes an idle session with it, and prints the session's answer. It replaces writing to the
  undocumented `messagingSocketPath` socket of Claude Code, which stays only as the caller's fallback. The
  session is told the message is from a local process, not the user or a Bot, and grants no permissions; the
  answer goes to a small local reply box (`--get-reply ID`, 10 minutes), never to a Bot. A new socket line
  `RubiSend` with its own slots; an older Rubi Home gives "too old". The claude-code plugin ignores anything
  addressed to a local sender. Plan in `docs/plans/documented-messaging.md`, example in
  `docs/examples/tell-session.sh`.
- **claude-code plugin: "trust this project until I revoke it" (project trust without an end time).** A fourth
  option on run approvals, "Allow and trust the project <root> until I revoke it", and an `until` parameter
  ("0400", the default, or "revoked") on `claude_code_trust_project` (still a strong, locked approval; the
  approval names the mode). Such a `pt_…` has no end time and no count limit: it stays on across days and
  across restarts of Rubi Home, the plugin and Rubi until it is revoked in the panel or with
  `claude_code_mandates`, and while Rubi is locked the helper denies as always (the allow is still a
  `claude.decide` through Rubi). The approval text, the panel and the Bot's messages say "UNTIL REVOKED — no end
  time" and the risk (sessions that skip permission prompts, a Bot that can open sessions in the project without
  asking). Same command check, branch rule and always-ask list as the 04:00 trust; each allow is still counted
  and audited (no cap for this mode, so it can't end by surprise). It is never pruned: the 20-trust cap evicts
  the oldest timed trust, and refuses a new grant when all 20 are unlimited. A new grant for the same computer
  and root replaces the old one of either mode (audited with both modes). `claude_code_mandates` and the panel
  show when a project trust was granted ("3 d 4 h ago") and how often it was used. Plugin repo, branch
  `citri/trust-unlimited`; plan in `docs/plans/trust-unlimited.md`. No change to Rubi Home or Rubi.
- **Rubi Home: a worktree's `commondir` no longer widens the project; sessions report resolved paths.** A `.git`
  file only makes the main repository the project when its `gitdir` is `<common>/worktrees/<name>` (git's own
  layout); a spoofed `commondir` naming an ancestor, or one inside a `.git` directory, is ignored and the
  folder with the `.git` file is the project. `claude-sessions.list` adds `real_cwd` and `real_project_root`
  (symlinks resolved; omitted when they can't be) so a plugin can check a trust against the real place.
- **claude-code plugin: one trust per project ("trust this project until 04:00").** A run approval gets a
  third option, "Allow and trust this project until <weekday date time zone>", and a Bot can ask for the same
  with `claude_code_trust_project` (strong, locked approval, for the project of the session it is bound to). Until
  the next 04:00 (at most 24 hours; the time is in the zone of the machine Rubi runs on, because Rubi Home does
  not report its computer's zone, and the approval says so) every Claude Code session on that computer inside
  the project's root runs plain, listed commands without asking (the 2-hour trust's check, each allow audited
  with the `pt_…` id, at most 2000), and switching to or creating a git branch is allowed only for names matching
  the trust's pattern (default `citri/*`). Push and every remote git command, `--force`, sudo, the network,
  installs, `git tag`, writes outside the root and branches outside the pattern still ask. The Bot bound to a
  session of the project can start sessions in it and bind to its other sessions without a new approval
  (audited). One live trust per computer and root (a new one replaces the old); it lives in the plugin's saved
  state, so it survives a restart of Rubi Home, the plugin or Rubi; revoke it in the panel or with
  `claude_code_mandates`. No change to Rubi Home. Plugin repo, branch `citri/project-trust`; plan in
  `docs/plans/project-trust.md`.
- **Rubi Home logs every hook it handles.** One line per hook: `claude-hook <event> session=<first 8 of the id>
  outcome=<what happened> <ms>ms` (for example `delivered 2`, `replies 1 asks 1 blocked`, `refused
  (protocol)`, `slots full`; a permission request shows its request id and the first 8 characters of its
  digest; the waiter logs attach, wake and close; a sister tool logs its name and ok/error). Counts, ids and
  digests only, never a command or message text; written after the work, with no lock held. Checked while
  adding it: `claude-sessions.send` already doesn't wait for the scan, and event hooks already have their
  own deadline, slots and tickets, so no change was needed there.
- **Rubi Home: `claude-start.create` starts a Claude Code session when none exists.** A new Home capability,
  `claude-start` (shown on the install screen as starting a new session with a first message, where
  sessions that skip permission prompts let Claude run any command; `claude-sessions` no longer says
  anything about it, and a plugin without `claude-start` can't call it). Opens the Claude desktop app's
  `claude://code/new?folder=...&q=...` link with `/usr/bin/open` (a `Launcher` seam; the headless `claude -p`
  route is left out: it needs a signed-in CLI and leaves no live session) and waits up to 45 s for the new
  session's SessionStart hook (`source: startup`, a new id, the same directory; when its transcript shows a
  first message it must carry the request's random reference code), answering its id or a note when none
  came (or several did). The directory must be the real path (a path through a symlink is refused) of an
  existing project the registry has seen, or inside `claude_start_dirs` (new key in `config.json`, default
  empty), and never `~/.claude`, `~/.ssh`, `~/Library`, `~/.local`, `~/.npm`, `~/.oh-my-zsh`, `~/.zsh*`, the
  home directory itself, `/var`, `/opt`, `/Applications`, `/Volumes` itself or a system tree. The prompt (at
  most 2000 characters, 40 lines, untrusted) is rejected, never cleaned, when it holds invisible characters
  (format, private use, tag, control, C1) or more than two blank lines in a row, so the approved text is the
  launched text; it reaches the session framed as a message from the sister. 3 launches an hour per computer,
  one at a time. The new session runs with the desktop app's permission settings: with bypassPermissions
  Claude runs commands without asking and Rubi approvals don't apply. Not yet checked by hand against the
  real app (see `docs/plans/claude-task.md`).
- **Rubi Home: sister tools inside Claude Code, installed with zero setup.** `rubi-home claude-mcp` is a
  stdio MCP server (reads no config) with `rubi_sisters`, `rubi_ask`, `rubi_reply` and `rubi_inbox_status`;
  asks and replies reach the Bot through the existing outbox, so a bound Bot sees `claude_ask` and
  `claude_reply` (with `in_reply_to`) as before. Calls travel on the hook socket as a new `RubiTool` line
  with their own 16 slots and a deadline; it is never an approval or an event, and an older helper
  answers `{}` (the tools then report that Rubi Home is too old). The calling session is
  decided by the kernel's view, not by the line: the helper takes the socket peer's pid (`LOCAL_PEERPID` /
  `SO_PEERCRED`) right after Accept, anchors on the session's own `claude` process (the first ancestor named
  `claude` within 6 levels, else the direct parent; never shells or the app above it), lets the first session
  that claims a process keep it, and attributes a tool call to the session that holds the caller's anchor
  (none: an error; an OS without peer pids refuses the tools). Any process started inside a session's
  `claude` tree counts as that session and could end its claim and re-claim as another id (as `/clear`
  does); the first claim holds while its session is active, a SessionEnd releases it only from the claiming
  process, and a claim silent for 10 minutes can move. A same-user process outside every tree gets nothing
  unless it forges hook lines for a session id no `claude` has claimed (as it always could). The CLI process
  must be named `claude`, else the tools are refused. The tools give no approval power. Tool asks and replies are rate limited per session (3 a minute) and
  overall (20 a minute), a message can be answered once. New op `claude-sessions.bound` (the plugin tells the helper which Bot is bound
  to a session); bound Bots and recent deliveries are kept in `claude-queue.json` (new optional keys,
  version still 1). The tools add no trust: nothing they return approves a command, and they act for the caller's own session
  (an ask from an unbound session may still go to its project's Bot, as for a text block).
  `rubi-home install-hook --with-claude-plugin` installs the embedded marketplace `rubi` (plugin
  `rubi-sisters`: tools, skill, `.mcp.json`) to `/Library/Application Support/Rubi Home/claude-plugin` in the
  same administrator step, with the files inline in the root script (no TOCTOU), a sha256 check per file and
  an atomic directory swap that keeps the old folder until the rename worked. Managed settings register it
  (`extraKnownMarketplaces` + `enabledPlugins`; not `managed-mcp.json`).
- **Rubi Home: the Claude Code hook binary reports its version, and `install-hook` updates it.** Every hook
  line (approvals, events, the `--wait` waiter) carries `rubi_hook_version`, `rubi_hook_protocol` (1) and
  `rubi_hook_path`. The helper serves a hook without them (v0.12, marked stale), refuses a protocol it
  doesn't speak (a denial that names `rubi-home install-hook`; events and the waiter get `{}`), logs a
  stale or mismatched hook once, and `claude-sessions.list` adds `hook` (`version`, `path`, `protocol`,
  `stale`, `seen_at`). `rubi-home status` runs `/usr/local/bin/rubi-home version` (3 s limit; `--hook PATH`
  or `RUBI_HOOK_BINARY`) and shows its version, sha256, ownership and "stale"; the helper warns in its log
  at start and `install-home.sh` warns after an upgrade. New `rubi-home install-hook [--dry-run] [--to PATH]
  [--sudo]` installs the running binary to the root-owned path with an administrator prompt; root itself
  copies the file once, checks its sha256 against the hash shown, and moves it into place atomically (refusing
  targets whose directories others can write), and the result is verified afterwards. `rubi-home version` no longer opens the helper's directory.
- **Rubi Home: idle Claude Code sessions are woken, and senders see what became of a message.** A second
  Stop hook, `rubi-home claude-hook --wait` (`asyncRewake: true`), stays connected while a session is
  idle; a message for the session wakes it (exit 2, the messages on stderr) and the helper counts it
  delivered only when the waiter acknowledges, otherwise the messages go back to the inbox. One waiter per
  session, 64 slots of their own, an older helper just answers `{}`. Inbox, outbox and owed replies are
  kept in `claude-queue.json` across restarts (24 hours), written off the hooks' path. The outbox
  carries `status` entries (`queued`, `delivered`, `answered`, `expired`) and
  `claude-sessions.send` answers `session_state` and `waiter`.
- **Rubi Home: a hung disk never blocks Claude Code hooks or approvals.** Hooks only touch the
  hook-reported sessions under their own lock; the session stores are scanned in the background, the
  desktop store and the transcripts each in its own goroutine with a deadline, and a list answers from
  the last scan while a newer one runs (`sources.scanned_at` and `sources.stale: true` say so). A hung
  desktop store no longer hides the transcript sessions: it is listed as its last full read (the list
  says stale), and a stuck read is reused, not repeated.
- **Approvals and events have separate slots on the socket.** Reading the first line has its own cap
  (64); then approvals take one of 32 slots (denied when full) and events one of 16 (an empty reply
  when full, never a denial). Over the reading cap an approval is denied with the reason ("too many hook
  requests are arriving at once") instead of "an answer that couldn't be read".
- **`claude-sessions.send` doesn't wait for the session scan.** It checks the id against the
  hook-reported sessions and the last scan, waits at most 0.3 seconds for a first scan, and answers
  "session list not ready" for an id it can't judge yet instead of waiting up to 6 seconds. The cap on
  concurrent session listings is gone (there are only two kinds of listing, each shared).
- **A stuck Rubi Home operation answers "still busy".** While a call of an op runs past its deadline, a
  new call of the same op gets "Rubi Home is still busy with earlier calls" at once (at most 8 per op),
  so one stuck call blocks that op only. Stop reads the transcript with a 2-second limit and keeps the
  reply it owes for a later Stop; a late event handler leaves the inbox alone.
- **A pasted routine key is checked like a Bot's.** For a Grok Bot routine address, the key pasted on the
  approval screen or given to `agent.add` must be `crsr_` plus letters, digits, `_` or `-`, at least 20
  characters, and not a redaction placeholder; one helper serves both paths. Errors say what is wrong and
  never repeat the key.
- **`plugins` takes plain ids.** `rubi_agent_connect` and `rubi_access_request` accept
  `plugins: ["gmail"]`, `[{id: "gmail"}]` or a mix; the tool schema declares `anyOf [string, object]`.
- **`rubi_access` task length counts characters, not bytes** (500), so a Cyrillic or CJK task fits. Errors:
  "task is empty: say in one sentence what you'll do" and "task is N characters, at most 500".
- **`rubi_ack` is the same for every Bot.** Events were acked once for everyone, so of several Bots sharing a
  broadcast (`plugin.update_available`, `approval.decided`, ...) only the first got `acked: true`, and its ack
  hid the event from the others. Acks are now kept per Bot (case-insensitive name): an event acked by one Bot
  stays in the others' `rubi_events` until they ack it. `rubi_ack` now also returns `reason`: `acked`,
  `already_acked`, `not_visible` (exists, but not addressed to you), `not_stored` or `unknown_or_expired`.
  `acked` stays, and is true only for a new ack (`already_acked` is false, as before).
- **No ack for messages that aren't stored.** `access.granted` told the Bot to `rubi_ack` an event that
  was never in the list; its `next_step` no longer mentions ack (same for `agent.code`), and acking such an id
  gives `not_stored`.
- Events saved by an older version with the old global flag count as acked by every Bot, so nothing old
  reappears. An event is pruned after an hour once every Bot that could see it at the time of the last ack has acked it, otherwise after a day (a Bot added later doesn't see an event that was already settled). After a downgrade, events acked by only some Bots show as unacked again for them (harmless).
- **Skill `rubi-claude-pair` for working in pairs with Claude Code.** How Grok Bot and a Claude Code session
  linked through the claude-code plugin split a project (modes balance / full GrokBot / full Claude, briefs,
  reports, which permissions carry over); the Grok Bot part is in `skills/grok-bot/rubi-claude-pair`, the Claude
  part ships in the embedded `rubi-sisters` plugin.

## v0.12.2 (2026-10-30)

- **Rubi Home: a hanging disk no longer silences `claude-sessions.list`.** The session list reads each
  project's `.git` files; on a disk that stops answering (an external drive that hangs) the read blocks in
  the kernel, held the registry lock, and every later list (and `.send`) waited forever with nothing in the
  log, until the caller's MCP timeout. Git info is now read in its own goroutine, waited for at most 2
  seconds per scan; sessions whose git info doesn't come in time are listed with `local:<dir>` as their
  project, and the log says which directory didn't answer. The read keeps going in the background (one per
  directory, at most 16) and fills the cache when it ends.
- **Rubi Home answers by the deadline.** `claude-sessions.list` gives up after 20 seconds with an error
  ("a project's disk may not be responding"), and any operation still running when the relay answer's 60
  seconds end now gets an error answer instead of none, which also frees its slot (8 at a time): before,
  8 stuck requests made the helper drop every later request, approvals included.

## v0.12.1 (2026-10-29)

- **Webhook key from a file Rubi reads (`key_file`).** v0.12.0's recipe passed the routine key to
  `rubi_agent_connect` as `"$file:/workspace/.rubi-hook-key"`; the Grok Bot runtime redacts known secrets
  inside that substitution, so Rubi stored `[REDACTED]` and the routine refused the key on `webhook.test`.
  `rubi_agent_connect` now takes `key_file: <path>`: the daemon, which serves the MCP tools (`rubi mcp`
  only relays stdio to its socket), reads the file and deletes it, success or failure. The path must be
  absolute and clean, under `/workspace`, `/tmp` or `$TMPDIR` (links in its directory resolved), a regular
  file that is not a symlink, owned by Rubi's user, mode 0600 or stricter, at most 4 KB; whitespace is
  trimmed. `key` and `key_file` together are refused.
- **A Bot's key is checked before the approval.** A redaction placeholder (`[REDACTED]`, only `*` or `•`), fewer
  than 20 characters, whitespace inside or a literal `$file:` is refused with a hint to use `key_file`; for
  a Grok Bot routine address the key must be `crsr_` plus letters, digits, `_` or `-`. A whole
  `Authorization: Bearer <key>` header or `Bearer <key>` is still accepted and normalized. Keys the user
  pastes or sets in settings are unchanged.
- **Recipe.** `rubi_status` (`webhook.how`, `webhook.note`), the MCP instructions, the
  `rubi_agent_connect` description and the Grok Bot skill now say `umask 077; printenv
  RUBI_WEBHOOK_KEY_<NAME> > /workspace/.rubi-hook-key` and `key_file`, with no `$file:` substitution.

## v0.12.0 (2026-10-29)

> **Behaviour change / Изменение поведения.**
> EN: events that rubi-mailkit or rubi-telegram address to a named Bot with no registered webhook (or to
> an unknown Bot) are now dropped and audited as `event.dropped`, instead of going to all subscribers.
> RU: события, которые rubi-mailkit или rubi-telegram адресуют названному Боту без зарегистрированного
> вебхука (или неизвестному Боту), теперь отбрасываются и записываются в аудит как `event.dropped`, а не
> уходят всем подписчикам.

- **Strict delivery of addressed events (breaking for plugins that relied on the fallback).** An event a
  plugin addresses to one Bot (`EmitTo`) goes to that Bot's webhook only. If no Bot of that name is
  connected with a webhook, Rubi drops the event, records `event.dropped` (plugin, type, name) in the audit
  log and returns an error to the plugin; it never falls back to the subscribers or the administrator. In
  the event list an addressed event is its Bot's alone (the administrator no longer sees other Bots'
  addressed events).
- **Verified caller for plugins.** A plugin tool call may name the Bot (`rubi_agent`) with a proof only its
  webhook receives: `rubi_agent_code` (the `agent_code` of its latest webhook, or from `rubi_verify`) or
  its `rubi_access` code for that plugin. Rubi checks it, strips the code and passes the plugin
  `rubi_verified_agent`, the Bot's registered name. A caller-supplied `rubi_verified_agent` is always
  stripped. A wrong `rubi_agent_code` or an unknown name fails the call.
- **Claude Code sessions in Rubi Home.** The `claude-hook` also reports SessionStart, UserPromptSubmit,
  Stop and SessionEnd; Rubi Home keeps a read-only registry of sessions (Claude desktop app store,
  transcripts in `~/.claude/projects`, project from the origin remote, main vs sub-session) and a per-
  session inbox: queued messages reach a session through UserPromptSubmit context or by blocking Stop
  (at most 5 times in a row); the turn's answer and ```` ```rubi-ask ```` blocks go back to the sender.
  The helper never starts or resumes a session. This works in bypassPermissions sessions too (those hooks
  still run there; only permission requests don't).
- **New Rubi Home capability `claude-sessions`**, separate from `claude` and shown on the install screen:
  `claude-sessions.list`, `claude-sessions.send` and `claude-sessions.next` (replies and asks). `claude`
  keeps only permission requests (`claude.next`, `claude.decide`); `claude.next` no longer carries session
  messages, and polling `claude-sessions.next` doesn't count as Rubi being there to approve.

- **Pending approvals in the panel.** Settings now shows a "Waiting for you" list of every request still
  waiting for a tap or passkey, and each one opens the usual approval screen. New panel ops are
  `approval.list` (no previews) and `approval.open` (mints a ticket for one request). Both need a full
  `settings` ticket.
- **Plugin approvals reach the user.** An approval a plugin submits outside a tool call (for example a
  Claude Code permission request) used to wake no one unless a Bot followed that plugin. Rubi now
  emits `rubi` `approval.requested` to the administrator, and the webhook carries a fresh approval link and
  a `next_step` to send it. This is skipped when the tool result or the plugin's own event already
  brought a Bot the link.
- **Connecting a Bot takes one paste.** The panel's Connect page reads the routine's whole Webhook
  section (or a curl line) in any order, says what is missing, refuses non-routine hosts and goes straight
  to the passkey. Pasted keys must be one printable token (no header injection), and URLs with userinfo are
  refused. Addresses that aren't a Grok Bot routine still work but the approval carries a warning. After
  saving, the panel waits up to 20 s for the routine's answer to a test event and explains a failure in
  plain words (wrong key, routine off, no routine, unreachable) without showing the address or key. The
  Bot guidance in `rubi_status`, the MCP instructions, `rubi_link` (new `send_with` hint) and the Bot skill
  all ask for one message with both links.
- **Bots propose their own connection.** New MCP tool `rubi_agent_connect(name, url?, key?, events?,
  plugins?, reason?)`: a Bot proposes its routine webhook; Rubi keeps the key sealed in memory (never on
  disk, in logs or on screen) and changes nothing until the user approves with a passkey. The approval
  shows the Bot, the webhook's host and path and the events, and highlights when it replaces an existing
  webhook (applying fails if that webhook changed meanwhile). On approval Rubi applies it and sends a test
  event; deny, expiry, lock and restart discard the key.
- **Bots ask for plugin access.** New MCP tool `rubi_access_request(agent, plugins: [{id, accounts?}],
  reason)` asks to be assigned plugin accounts for good. Each account is one item the user can leave out;
  a Bot's requests (connect and access) are merged into one pending approval with one link. Rate-limited
  (10 per Bot, 30 overall per hour).
- **Store search.** `rubi_store` takes optional `query` and `capability`; it searches the signed catalog
  and returns per match the latest version, `min_rubi`, installed/connected, what the plugin needs and the
  next step. Without arguments the output is unchanged.
- **Connecting a Grok Bot: key through a secret field.** The main path: the user gives the routine URL in
  chat and the key in a masked secret field (env `RUBI_WEBHOOK_KEY_<NAME>`); the Bot passes it to
  `rubi_agent_connect` as `"$file:<path>"` from a 600 file it shreds right after, and the user confirms
  with the passkey. A Bot may propose only Grok Bot routine webhooks; others are refused (the user's own
  settings still accept them, with a warning).
- **Fallback: paste on the approval screen.** `url` and `key` are optional. Without a key the approval screen asks the user to paste the routine's Webhook section
  (prefilled with the Bot's `url`) and confirm with the passkey in the same step. Approvals can now carry
  typed inputs (`approval.get` lists them; `approval.challenge` and `approval.decide` take `inputs`). The
  passkey or password proof covers a digest of the pasted values, so they can't be swapped after signing;
  they are never stored or echoed. A key passed with a trailing newline (read from a file) is trimmed, and
  a literal `$file:` reference is refused. Bot requests appear in "Waiting for you" like any other.
- Panel: `replaces` is highlighted on approval screens; Bot requests have their own labels.
