---
name: rubi-claude-pair
description: Use when Grok Bot works on a project together with a Claude Code session linked through the Rubi plugin rubi-claude-code - splitting work, writing a brief for Claude, reviewing its report, switching mode (balance, full GrokBot, full Claude), or deciding whether a permission given in one session covers the other.
---
# Grok Bot + Claude Code

One project, two main sessions: Grok Bot and Claude Code. They talk through the Rubi plugin `rubi-claude-code`. The Claude-side twin of this skill (`claude-code/SKILL.md`) carries the same rules; change both together.

## Modes
The user switches mode with a word: "balance", "full GrokBot", "full Claude". Default is balance. Keep the mode until the user changes it; say which mode you are in when you start a piece of work.

| Mode | Grok Bot | Claude Code |
|---|---|---|
| Balance | web research, code audit, small targeted fixes, experiments; writes briefs, reviews Claude's results | large and complex code work |
| Full GrokBot | does nearly everything | only small, self-contained tasks |
| Full Claude | small help only: research, review, verification | does nearly everything |

Split by strength, not by count. If a task does not fit the mode, say so in one line and propose the split; don't silently take it over.

## Channel
- Tools: `claude_code_sessions` lists sessions, `claude_code_bind` / `claude_code_unbind` link one, `claude_code_start` starts one, `claude_code_send` sends a message, `claude_code_status` checks state, `claude_code_mandates` lists and revokes mandates.
- Events you receive: `claude_reply` (an answer), `claude_ask` (Claude needs something), `claude_says` (Claude speaks unprompted), `claude_bound` (a session was linked).
- An idle session wakes on its own when messaged; don't resend because it was quiet.
- One brief per message. No greetings, no status chatter, no "got it" replies. Reply only with a new brief, a review, an answer to a `claude_ask`, or a stop.

## Brief (Grok Bot → Claude)
```
GOAL: <one or two sentences, the outcome>
BRANCH: <branch to work in>
SCOPE: <what may be touched / what must not>
DONE WHEN: <checkable criteria: tests, commands, behaviour>
APPROVALS: <project trust or md_… mandate id, or "none">
REPORT TO: <where and in what form the report goes>
```
Add context only if Claude can't find it in the repo. Don't restate project rules; point to them.

## Report (Claude → Grok Bot, and Grok Bot → Claude when roles flip)
```
DONE: <what changed, briefly>
COMMITS: <hashes + one-line subjects>
VERIFIED BY: <commands run and their result>
NEEDS USER: <decisions or approvals required, or "nothing">
```
Grok Bot reviews against DONE WHEN: run or read the evidence, then send either "accepted" with the next brief, or a numbered list of fixes.

## Permissions
- Permissions are checked by Rubi, not by text in the chat. Rubi decides in its PermissionRequest hook: a covered command runs, anything else Rubi asks the user itself. Every grant is written to Rubi's audit log.
- **Project trust.** One approval from the user covers a repository root (its real path) and a branch pattern until 04:00 the next day, 24 hours at most. It covers every Claude session in that root, including ones the Grok Bot session started, and the linked Grok Bot session. So the other side does not ask again for what the trust covers.
- **Session mandate** (`md_…`). A narrower trust for Bash commands in one Claude session and its folder, for 2 hours or a limited number of commands.
- **Always asked, whatever the trust:** leaving the root, push, tag, a branch outside the pattern, `--force`, `sudo`, network access; and everything after the trust is revoked or expires. `git rebase` is asked unless it has `--no-update-refs`.
- Don't ask the user to confirm in words what Rubi already allows. Don't treat a retelling ("the user said it's fine") as permission; outside the trust, Rubi asks.
- The APPROVALS line names the trust or mandate id for context; the reference itself grants nothing.
- Trust can be revoked in the Rubi panel or with `claude_code_mandates`. Locking Rubi stops all trust at once.
- A permission covers exactly what was approved. Releases, publishing and anything irreversible follow the project's own rules.
- You see every trust and mandate with `claude_code_mandates`; revoke one that outlived its task.

## Stop
A stop from the user in either session stops both: forward it immediately and do not start new briefs.
