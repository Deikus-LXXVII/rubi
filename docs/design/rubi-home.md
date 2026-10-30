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

Plugins reach the helper through Rubi (`home.call`), limited to the capabilities their manifest declares;
the install screen shows them.
