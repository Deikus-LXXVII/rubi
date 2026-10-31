# Rubi Home

Rubi runs on the agent's machine, far from the user's home. Some things can only be reached from the home
network: a Philips Hue Bridge, or Apple Home, which Apple opens only to apps on the user's own devices.
Rubi Home is a small helper for a computer at home (a Mac that stays on, or a Linux box) that does those
things on Rubi's request.

## Connection

- The helper is the server. It keeps a fixed routing key on the relays (Rubi Gateway and the public ones)
  and its own identity key (X25519 + Ed25519), stored in its directory (`~/Library/Application Support/Rubi
  Home` on a Mac).
- Rubi calls it with the panel protocol's encryption (`internal/e2e`, path `/home/v1/rpc`): only the helper
  can read a request, and only the helper can produce an answer Rubi accepts. Relays see ciphertext.
- Requests carry a random token. The helper keeps only a hash of each token it accepted; a request without
  a known token gets nothing. Requests are single-use and must be within 2 minutes of the helper's clock.

## Pairing

1. On the home computer: `curl -fsSL https://rubi-panel.com/install-home.sh | sh`. The installer verifies
   the release signature, starts the helper at login (LaunchAgent / systemd user service) and prints a
   pairing code: the helper's routing key, public identity and relays, plus a one-time secret valid for
   15 minutes. `rubi-home pair` prints a new one.
2. The user pastes the code into the panel (Settings > Rubi Home > Add a home computer).
3. Rubi calls `pair` with the secret and a new token, then asks the user to approve adding the computer
   (passkey or password). Removing it later also tells the helper to forget the token.

A new pairing replaces every earlier one on that computer: codes are made there by its owner, so a pairing
someone obtained from an older code ends at the next `rubi-home pair`. `rubi-home status` lists the
pairing with its time. The helper introduces itself by its model ("MacBook Pro"), not by a hostname that
often carries its owner's name.

## Watching Rubi

If the user chooses it, the helper also watches its Rubi: Rubi sends it a signed beat every minute (op
`watch.beat`, which needs no pairing token, since a locked Rubi can't read its token, but must be signed
by Rubi's watch key and carry a registration sealed to this helper). When beats stop or Rubi stays locked,
the helper wakes Rubi's administrator Bot. It watches only while this computer is on.

## What it does

The helper does only what it was built for, whatever Rubi asks:

- **Philips Hue** (`hue.discover`, `hue.pair`, `hue.request`): finds bridges (mDNS, then Signify's
  discovery service), pairs when the user presses the link button, and passes GET and PUT requests to the
  bridge's resource API (`/clip/v2/resource/…`) only. Bridge addresses must be on the home network
  (private or link-local), and no request follows a redirect. The bridge's certificate is recorded at pairing
  (its name must be the bridge id) and must match on every later connection. The application key stays in
  Rubi's vault and comes with each request. Requests to one bridge are at least 100 ms apart.
- **Shortcuts** (`shortcuts.list`, `shortcuts.run`): lists and runs only the shortcuts in one folder (Rubi,
  by default; `rubi-home folder NAME`), by identifier, with a 90-second limit and 64 KB of output. This is
  how Apple Home is reached: HomeKit is only available to App Store or Mac Catalyst apps signed with a paid
  developer account, so the user builds shortcuts with Apple Home actions instead.
- **Claude Code** (`claude.next`, `claude.decide`): a PermissionRequest hook in Claude Code's managed
  settings runs `rubi-home claude-hook`, which hands the request (tool, command, directory, session) to the
  running helper over a local socket (`claude.sock`, mode 0600, in its directory) and waits. The socket can
  only ask, never answer, and holds at most 16 open requests. Rubi takes requests with `claude.next` (a long
  poll of at most 25 seconds) and answers with `claude.decide`, which must name the request's id, session
  and the SHA-256 of its tool input, and counts once. Every other ending is a denial with a reason: no
  answer within `ClaudeWaitSeconds` (280 by default, below the hook's 300-second timeout), Rubi not polling
  (locked, offline, not paired; it counts as connected for 15 seconds after its last poll), the helper not
  running, or a malformed answer. `claude-hook` reads no configuration and writes nothing; it always prints
  a decision for Claude Code. The `claude-code` plugin turns each request into a Rubi approval (passkey or
  password) and answers only from its execute step, so only an approved action can allow a command. Its
  approvals count toward the same limit as every other approved action (30 per hour).

  **Trusting a project (plugin-side; Rubi Home needs no change).** The plugin can turn one approval into a
  trust that covers a whole project until 04:00: a run approval's third option, or the Bot's
  `claude_code_trust_project` (strong, locked). It uses what Rubi Home already reports: the project's top
  directory (`ClaudeSession.project_root` of `claude-sessions.list`) names the root, and any session whose
  request directory is inside it is covered, so no per-session state is needed in the helper. The trust is a
  record in the plugin's own encrypted state (id `pt_…`, computer, root, branch pattern, end, uses), so it
  survives a restart of Rubi Home or of the plugin, and an allow is still a `claude.decide` through Rubi: with
  Rubi locked the helper denies as before. Rubi Home does not report its computer's time zone, so the 04:00
  end is computed in the zone of the machine Rubi runs on, and the approval says so. The same trust lets the
  Bot bound to a session of the project call `claude-start.create` for a folder inside the root and bind
  to other sessions in it without a new approval (the helper's own checks of the folder apply as always). For this
  the list reports `real_cwd` and `real_project_root` (symlinks resolved), and `readGitInfo` takes a worktree's
  main repository as the project only when the `.git` file's `gitdir` is `<common>/worktrees/<name>`, so a
  spoofed `commondir` can't widen a trust's root.

  The project trust can also be granted **until revoked** (a fourth run option, or `claude_code_trust_project`
  with `until: "revoked"`): the record is flagged `unlimited`, has no end time and no count limit, and is
  never pruned (the cap of 20 evicts timed trusts only); a revoke is the only way it ends, apart from Rubi
  being locked, which stops every allow because an allow is a `claude.decide` sent through Rubi and the
  helper denies while Rubi is not polling. Nothing in Rubi Home changes. The cost is plain: the trust is a
  standing permission for plain, listed commands (builds and tests run the project's own code) in every
  session of the project, and for the Bot to open sessions in it without asking, which may skip permission
  prompts; so the approval says "UNTIL REVOKED — no end time" in capitals, the panel and
  `claude_code_mandates` show the trust's age and use count, and each allow is audited as before.

  **Naming sessions (plugin-side; Rubi Home needs no change).** Claude Code documents `/rename`, `--name` and the
  session picker, all from inside Claude Code (checked against code.claude.com/docs `cli-reference`, `commands`,
  `sessions`, `desktop`); there is no documented way for another process to set a session's name, and writing
  `~/.claude/sessions/<pid>.json` or the desktop app's store is undocumented, so Rubi writes neither. The
  `claude-code` plugin keeps the name as an **alias in its own encrypted state** (`session_names`: computer,
  session id and the desktop app's id, name, set, seen). Rules: 1 to 64 characters, trimmed, no control, format
  (zero-width, bidirectional, tag), private-use or separator characters, no special spaces, not like an id
  (`local_`, UUID, 8+ hex characters, Rubi ids, a session id of that computer), unique per computer ignoring case; only
  letters, numbers, punctuation, symbols and the ordinary space are allowed (no marks, variation selectors, fillers or
  blanks).
  `claude_code_sessions` shows `name` beside `title`; `claude_code_bind` accepts `name` for `session_id`
  (unknown, ambiguous and vanished names are errors). `claude_code_rename` needs no approval for the session the
  Bot is bound to (audited), none for a session inside a trusted project when the Bot is bound inside it
  (audited with the trust's id, as the other trust shortcuts), and a locked **tap** approval otherwise (a name
  is a reversible label; strong stays for what gives a Bot reach). The panel renames or clears any session
  directly. A Bot needs a tap to replace or clear a name the user set. A name is dropped after its session has been
  missing for 30 days from a *whole* list: `claude-sessions.list` now answers `truncated: true` (and `total` when it
  cut the list at 200) when the list may lack sessions that exist (the cut, a store at its cap of 2000 files / 200
  transcripts, a partial or old scan), and the plugin never drops a name on such a list. The panel needed one
  generic addition: a `view` action may carry `prompt` (see `plugins.md`).

  The boundary is the computer's user account: a process running as the same user can talk to the socket
  (it can only ask) or replace the helper binary, so the hook's command should be a root-owned path, and
  managed settings (which a user can't edit) should keep Claude Code from editing its own settings.

- **Claude Code sessions** (Rubi Home capability `claude-sessions`, separate from `claude`: `claude-sessions.list`, `.send`, `.next`; v0.12): the same `claude-hook`, installed for
  SessionStart, UserPromptSubmit, Stop and SessionEnd, reports each session (id, directory, transcript,
  permission mode, whether it is in a turn) and never blocks or fails Claude Code. `claude-sessions.list` merges
  that with the Claude desktop app's session store (`<user config dir>/Claude/claude-code-sessions/*/*/
  local_*.json`: title, origin directory, the session it was spawned from) and the transcripts in
  `~/.claude/projects`, all read only. A session's project is its origin remote, read from `.git/config`
  (worktrees resolve through `commondir`), so a sub-session in `.claude/worktrees/<name>` belongs to its
  project; it is a "sub" when spawned from another session or in such a worktree. Git files are read with
  a budget (2 seconds per scan, v0.12.2): a project on a disk that stops answering is listed as
  `local:<dir>` while the read finishes in the background, and the list itself gives up with an error after
  20 seconds, so a hanging disk shows as an error, not as silence. The stores are scanned in the
  background, off every lock the hooks use (unreleased): the desktop store and the transcripts are each
  read in their own goroutine with a 4-second budget, and the scan lists what came in, so a hung store
  doesn't hide the transcript sessions; a store that doesn't come in full in time is listed as its last
  full read and the scan counts as partial, and a read that never returns is reused by later scans (one
  per store). A list answers from the last scan at once while a newer one runs; `sources` then carries
  `scanned_at` and `stale: true` (also after a partial scan, and when no scan has finished yet and only
  hook-reported sessions are listed). Before the first scan a list waits for it at most 6 seconds; `claude-sessions.send` checks
  the id against the hook-reported sessions and the last scan, waits at most 0.3 seconds for a first scan,
  and answers "session list not ready" for an id it can't judge yet (no scan, or a partial one).

  The socket never lets events and approvals starve each other: reading the first line (3 seconds) has
  its own cap of 64 connections, then a PermissionRequest takes one of 32 approval slots (a denial when
  none is free) and an event one of 16 event slots (an empty reply when none is free, never a denial;
  its handler gets 5 seconds). A connection over the reading cap gets a denial naming the reason ("too
  many hook requests are arriving at once"), which an event hook reads as an empty reply. Over the relay,
  a Rubi Home operation still running past its deadline keeps its place: while one call of an op is stuck
  that way, a new call of the same op answers "Rubi Home is still busy with earlier calls" at once (at
  most 8 handlers per op run), so one stuck call blocks new calls of that op, not of others. `claude-sessions.send` queues a
  message (at most 4000 characters, 20 waiting) for a session the registry knows. UserPromptSubmit adds
  waiting messages to the prompt's context; Stop, when messages wait, blocks the stop with them as the
  reason (at most 5 times in a row), so a session in a turn picks them up without the user. The last
  answer of a turn that carried messages, read from the hook or from the transcript (only under
  `~/.claude/projects`, at most 16 KB), goes back to their senders, and ```` ```rubi-ask kind=research|audit|lookup ```` ````
  blocks in an answer (at most 3, 2000 characters each) become questions; Rubi takes both with
  `claude-sessions.next`. The helper never resumes or starts a session. Which Bot may talk to which session is
  decided in Rubi (the claude-code plugin's bindings).

  An idle session (stopped, waiting for the user) fires no hooks, so since this change it can be woken by
  a waiter: `rubi-home claude-hook --wait [--wait-limit SECONDS]`, a second Stop hook with
  `asyncRewake: true` and a long timeout (3600 s in the example; the waiter gives up 30 s earlier, exit 0).
  It sends the Stop input to the socket as event `RubiWait` (an older helper answers `{}` and the waiter
  exits 0) and stays connected, reading no config and never printing to stdout. It has its own 64 slots
  (full: `{}`), one waiter per session (a newer one closes the older). When a message is queued for the
  session, or waits when the waiter connects, the helper takes the messages out of the inbox and sends
  `{"wake":"<framed messages>","ids":[...]}`; the waiter writes the text to stderr, answers `ok` and exits
  2, which wakes the session with the text as a system reminder. The waiter answers `ok` before it writes
  the text, so a waiter that can't answer writes nothing. Two small windows remain: a waiter stalled for
  more than 3 seconds (or a helper stopping) can still write the text after the messages went back to the
  inbox, so they arrive twice; and one that dies between `ok` and the write has them counted as delivered
  without Claude seeing them.
  Without `ok` within 3 seconds the messages go back to the front of the inbox in order and that waiter
  has exited: the session is not re-armed and waits for its next prompt or Stop. With `ok` they count as
  delivered and their senders get the turn's answer at the next Stop, as for the other deliveries.

  **A local script can message a session: `rubi-home claude-send`** (B9, the documented path). Claude Code
  documents [cross-session messaging](https://code.claude.com/docs/en/cross-session-messaging.md) only for
  Claude to Claude (the SendMessage tool), says nothing about the line format of a session's inbox socket, and
  its [channels](https://code.claude.com/docs/en/channels.md) don't work in the desktop app. What
  [hooks](https://code.claude.com/docs/en/hooks.md) do document is that an `asyncRewake` command hook that
  exits 2 wakes Claude even when the session is idle. So the path is Rubi Home's own inbox plus the waiter:
  `printf '%s' "$text" | rubi-home claude-send (--session ID | --name TITLE) --from NAME [--wait-reply SECONDS]`
  (text on stdin, at most 4000 characters, rejected, never stripped, when it holds invisible or control
  characters, like `claude-sessions.create`; it reads no config, so it works under any user shell). It sends the
  socket line `RubiSend` (stamped like a hook line, its own 8 slots, a 5-second deadline; ops `send` and
  `get_reply`; the session id is in `target_session_id`, so a helper without this line takes it for no hook
  and answers `{}`, which the command reports as "Rubi Home is too old"). The helper checks it as it checks
  `claude-sessions.send` (known session, not ended, at most 20 waiting), queues it with the sender
  `local:<NAME>` and kicks the session's waiter. The session is told it is a message from a local process on
  this computer, not the user and not a Bot, that the name is its own and unverified, and that it grants no
  permissions. `--name` matches the desktop app's title (case-insensitive, live sessions only; two matches are an
  error that lists the ids; `sessionNames` in `localsend.go` is where Rubi aliases plug in). The reply (the
  turn's final answer, or an explicit `rubi_reply`) is not sent to Rubi: for a `local:` sender it goes to a
  small reply box in the helper (by message id, 10 minutes, 50 entries, memory only), and the outbox for Bots
  never sees it (the claude-code plugin also drops anything addressed to `local:`; a Bot's own message can't use
  that prefix). `--wait-reply N` waits up to N seconds and prints the answer (exit 3 when none came);
  `rubi-home claude-send --get-reply ID` fetches it later. When a Bot's and a local sender's messages arrive in one turn, the framing says so and the final answer goes to every sender in the turn (the Bot through the outbox, the local one into the reply box). A script should name its target with `--session`: titles can be influenced by whoever can write to a session, so `--name` is for interactive use. `get_reply` long-polls have their own 4 slots, so waiting clients cannot block sends. `RubiSend` lines are checked for protocol like hook lines but are not recorded as the hook binary in `claude-sessions.list`'s `hook` info. Only the same OS user can do this (the socket is 0600).
  **The waiter is required to wake an idle session**: the second `Stop` hook with `asyncRewake: true` must be in
  the managed settings (see `examples/managed-settings.json` of the plugin); without it a message to an idle
  session waits for the next prompt, and `claude-send` says so. The old way, a script writing an undocumented
  `{"type":"user",...}` line into the `messagingSocketPath` of `~/.claude/sessions/<pid>.json`
  (`~/.feature/tell-rubi.py`), may break with any Claude Code update; nothing in Rubi or its plugins writes to that
  socket, and it remains only as the caller's own fallback. A short example is
  `docs/examples/tell-session.sh`.

  **The helper's log has one line per hook it handles**, so a silent session can be traced:
  `claude-hook <event> session=<first 8 characters of the id> outcome=<what happened> <ms>ms`. Outcomes:
  `nothing`, `delivered 2` (UserPromptSubmit), `replies 1 asks 1 blocked` (Stop), `refused (protocol)`,
  `slots full`, `late` (an event that missed its deadline); `PermissionRequest` says `allowed`/`denied` with
  `req=<request id> digest=<first 8 of the digest>`; `RubiWait` logs `attach`, `woke N`, `close` and `gone`;
  `RubiTool:<tool>` logs `ok`, `error` or `timeout`. The line has counts, ids and digests only: never a
  command, a message, a reply or a path. It is written after the work with no lock held.
  Waiters only wake sessions that are idle; if a waiter is attached while the session is busy (it arrived
  before the Stop hook marked it idle, or the Stop continued), the messages stay queued for the turn's
  hooks, and a Stop that ends the turn wakes the waiter if messages wait. UserPromptSubmit, SessionEnd, a
  blocking Stop, the waiter's limit and the helper stopping all end the waiter (exit 0). Inbox, outbox and
  the replies a turn owes (message and reply text included, so the file is as private as the helper's
  other files) are kept in `<helper dir>/claude-queue.json` (0600, atomic, written by a
  background saver within a second of a change and when the helper stops; a message waiting more than 24
  hours expires, an outbox entry that old is dropped; an unreadable file is moved to `.bad`), so a restart
  loses nothing; the saver takes its snapshot under the lock and writes outside it, so a slow disk never
  holds up a hook. Senders learn what became of a message from `kind: "status"` entries in
  `claude-sessions.next` (`id` is the message's, `to` its sender, `state` one of `queued`, `delivered`,
  `answered`, `expired`); when the outbox is full the oldest status goes first, and a status
  never pushes out a reply or an ask. `claude-sessions.send` also answers `session_state` (idle, busy,
  ended, unknown) and `waiter` (whether an idle session will be woken now).

  **The hook binary's version.** Claude Code runs a root-owned copy of `rubi-home` (`/usr/local/bin/rubi-home`;
  a user-writable path would let any process swap in a binary that allows everything), which changes only
  when the user re-installs it, so it can fall behind the helper. Every line a hook sends (all event kinds,
  PermissionRequest and the `--wait` waiter) carries `rubi_hook_version`, `rubi_hook_protocol` (the constant
  `ClaudeHookProtocol`, 1) and `rubi_hook_path` (`os.Executable`, best effort), spliced into the JSON text so
  `tool_input` and its digest are untouched; the fields never reach what Rubi shows. The helper reads them
  before anything else. No protocol: a legacy hook (v0.12), served and marked stale. A protocol outside the
  supported range: refused. PermissionRequest is denied with "the Claude Code hook binary speaks protocol N,
  Rubi Home speaks M: run `~/.rubi-home/bin/rubi-home install-hook`; denied."; events and `RubiWait` get
  `{}`. Stale means the hook's version is older than the helper's (numeric semver; `dev` or unparseable is
  never stale). The last hook seen (`version`, `path`, `protocol`, `at`) lives under its own small lock, never
  held with the others and with no I/O under it; a mismatch or stale hook is logged once per distinct
  (version, path, protocol). `claude-sessions.list` adds `hook` (`version`, `path`, `protocol`, `stale`,
  `refused`, `legacy`, `seen_at`, `home_version`) once a hook has been seen. `rubi-home status` is another process,
  so it runs `<hook> version` (default `/usr/local/bin/rubi-home`, `--hook PATH` or `RUBI_HOOK_BINARY`; 3 s,
  the process group is killed) and prints version, sha256, whether the file is root-owned, not a symlink and
  not group/world-writable, and "stale". The helper does the same check once at start (it has no
  self-updater: upgrades are an `install-home.sh` run, which restarts it) and logs a warning; the installer
  warns too.

  The hook identity is advisory: the hook reports it itself, so a process that can reach the socket can claim
  any version. Nothing trusts it for a decision; it only warns and refuses, and what Bots are shown of it is
  filtered (version to `[0-9A-Za-z.+-]`, 40 characters; path printable, 200). A stamp field of the wrong
  type, or a hook line that can't be decoded, is refused like an unknown protocol (fail closed). `rubi-home
  status` hashes only a regular file of at most 256 MiB, so a FIFO or device can't hang it, and runs the hook
  with a minimal environment.

  `rubi-home install-hook [--dry-run] [--to PATH] [--sudo]` plans a copy of the running binary (symlinks
  resolved) to the hook path. The privileged part is one script run by `/usr/bin/osascript` ("do shell script
  ... with administrator privileges", the script passed as an argument so no AppleScript quoting is involved)
  or, with `--sudo` in a terminal, `/usr/bin/sudo /bin/sh -c`, both with `PATH` reduced to the system
  directories and every tool in the script an absolute path. The script, as root: checks that the target
  directory and every ancestor are root-owned and not group/world-writable; reads the source once into a temp
  file in the target directory; checks that file's sha256 against the hash the user was shown (computed
  earlier from the same path); only then `chown root:wheel` (`root:root` off macOS), `chmod 0755` and an atomic
  `mv -f` over the target; the temp file is removed on any failure. So a source swapped after the hash was
  shown is not installed. The hash is of the file at the source path: it shows what will be installed, not that
  it is genuine; the output says to compare it with the release's `SHA256SUMS`. The target must be a
  plain file or absent (a symlink is refused), its directory must exist and have root-owned, closed
  ancestors (otherwise install-hook refuses and says how to create the directory; it creates nothing). Paths
  with quotes, backslashes, backticks, `$`, control, format or line-separator characters, invalid UTF-8, or
  relative paths are refused instead of escaped. Afterwards it re-checks, as confirmation only, the target's
  sha256, that `DST version` prints the same version, and that the target and its ancestors are secure, and exits
  non-zero with the reason otherwise. `--dry-run` prints the plan (hashes, target, the script) and runs nothing.

  **The sister tools of Claude Code** (`rubi-home claude-mcp`). A stdio MCP server (go-sdk) that reads no
  configuration and gives Claude four tools: `rubi_sisters`, `rubi_ask {text, kind?}`, `rubi_reply
  {message_id, text}` and `rubi_inbox_status`. Each call is one line on `claude.sock`: `{"hook_event_name":
  "RubiTool","tool":...,"claude_session_id":...,"tool_cwd":...,args}`, stamped like a hook line (version,
  protocol, path). The session id sits in `claude_session_id`, not `session_id`, so an older helper, which
  sees an unknown event and answers `{}`, doesn't even register a session from it; the tools then say Rubi
  Home is too old. The helper routes `RubiTool` right after the version check, before the waiter and the
  event path: it is never an approval or an event, has 16 slots of its own (full: `{"error":...}`), a 5 s
  deadline, a panic guard, and the handler does memory work only. Answers are `{"ok":true,...}` or
  `{"error":"..."}`.

  Which session calls is decided by the kernel's view of the process tree, not by what the line says (the
  `session_id` and directory in a tool line, and `CLAUDE_CODE_SESSION_ID`, are sent for diagnostics only).
  Right after Accept, before the line is read, the helper asks for the peer's pid (macOS `LOCAL_PEERPID`,
  Linux `SO_PEERCRED`) and its start time. A session is one `claude` process, so the identity is anchored
  there: for the peer the helper walks up at most 6 levels to the first ancestor whose process name is
  `claude` (macOS `kern.proc.pid` `P_comm`, Linux `/proc/<pid>/stat`) and uses that one process with its
  start time; with no such ancestor it uses the peer's direct parent. Shells, terminals and the desktop app
  above it are never recorded or consulted. A hook line for session S (SessionStart, UserPromptSubmit, Stop,
  PermissionRequest, RubiWait) makes that process S's. While S keeps sending hooks, a hook line naming another
  session from the same process is ignored (logged once). A claim ends with a SessionEnd line, but only one
  whose peer anchor is the process holding the claim (a SessionEnd from another tree leaves it alone; the
  registry and inbox handling of SessionEnd are unchanged, and `/clear` works because its SessionEnd comes from
  the same `claude`); when the start time no longer matches (pid reuse: a new `claude` under the same shell has
  another pid and no claim yet); after 24 hours without a hook; or when its session has been silent for 10
  minutes and a hook line for a different session arrives from the same process (a missed SessionEnd; logged).
  The map holds at most 2000 processes. A tool call is attributed by looking up exactly the caller's anchor,
  and nothing past it: no entry, no tools ("this session isn't known to Rubi Home yet ... try again after its
  next prompt"). The process must be named `claude` for the anchor to be the session's own process;
  otherwise the direct parent is used, which for hooks and MCP servers are different processes, so the tools
  are refused (fail closed). Where the peer can't be identified (an OS without it) the tools are refused too.

  What this does and doesn't guarantee. Tool identity is the claude process tree as the kernel reports it,
  plus the hook lines, which are trusted as before. Any process inside session A's tree acts as A: it can
  use A's tools, end A's claim with a SessionEnd line and claim the process for another session id (which is
  what `/clear` does). A same-user process outside every `claude` tree is refused unless it first claims a
  process of its own by sending hook lines (it could always forge hook lines): it can then claim a session
  id that no `claude` process has claimed yet, and act as that id, nothing more; it cannot take a process that
  an active session holds. The tools add no approval power: they queue an ask or a reply for a session's
  sister and read that session's inbox state; there is no way to approve a command, decide anything or
  address another Bot. A session is known from its first hook, so the tools work from its first prompt on. An
  ask from a session that isn't bound may still go to the Bot of its project (the plugin's route, as for a
  text block).

  `rubi_ask` queues a `kind: "ask"` outbox entry like a ```` ```rubi-ask ```` block (kinds research, audit,
  lookup; at most 2000 characters, but an error instead of a silent drop; at most 3 per session per minute and
  20 per minute for all tool calls together; the rate keys are bounded, the least recently used goes first)
  and returns its id. `rubi_reply` answers a message that was delivered to the session within 24 h (the
  helper remembers 100 deliveries per session: id, sender, time, answered): a `kind: "reply"` entry with
  `to` the message's sender and `in_reply_to` its id (the plugin delivers a reply only to the Bot it is
  bound to, so `to` must be the sender), `answered` status once, and the message leaves the turn's owed
  replies, so the turn's final answer doesn't repeat it (other senders' messages still get the final
  answer). A message can be answered once: a reply to one already answered, by a reply or by the final
  answer, is refused. At most 3 per session per minute. `rubi_inbox_status` shows the session state, waiting
  messages, owed replies, whether a waiter is attached, how many asks and replies of the session are still
  queued for Rubi (`queued_outgoing`) and the last 10 message states. `rubi_sisters` shows
  the Bots bound to the session (name, since) and the senders of messages delivered in the last 24 h.

  When the queue file is loaded, the bound lists and delivered messages are sanitized (a few Bots per session,
  one binding per Bot, message ids of the helper's shape, one-line senders, no times in the future).

  Bound Bots are told by the plugin: `claude-sessions.bound {session_id, sister, bound}` (capability
  `claude-sessions`; a Bot has one binding, so binding it takes it off any other session; 500 sessions at
  most). Bound sisters and the delivered messages are saved in `claude-queue.json` as the optional keys
  `bound` and `delivered`; `version` stays 1, and an older helper ignores them.

  `rubi-home install-hook --with-claude-plugin [--plugin-to DIR]` also installs the marketplace embedded in
  the binary (`claude-plugin/`, `go:embed`: marketplace `rubi`, plugin `rubi-sisters` with a skill and an
  `.mcp.json` that runs `/usr/local/bin/rubi-home claude-mcp`) to `/Library/Application Support/Rubi Home/
  claude-plugin`, in the same privileged run, after the binary. No TOCTOU: the file contents are inline in the
  script (a base64 literal per file), so root reads nothing a user can write. As root the script checks the
  ancestors as for the binary, creates the parent directory if missing, removes leftover `.claude-plugin.*`
  entries of a cut-off earlier run, decodes into a fresh `mktemp -d`
  next to the target, checks each file's sha256, `chown -R root:wheel`, `chmod -R go-w`, then moves the old
  directory aside, renames the new one into place and removes the old one; if the rename fails the old one is
  moved back. (Between the two renames the target briefly doesn't exist.) Afterwards the command re-hashes
  every installed file. Without the flag the plan prints a hint. Managed settings register it
  (`extraKnownMarketplaces` with a `directory` source, `enabledPlugins`); `managed-mcp.json` is not used
  because it takes exclusive control of MCP servers. What this does and doesn't protect: the folder is
  root-owned, and the docs say a marketplace added from a local directory with a relative `source` is read in
  place (no copy), but they say that for adding a marketplace, not for managed `extraKnownMarketplaces`. If
  Claude Code copies the plugin into `~/.claude/plugins`, that copy is user-writable and the root-owned folder
  protects only the source. Either way the MCP server command is `/usr/local/bin/rubi-home` (root-owned) and
  the managed `strictKnownMarketplaces` setting can lock which marketplaces may be added. The hook binary at
  that path must be new enough to have `claude-mcp`; with an older one the MCP server fails to start (Claude
  Code shows the server as failed) until `install-hook` is run again.

  **Starting a session** (`claude-start.create {cwd, prompt, from?}`, its own capability `claude-start`: a
  plugin must declare it, and `claude-sessions` doesn't cover it; it can't approve anything). When the Bot's
  project has no session open, the helper opens the Claude desktop app's link
  `claude://code/new?folder=<dir>&q=<prompt>` with `/usr/bin/open` (a `Launcher`; the encoding is `url.Values`
  with `%20` for spaces) and waits up to 45 seconds for the new session's SessionStart hook (`source` must be
  `startup`, same directory, an id not known before the launch; the framed prompt carries a random reference
  code, and when the transcript (inside `~/.claude/projects`, first 64 KB) already shows the first user
  message it must contain the code, else the session is another one; an unreadable transcript is accepted
  after 3 seconds). It answers `{launched, session_id, cwd, permission_mode}`, or `session_id:""` with a note
  when none showed up (or several did: never a guess). The relay gives a request to Rubi Home 60 seconds in
  all, hence 45. Limits: the directory must be absolute, clean, existing and its own real path (a symlink in
  it is refused, so the approval names the directory the session runs in), and either the working directory
  or project root of a session the registry already knows, or inside a directory in `claude_start_dirs` of
  `config.json` (absolute paths, default none, read fresh on each call); never `/`, the home directory
  itself, `~/.claude`, `~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.kube`, `~/.config`, `~/.docker`, `~/.local`,
  `~/.npm`, `~/.oh-my-zsh`, `~/.zsh*`, `~/Library`, the helper's own directory, `/var`, `/private/var`,
  `/Applications`, `/opt`, a system tree, or `/Volumes` itself (not what is inside), however it is spelled
  (the check compares files with `os.SameFile` too). The prompt is at most 2000 characters and 40 lines, and
  it is rejected, not cleaned, if it has Unicode format characters (zero-width, bidirectional), private use,
  tag characters, control characters other than the newline, C1 controls, or more than two blank lines in a
  row (the plugin runs the same check before the approval, so the approved text is the launched text). It
  reaches the session framed as a message from the sister (untrusted text from a Bot, last, after a line
  saying so). 3 launches per hour per computer, one at a time; the work runs in a goroutine of its own and the
  hooks never wait for it. The session runs with the desktop app's permission settings: if those are
  bypassPermissions (the app's default for the user's sessions here), Claude runs any command in it without
  asking and no Rubi approval applies to it; the plugin's approval says so. The headless route (`claude -p`)
  is not used: it needs the CLI signed in (a scratch run failed with `api_error`), and it ends after the
  prompt unless run with stream-json input, which would make the helper supervise a long-lived child. It fits
  the same `Launcher` interface if it is ever wanted. Manual check on the Mac: see
  `docs/plans/claude-task.md`. The end-to-end build (`-tags rubie2e`) swaps the launcher for one that appends
  the URL to `$RUBI_E2E_LAUNCH_FILE`; it is not in release builds.

Plugins reach the helper through Rubi (`home.call`), limited to the capabilities their manifest declares;
the install screen shows them.

## Agents in a VM (`claude-agent`)

Contract: `docs/design/agent-runner.md`. Code: `internal/home/agent`. Capability `claude-agent`, operations
`claude-agent.projects | sessions | messages | start | send | rename | fork | interrupt | close | mode | next |
decide | status | role.list | role.get | role.history | role.diff | role.put`; a plugin that declares
`claude-agent` can call only those.

- **Projects** are the directories in config `agent_projects` (absolute, existing, each its own project, id from
  the directory name). A directory that is `/`, that holds the helper's own directory (its keys) or that is the
  user's home is skipped. Encrypted per-project volumes (design section 9a) are a later milestone.
- **VM.** `VM` is an interface; `AppleContainer` shells out to Apple's `container` CLI (1.6.x): `build` of the
  image from `runner/container/Dockerfile` (the runner's sources are embedded in the binary, tagged by their hash,
  and built from `<helper dir>/runner-build/<hash>` unless `agent_runner_dir` names a directory), `run -d --network none` (no network at all) with the
  project bind-mounted at `/work`, a non-root user, `--cap-drop ALL`, a read-only root, then
  a long-lived `exec -i` of `rubi-netpipe` (the VM's only way out, see agent-runner.md) and `exec -i --env-file` for the runner. The run token and proxy settings go in an environment file (0600) that
  is deleted once the handshake succeeded, never on a command line. `Fake` plays a runner in-process for tests.
- **Handshake.** Per start Home makes a random `run_token`; the runner's first line must be `hello` with it
  (constant-time compare), else stdin is closed and the process killed; no hello in 10 s ends it too.
- **Slots.** A slot is one running top-level session (subagents and forks inside it don't count against the slot
  limits; the subagent cap below handles them). Three limits apply to a new session: its role (a profile of a project; the project's legacy role is one, and a project is one directory in `agent_projects`) has
  `agent_max_sessions_per_profile` (default 1, at least 1; see "VM per role" below), optionally its project has
  `agent_max_sessions_per_project` (an extra cap on the project's roles together; unset or 0 = none), the Bot that started it has its mode's session limit, and the computer has
  `agent_max_sessions` (default 4, 0 = no cap, so the machine stays bounded whatever the modes say; a Bot on maximum is bounded only by it). Events decide
  each slot: `status busy` takes one, `status idle` frees it
  only when no background subagent is left (`background {agent_id}` adds one; `background {agent_id,
  done:true}` or a `result` with that `agent_id` removes it), `status closed`, the end of the project's runner
  and `close` free it. A `send` to a session that holds a slot goes straight to the runner; a start, or a send
  to a session without a slot, when a limit is reached goes to the disk queue
  (`<helper dir>/agent-queue.json`, 0600, atomic, 24 h, 64 items, 64 KiB each) and is delivered when a slot
  frees, also after a restart (`rubi-home run` resumes it). Delivery is at most once. The queue is first in,
  first out, except that an item waiting on a full project, a Bot at its limit, doesn't hold up the items behind
  it: the first item that can run now goes, so a project with a free slot is never stuck behind another project's
  waiting item. Items of one project and Bot keep their order (they wait on the same limits; items of different Bots in one project can pass each other when one Bot is at its limit), and when only the computer's cap is reached nothing runs, so
  the oldest waiting item takes the next slot freed. A new item doesn't jump ahead of a queued one that could run.
  Items of one role and Bot keep their order. Each queued item says why it waits in plain words (`reason`: "waiting: role visual-design (my-project) already has 1
  session" (the legacy role is "default"), "waiting: project my-project already has 2 sessions" (the optional project cap), "waiting: bot bot-a already has 1 session, the limit of its light mode", "waiting: the computer's
  session limit is reached"). The plugin sees `queue` items (`queued` with the reason, `delivered`,
  `failed`, `expired`) in `next`. `status` has `slots: [{project, role, role_name, session, bot, busy, background, subagents}]`,
  `limits: {per_profile, per_project, total}` (`per_project` 0 = no cap; total = the computer's cap), `queue` rows with `role` and `role_name`, `sessions_running`, `usage` (below); `slot` (the first
  busy slot, or an empty one) stays for older plugins; `projects` rows have `role`, `role_name`, `slot_busy` (the role's slots are
  full) and `sessions_running`. A `role` is the role's id; the legacy role's id is its project id and its name `default`.
- **VM per role (agent roles R4).** `start`, `send`, `sessions`, `messages`, `rename`, `fork`, `interrupt`, `close` and
  `mode` take an optional `role` (a role id from `role.list`; missing, or the project's id, is the legacy role, so older
  plugins keep working). Each isolated role has its own VM (container `rubi-agent-<role name>-<hash of dir|role id>`):
  the project at `/work` (rw), `<helper dir>/agent-roles/<id>/content` at `/rubi-role` (read-only; the runner follows
  `current` per query), `<helper dir>/agent-roles/<id>/claude` at `/rubi-claude` (rw, the config directory and sessions),
  `RUBI_ROLE_DIR=/rubi-role` and `RUBI_CLAUDE_DIR=/rubi-claude`, no sign-in mount, never `--ssh`. Home never reads or
  deletes inside `claude/` (the VM writes there). The legacy role's VM, mounts and environment are unchanged. A role's
  proxy list is the common list plus that role's `hosts`, and its `net.ask` approvals are kept per role in
  `agent-net.json` (`roles` section; the legacy role keeps its per-directory `projects` section), so a role never gets
  another role's hosts; both apply when the role's VM starts. While a role's VM runs, every role version it may read
  (the one current at its start and each one put since) is pinned in the store so GC keeps it. A role VM with no slot
  and no queued item for `agent_role_vm_idle_minutes` (default 15) is stopped and starts again on the next use; the
  legacy VM isn't. An archived role refuses `start` (also a queued one at delivery). `projects` rows: one for the
  legacy role of each project, then one for each isolated role that has a VM, a session or a queued item (same `id`,
  own `role`).
- **Session limits, `claude-agent.limits`.** `agent_max_sessions_per_profile` (default 1) is the rule counted per role;
  `agent_max_sessions_per_project` (optional extra cap on one project's roles together) and `agent_max_sessions`
  (the computer, default 4) are as before. Someone who raised `agent_max_sessions_per_project` before must now also set
  `agent_max_sessions_per_profile`. `claude-agent.limits` with any of `per_profile` (1..256), `per_project` (0..256, 0 = no
  cap) and `total` (0..256, 0 = no cap) saves them and returns the new status; a value not given stays. Like
  `usage_mode` it is for the plugin's panel only (raising a cap is the user's decision), never a Bot tool. The setup
  self-test holds its whole project (all roles) while it runs.
- **Usage modes, per Bot.** Each Bot has a mode that says how much it may run at once; its limits apply to the
  sessions it started and their subagents only, so a Bot on light never caps another on medium. `maximum`: no cap on
  sessions or subagents. `medium` (the default, "Recommended"): 4 sessions, 4 subagents per session. `light`: 1
  session, 2 subagents per session. The per-project limit is separate (1 by default, for every mode) and so is
  the computer's cap. Config: `agent_default_mode` (default `medium`: the mode of a Bot met for the first time and
  of the sessions the user starts from the panel, which count as one extra owner under the default mode),
  `agent_bot_modes` (`{botID: mode}`), and the raw overrides `agent_max_sessions_per_project`, `agent_max_sessions`
  and `agent_max_subagents_per_session` (0 = no cap; it overrides every Bot's mode, and `status` then marks the
  rows `custom`). **Identity:** the plugin knows which Bot sent a message and passes a stable id (letters, digits,
  `_ . : @ -`, up to 64) as `bot` with `start` and `send`; Home keeps the mode table in its config (simpler and safer
  than trusting limits sent per start: a Bot can't pick its own id or its own limits, and the table is one file the
  user owns). A Bot met for the first time is recorded with the default mode (up to 256), so the panel lists it.
  Slots and queue items keep the Bot id. `status.usage` is `{default, modes, bots: [{id, mode, custom,
  sessions_running, sessions_limit, subagents_per_session}]}` (the user's own sessions are the row with id `""`).
  The runner gets the Bot's subagent cap with every `session.start` and `session.send` (`agent-runner.md`).
  **Only the user changes modes:** `claude-agent.usage_mode` with `mode` and exactly one of `bot` (one Bot),
  `default` (the default), `all` (every known Bot and the default; Bots can be changed one by one again after)
  returns the new status. The operation exists for the plugin's panel; Home cannot tell a panel from a Bot tool
  because both arrive through the paired plugin, so the plugin must expose it only as a panel button, never as a
  Bot tool, and a Bot has no way to reach it. A change applies to new starts only: nothing running is killed;
  when a mode is lowered the excess finishes and new work queues; when raised, waiting work goes at once.
- **Roles (`role.*`).** Design: `agent-roles.md`. Code: `internal/home/agent/roles`. Home keeps agent roles per
  device in `<helper dir>/agent-roles/<id>/` (0700; ids `r_` + 8 of `[a-z0-9]`, made by Home). The VM per role and
  its mounts (R4) and the slots per role (R5) are not built yet; this is the store and its operations.
  - **On disk.** `versions/<n>.json` (0600, never rewritten; the commit point), `index.json` (a cache, rebuilt from
    `versions/`), `blobs/<sha256>` (ad-hoc skill text, named by the hash Home computed; read back, it must match its
    name), `content/v<n>/` (`role.json`, `plugin/.claude-plugin/plugin.json` named `role`,
    `plugin/skills/<name>/SKILL.md` normalized by `sdk/skillscan`, `plugin/agents/<name>.md` rebuilt from fields),
    `content/current` (relative symlink `v<n>`) and `claude/` (empty; the future `/rubi-claude`).
  - **Operations.** `role.list {project?}` returns `{roles: [{id, project, name, description, status, version,
    changed_by, changed_at, legacy?}]}`; every project also has an implicit legacy role `default` (id = the project
    id, `legacy: true`, not stored, cannot be put). `role.get {id, version?}` returns the stored version record
    (the current one when `version` is 0), including each skill's scan result. `role.history {id, limit}` returns
    `{id, current, versions: [{version, parent, changed_by, changed_at, name, status, change}]}`, newest first (limit
    default 20, at most 100). `role.diff {id, from, to}` returns `{changes: [{field, key?, kind: added|removed|changed,
    from?, to?}], skill_text: [{name, from_sha256, to_sha256, diff, clipped?}]}`; the diffs are unified, 3 lines of
    context, clipped to 200 lines and 16 KiB. `role.put {id?, base_version, spec, blobs, by?, change?}` returns `{id,
    version, skills: [{name, state, verdict}]}`; `spec` is the role record of section 2.1 (fields Home sets are
    ignored), `blobs` maps sha256 to skill text, `by` is the Bot or user the plugin acts for and `change` is
    `{op, tier, approval, summary}`, kept in the version. Arguments are strict (an unknown key is refused). Archive,
    rename and revert are puts; a revert sends an old version's record as the spec.
  - **Concurrency and conflicts.** One writer per role. A put whose `base_version` is not the current version is
    refused (`the role changed: base_version 1, current version 2`) and writes nothing; a new role has
    `base_version` 0. Names are unique per project among active roles (`default` is reserved), at most
    `role_max_total` (50) roles exist, at most 1000 versions per role.
  - **Validation (all reasons are returned).** Name, description (300), instructions (8 KiB), status, default mode
    (not `dontAsk`); tools from the known list (`roles.KnownTools`, which must match the runner's list); at most 64
    deny rules; MCP servers only with an `approval` id, https URL without credentials or a stdio command; Claude
    plugins only with an `approval` id; hosts and SSH target names by pattern; subagents with `tools` cut to the
    role's tools (a subagent whose tools are all outside the role's is refused) and `model` in inherit, sonnet, haiku,
    opus; at most 20 skills, 10 of them ad-hoc, names `[a-z0-9-]` so none can name a path. A blob is hashed again
    by Home and must be the sha256 it is sent under and be used by a skill. A library skill is accepted only if the
    configured `Verifier` says its signature is valid; until the library exists (R8) the stub refuses every one.
  - **Scan.** Every skill goes through `sdk/skillscan`. Reject: the put is refused with the rule ids and lines.
    Flag: the skill is stored with `state: held`, is not in `role.json` and is not materialized, until a put carries
    every flagged rule id in the skill's `accepted` list (the plugin sets it after the user taps accept); a flagged
    library skill is active (the curator acknowledged its flags). Pass: `active`. The frontmatter keys other than
    `name` and `description` are removed and listed in the stored scan (`stripped`).
  - **Crash safety.** A put writes the blobs, builds `content/v<n>` aside and renames it, links `versions/<n>.json`
    into place (the commit point, which can't replace a file), renames a new symlink over `current`, and writes
    `index.json`, syncing each file and directory. Opening the store and every put reconcile the role from its
    highest version file (rebuild a missing `content/v<n>`, fix `current` and `index.json`, drop temp files and
    content above the committed version). `current` never names a directory that is not complete.
  - **GC.** After each put Home removes `content/v<n>` except the newest 5, the current one and any version marked
    in use with `Store.Pin(id, version)` (in memory), and blobs that no version record names. Version records and
    the blobs they name stay, so history, diffs and reverts keep working.
- **Asks.** `perm.ask` from the runner and `cred.ask` from the git helper become `next` items with Home's own
  `ask_id` and wait for `decide`. No decision in 280 s (`claude_wait_seconds` does not apply here), no `next`
  poll for 15 s, a cancelled runner or more than 16 pending: deny. Home never decides itself.
- **Proxy.** One per running project, HTTP CONNECT to port 443 only, host names from the list, TLS not
  decrypted. Allowed: `api.anthropic.com`, `registry.npmjs.org`, `pypi.org`, `files.pythonhosted.org`, the login
  candidates `claude.ai`, `console.anthropic.com`, `platform.claude.com` (TO CONFIRM with a real login), the
  hosts in `agent_git_hosts`, the owner's `agent_allowed_hosts`, the forges `github.com`, `gitlab.com`,
  `bitbucket.org` (unless `agent_no_default_forges`), and hosts the owner approved for that project.
  **Nothing is derived from the project's files**: `/work` is written by the agent, so a `Package.resolved`,
  `.gitmodules` or `.git/config` there must not widen the list. A host that is not listed is asked about: the
  proxy holds the CONNECT and raises a `net.ask {host}` item; the plugin answers with `decide`; an allow is
  recorded per project in `<helper dir>/agent-net.json` (0600, 100 hosts per project), a denial is
  remembered for 5 minutes, and with Rubi locked the answer is deny. Datadog and `downloads.claude.ai` are
  refused even if listed, and are never asked about. The proxy resolves names itself and refuses to connect to
  an address that is loopback, private (RFC 1918), link-local, ULA, CGNAT (100.64/10), 0.0.0.0/8, multicast,
  reserved, or an IPv4-mapped/NAT64/6to4 form of those: one such address in the answer refuses the host, and
  the socket's own address is checked again when it connects. Only `agent_git_hosts` may resolve to private
  addresses (an owner's own git server). It requires
  `Proxy-Authorization: Basic` with a per-start token (put into `HTTPS_PROXY` as `http://rubi:<token>@127.0.0.1:3128`;
  not the run token), and ends tunnels idle for 10 minutes. It listens on no socket: a VM has no network, so the proxy
  and the auth gateway serve the streams of the VM's `rubi-netpipe` (agent-runner.md, "No network: rubi-netpipe"), which
  the VM reaches on its own loopback. `agent_network`, `agent_proxy_listen` and `agent_proxy_vm_host` are obsolete: they
  still parse and are ignored (the proxy still refuses a wildcard listen address when it is given a socket, as the tests do).
- **No network.** Every VM is started with `--network none`. There is no host-only network to create or to verify, no
  address of the Mac the VM could reach, and no listener opened for it. The setup wizard's network step stays (same id) and
  reads "done: not needed". The self-test asserts the absence of any route or interface besides `lo`, `ENETUNREACH` for
  direct connections, failing DNS, a working proxy path and a refused unlisted host.
- **Git credentials.** The helper in the VM (`runner/container/git-credential-rubi`) hands `git credential
  get|store|erase` to the runner, which sends `git.credential {op, input, purpose}` to Home (the runner side
  of the relay is part of the runner, not yet in M1). Hosts must be in `agent_git_hosts` and https. `get` for a
  read is served from the source (file `agent-git-credentials.json`, 0600: `{"git.example.com": {"read":
  {...}, "push": {...}}}`); `purpose: "push"` raises a `cred.ask {host, path}` (path cleaned and cut at 200)
  **every time**, so each distinct repository path is asked about again, and gets the separate push credential
  only on an allow (the mandate check is the plugin's). `store` and `erase` are accepted and ignored. The read
  credential must be a read-only token. The push credential must be scoped to the repositories the agent
  may push to and short-lived: once the VM has it, nothing stops it using it until it expires, and the VM can
  keep it. Home never logs the credential helper's input or output (tested).
- **Slot watchdog.** A session that holds a slot with no event and no pending ask for 30 minutes
  (`SlotIdleTTL`) is interrupted and its slot freed (logged, and the plugin sees a `status closed` event); each
  slot has its own clock.
- **Modes.** `agent_allow_bypass` (default `true`) allows `start` and `mode` with `bypassPermissions` or
  `dontAsk`; set it `false` to refuse them. The plugin still decides every tool call and approves loosening a
  mode with a passkey.
- **Projects and `/work`.** A project directory is refused if it is `/`, the home, a parent of the home
  (`/Users`), a parent of the helper's directory, `~/.ssh`, `~/Library`, `~/.gnupg`, `~/.aws`, `~/.config`,
  `~/.kube` or inside them, or has a `,` or control character in its path (it would break the `--mount`
  option). `/work` is the same directory the user sees on the Mac: what the agent writes there (git hooks,
  `.vscode/tasks.json`, build scripts, `.envrc`) runs with the user's rights the next time they use an editor or
  git there, so review it like code from a stranger. On the first start Home creates `/work/.rubi/` and a
  `.gitignore` containing `*` in it (never through a symlink; an existing file is left alone), because the
  runner's sessions and config live there. Home reads nothing else from `/work` and makes no decision from it.
- **Resource limits.** The runner serves at most 64 requests at once; more are answered with an error.
- **Not in M2.** The plugin (M4), encrypted volumes, a cap on running VMs, the `git.credential` relay inside
  the runner.
