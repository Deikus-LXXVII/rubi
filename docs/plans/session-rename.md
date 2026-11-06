# B10: rename Claude Code sessions

Branches: core `feature/session-rename` (docs plus one generic panel feature), plugin `feature/session-rename`
(both stacked on `feature/project-trust`). Not pushed.

## Does Claude Code have a documented way an outside process can use?
Checked on 2026-10-30 (https://code.claude.com/docs/en/cli-reference, /commands, /sessions, /desktop):

| Way | Documented where | Usable from Rubi? |
|---|---|---|
| `claude -n/--name <name>` | cli-reference | Only at startup of a new session. Cannot name an existing one. |
| `/rename [name]` | commands, sessions | A slash command typed in the session ("rename the current session"; also in `-p` mode). Not callable by another process, and not by the model. Running `claude -p --resume <id> "/rename x"` would start a second Claude Code process on a live session's transcript (and the desktop app's own title would not change); that drives Claude Code, which this feature must not do. |
| Ctrl+R in the `/resume` picker | sessions | Interactive UI. |
| Rename in claude.ai / Remote Control, or by clicking the title in the desktop app | sessions, desktop | UI. |
| Asking Claude in the desktop app to rename a session | desktop ("You can also ask Claude to rename or archive a session") | That is the desktop's own agent surface, not an API. |
| `~/.claude/sessions/<pid>.json` "name", the desktop store's titles | undocumented | Writing them is undocumented and forbidden here. |

Result: no documented way for an external process. So the name is an **alias kept in Rubi** (the plugin's
encrypted state). It never reaches Claude Code and never changes what Claude Code shows; it is the name Rubi
and the Bots use. (Claude Code's own rule for names, for comparison: control and invisible characters become
spaces, at most 200 characters. The alias rules below are stricter and reject instead of repairing, because the
user approves what the alias says.)

## Design (plugin; one generic panel feature in core)
- **State.** `pluginState.session_names`: `{device, device_name, session_id, app_session_id, name, set, seen}`.
  A session is identified by Claude Code id or desktop app id (the id may change when the app restarts a
  session; the alias follows through the app id and is updated when seen).
- **Name rules** (`checkAliasName`): trimmed; 1 to 64 characters (runes); valid UTF-8; no control, format
  (zero-width, bidi, tag), private-use, line/paragraph separator characters, no space other than U+0020, no two
  spaces in a row; must not look like an id: starts with `local_`, a Rubi id prefix (`md_`, `pt_`, `bc_`, `ask_`,
  `cc_`) with its random tail, a UUID, hexadecimal only with 8 or more characters, equals any session id on that
  computer. Unique per computer, compared case-insensitively. (Look-alike letters from other scripts are not
  detected; the approval shows the name and the session's real id.)
- **Tools.**
  - `claude_code_rename {rubi_agent, rubi_agent_code, session_id? | session_name?, device?, name | clear}`.
    With neither session field it renames the session the Bot is bound to.
  - `claude_code_sessions` shows `name` next to `title` for named sessions.
  - `claude_code_bind` takes `name` instead of `session_id`: alias to id on that computer (`device` narrows);
    unknown, ambiguous (same name on two computers) and vanished sessions give a clear error. `claude_code_send`,
    `claude_code_status` and `claude_code_start` take no session (they act on the bound session / start a new
    one), so there is nothing to resolve there; status, bind and start results include `name`.
- **Permissions.**
  1. The session the calling Bot (verified by Rubi, as for bind) is bound to: no approval, audited
     (`session_renamed`, `approval: own bound session`).
  2. Any other session, covered by a live project trust (`pt_`) whose root also holds a live session the Bot is
     bound to: no approval, audited like the other trust shortcuts (`mandate_allow`, action `claude_code_rename`).
  3. Otherwise a new action `claude-code.rename` with **Tap** (locked, so the user cannot lower it). Why Tap:
     renaming is a label inside Rubi, reversible, and changes nothing in Claude Code; the plugin's non-strong
     level (Tap, the one read-only commands use) fits ("Tap only for low-risk actions"). Strong stays for what
     gives a Bot reach (bind, start, trust).
  4. The user in the panel renames or clears any session directly (the panel is the user's own request).
- **Panel.** The page gets a "Session names" section: live sessions (title, computer, project, state) with
  Rename and Clear name buttons, and names whose session is gone. The generic `view` action gets an optional
  `prompt: {arg, label, value}`: the panel asks with `window.prompt` and passes the answer in `args[arg]` (an empty
  answer is a valid answer; cancel does nothing). The plugin refuses a rename call that has no `name` key, so an
  older Rubi panel without `prompt` cannot clear a name by accident.
- **Clearing / expiry.** An empty name in the panel, `clear` for a Bot, or Clear name removes the alias. An alias
  whose session has not been in a complete session list (all sessions, archived included) of its computer for 30
  days is dropped; an alias of a computer that is no longer paired is dropped after 30 days too. `seen` is
  refreshed (at most hourly, to avoid saving on every list) whenever the session is listed.
- **Limits.** 20 rename calls per Bot per hour; at most 500 aliases (a new one is refused with a message; clearing or
  expiry makes room).

## Review fixes
- Home's `claude-sessions.list` reports `truncated` (and `total`); the plugin never drops names on a truncated, stale
  or unanswered list, and resolves a name missing from one through the stored ids and last-seen details.
- The sweep reads archived sessions only when names exist and every 10 minutes; the panel reuses its list for 5 minutes.
- Name check is an allowlist (L, N, P, S, ASCII space; no marks, fillers, blanks) and case folding is one function.
- Names remember who set them; a Bot needs a tap to replace or clear the user's name; results show `previous_name`.
- Rate limit counts after the session is found; execute re-checks the name against session ids.
- The panel asks in its own box ("<plugin> asks:"), with limits and a refusal list, instead of `window.prompt`.

## Tests (`-race`)
Validation table; uniqueness (case-insensitive, per computer); own session without approval, other session asks
(Tap, preview names old and new), project trust shortcut, panel rename and clear; bind by name, unknown and
ambiguous name, name that is an id; sessions output has `name`; persistence over a reload; 30-day expiry (kept at
29 days, dropped at 31, kept when the computer did not answer); no `name` key in the panel call is refused.
e2e: a rename case only if cheap (see report).

## Docs
Plugin README ("Naming sessions"), core `docs/design/rubi-home.md` (claude-code plugin section),
`docs/design/plugins.md` (view action `prompt`), `CHANGELOG.md` Unreleased.
