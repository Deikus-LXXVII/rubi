# B4: Hook binary versioning and install-hook

Branches: core `feature/hook-version` (from `feature/idle-wake`), plugin `feature/hook-version`. Not pushed.

## Problem
Claude Code's managed hooks run `/usr/local/bin/rubi-home`, root-owned on purpose (a user-writable path would
let any process swap in an auto-allow binary). The helper at `~/.rubi-home/bin/rubi-home` updates itself, the
root copy changes only when the user re-copies it, so it silently falls behind. The socket line has no version.

## Design (core)
1. **Hook side.** Every line a hook sends (PermissionRequest, events, the `--wait` waiter) gains
   `rubi_hook_version` (internal/version), `rubi_hook_protocol` (const `ClaudeHookProtocol` = 1) and
   `rubi_hook_path` (os.Executable, best effort). The fields are spliced into the compact JSON object
   textually, so `tool_input` bytes (and so its digest) are untouched.
2. **Helper side** (`hookver.go`). serveConn reads the fields before anything else:
   - protocol missing: a legacy hook (v0.12.x): served as before, marked stale;
   - protocol outside [min, max] (1..1) or unreadable: refused. PermissionRequest (and lines with no event) get
     `claudeDeny("the Claude Code hook binary speaks protocol N, Rubi Home speaks M: run
     `~/.rubi-home/bin/rubi-home install-hook`; denied.")`, events and RubiWait get `{}`;
   - the last seen hook `{version, path, protocol, at}` is kept under its own lock `hmu` (never held with
     `mu` or `wmu`, no I/O under it); a mismatch or staleness is logged once per distinct
     (version, path, protocol);
   - stale = hook version older than the helper's (numeric semver compare; `dev` or unparseable: not stale).
   - The extra fields never reach `parseHookInput` results (it reads known fields only); a test pins that the
     shown command and digest are identical with and without them.
3. **Exposed.** `claude-sessions.list` adds `"hook": {version, path, protocol, stale, refused, seen_at,
   home_version}` once a hook has been seen. `rubi-home status` (a separate process) runs
   `<hook> version` with a 3 s timeout and kill (default `/usr/local/bin/rubi-home`, override
   `RUBI_HOOK_BINARY` or `--hook PATH`), and prints version, sha256, root-owned/not group-or-world-writable,
   and "stale" when older than itself. `version` no longer opens the helper's directory.
4. **`rubi-home install-hook [--dry-run] [--to PATH] [--sudo]`.** Source is os.Executable() with symlinks
   resolved. Plan: source sha256, `install -m 0755 -o root -g wheel SRC DST` through
   `osascript -e 'do shell script "..." with administrator privileges'` (or `sudo` with `--sudo` when stdin is
   a terminal). Paths with quotes, backslashes, newlines or other control characters are refused, not escaped.
   After the run: dst sha256 equals source and `dst version` prints the same version, else non-zero exit with
   a clear message. `--dry-run` prints the plan and runs nothing. The runner is a seam for tests.
5. **install-home.sh.** After installing, compare `/usr/local/bin/rubi-home version` with the new helper's:
   differs: warn with `~/.rubi-home/bin/rubi-home install-hook`; missing: point at the plugin README. No root.
6. **Self-update.** There is no helper self-updater in this repository (upgrades are `install-home.sh`
   re-runs and a LaunchAgent restart), so the helper checks the hook binary at `run` start, in a goroutine
   off the hook paths (3 s timeout), and logs a warning when it is older.

## Design (plugin)
`claude_code_sessions` adds a per-computer problem when the list's `hook.stale` or `hook.refused` is set:
"the Claude Code hook binary on <device> is <v> (Rubi Home <w>): run `~/.rubi-home/bin/rubi-home
install-hook` there". README: install-hook replaces the manual copy (manual fallback kept) and the warning.

## Tests
Core: hook lines carry the fields; protocol mismatch denies / `{}`; legacy line works and marks stale; extra
fields not in Ask's command/digest; list shows hook; status hook check (script, hanging script); install-hook
dry run, fake-runner success and hash mismatch, unsafe paths; semver compare. Plugin: problem text.
`go test -race ./...` in both, e2e `RUBI_REPO=... bash e2e/run.sh` must print E2E PASS.

## Status
- [x] core
- [x] plugin
- [x] e2e (Linux container: E2E PASS; manual Mac check of install-hook pending, user)
