# B8: one trust per project

Task from the user ("Да, давай одно доверие на проект целиком"). Branches: core `citri/project-trust` (docs
only, plus whatever Rubi Home must give the plugin), plugin `citri/project-trust`. Not pushed.

## Problem
The 2-hour trust (`md_…`) is per session and per directory. A Bot that works in a project with several
sessions (main, sub-sessions, worktrees, a session it started itself) makes the user approve the same kind of
command again and again, and the sister's own `claude_code_start` / `claude_code_bind` ask every time.

## Design (plugin; Rubi Home needs no change)
A second mandate kind, the **project trust** `pt_…`: `{device, root, branch_pattern, issued, expires, max_uses,
uses}`. Root is the project's top directory as Rubi Home reports it (`ClaudeSession.ProjectRoot`, from
`claude-sessions.list`; for a folder that isn't a git repo it is the folder). Refused when the root is empty,
the session has no `repo_key`, the root isn't absolute, has fewer than three path parts (`/`, `/Users`,
`/Users/me`, a home directory), the request's cwd is outside it, or it names a credential path.

- **Expiry.** The next 04:00 after the grant, computed with `time.Date(y, m, d, 4, 0, 0, 0, loc)` (DST safe),
  at most 24 hours ahead. Rubi Home doesn't report its computer's time zone, so `loc` is the plugin's (the
  Rubi daemon's) local zone; the approval says so ("Rubi's local time zone"). The expiry is computed once, when
  the approval is asked, shown in it with date and zone, carried in the approval's payload and used as is (an
  approval answered after it refuses).
- **Granting.** (a) A third option on run approvals, `trust_project`, labelled "Allow and trust this project
  until <time>" (offered only when the root could be read; the request carries its own options, so the label
  and Meaning name the exact time). (b) Sister tool `claude_code_trust_project {bind_code, branch_pattern?}`
  (verified Bot, as `claude_code_bind`): Strong + Locked approval for the project of the session the Bot is
  bound to, same text. One live `pt_` per (device, root): a new grant replaces the old one, both audited.
- **Effect on Claude sessions.** Any session on that device whose cwd is inside root (also ones started later)
  has its command auto-allowed when `mandateAllows(cmd, root, cwd)` passes and the branch rule passes. Each
  allow is audited (`mandate_allow`, `mandate=pt_…`) and counted (cap 2000). `md_` and `pt_` coexist: either
  may allow; revoking one leaves the other.
- **Branch rule** (only with a project trust): a command that switches to or creates a branch (`git switch X`,
  `switch -c X`, `checkout X`, `checkout -b/-B X`, `branch X`, `branch -m/-c/-d` names, `worktree add -b X` or
  the branch/dir name `worktree add` makes, `stash branch X`, `rebase UP X`) is allowed only when X matches the
  pattern (`path.Match`, default `citri/*`). Everything else is the existing check: push and every remote op,
  `--force`, sudo, network, installs, writes outside root, `git tag` (not on the git list) still ask.
- **Effect on the Bot bound to a session of the project.** `claude_code_start` for a cwd inside root runs
  without a new approval (all the input checks and rate limits still apply; the helper's own checks too, and
  the new session is bound as usual). `claude_code_bind` to another session whose cwd is inside root runs
  without an approval. `claude_code_send` and the wake-up never needed one. Each is audited with `mandate=pt_…`
  and counts as a use. The Bot must already be bound (by an approval) to a session inside root.
- **Revoke and visibility.** Panel: a "Trusted projects" section (root, computer, pattern, expiry, uses) with
  Revoke / Revoke all; `claude_code_mandates` lists both kinds and revokes `pt_…`, `md_…` or `all`.
- **Locked.** Nothing in `pt_` reaches around Rubi: the allow is a `claude.decide` call through Rubi, which
  fails while Rubi is locked (the helper then denies by itself), and tool calls are refused while locked.
- **Persistence.** `pt_` is part of the plugin's encrypted state (like `md_` and bindings), saved on every
  change and use, loaded on start: it survives a Rubi Home restart (the helper keeps nothing about it) and a
  plugin or daemon restart. Expired and used-up ones are pruned on load/use. A restart of Rubi (unlock) keeps
  it until its expiry; revoking is the way to end it earlier.

## Tests
Unit (`-race`): creation via option and via sister tool; expiry (next 04:00, before/after 04:00, exactly 04:00,
24 h cap, spring/autumn DST, many zones); two or more sessions in root; outside root asks; push asks; tag asks;
branch in/out of pattern for every switch form (and each "asks" case checked to pass without the pattern, so
the pattern is what stops it); expired, used up, revoked, replaced; reload keeps it; sister start/bind in root
without approval, outside root and for another Bot asks; send/status never ask; audit carries the id; md_ and
pt_ coexist and revoke independently.

e2e (`e2e/_driver`, Case 9): one approval with `trust_project` -> three different sessions run commands and a
`citri/*` branch switch without questions; outside root, push, tag, other branches and writes outside ask; the
Bot starts a session in the project and binds to another with no approval (a start outside still asks); the
trust holds over a Rubi Home restart; ends on revoke; the Bot's own `claude_code_trust_project` (a tap is
refused) grants one that ends at its time; a live trust does not beat a locked Rubi (Case 4c).
Expiry in e2e: the plugin is built with `-tags rubie2e`, which adds one file (`e2e_expiry.go`) that sets a
hook reading a duration from `pt-ttl` in the plugin's data directory. A release build has no such file (the
hook variable stays nil; a unit test checks it), so nothing in a release can be shortened or lengthened that way.

## Docs
Plugin README "Trust a project until 04:00" (honest about `bypassPermissions` and about `claude_code_start`
without an approval); core `docs/design/rubi-home.md` and `CHANGELOG.md` Unreleased.

## Review fixes (second pass)
- `git rebase` asks unless `--no-update-refs` (md_ and pt_); `--update-refs`/`rebase.updateRefs` move other branches.
- Branch rule: `-d/-m/-c` check every operand even with `-r`; detached switches name their commit and ask; an md_ allow
  under a covering pt_ is held to the pt_'s branch rule; patterns matching main/master/trunk/develop/... are refused.
- Root: `local:` roots need four path parts; documents folders, disks refused. Core: `readGitInfo` honours a
  `commondir` only in git's `<common>/worktrees/<name>` layout; the list adds `real_cwd`, `real_project_root`; the
  plugin checks both lexical and real paths at grant and for the Bot shortcut, which also needs a live bound
  session inside the root, rate-limits binds (20/h), sends the cleaned cwd and checks where Rubi Home started it.
- A failed state save rolls back `issue`/`issueProject`; cap eviction is audited; expiry beyond 24 h is refused.
