# Agent roles (profiles) for the Agent SDK path

Status: DRAFT for review, 2026-10-31. Implemented so far in the core repo: runner role mode (R1), the role store
(R3), the VM per role with per-role hosts and approvals, version pins, idle stop and the `role` argument of the ops
(R4), slots per profile with `claude-agent.limits` (R5), the shared skill library in core with signing, the curator
setting, the `rubi_library_*` tools, the `library.get` host method and Home's pinned-key verifier (R8, §8), and Home SSH
targets (R11, section 9.5.1). See rubi-home.md ("VM per role", "Session limits").
Deviations noted in implementation: the container's name uses the role's id and a 64-bit hash; role versions put
while a VM runs stay pinned until it stops; `projects` rows are one per (project, role); `net.ask` approvals of a role are
kept in `agent-net.json` under `roles`.
Covers the owner's addenda on roles (A2), Bots curating roles (A3), the shared skill library (A4) and SSH targets
plus the VM-to-Mac gateway (A8). Builds on agent-runner.md, rubi-home.md ("Agents in a VM"), home-setup-wizard.md
and plugins.md §5-§8.

**Assumes the auth gateway (decided 2026-10-31).** No Claude credential is mounted into any VM; `/rubi-auth` goes away.
Rubi Home runs an auth gateway: the VM gets `ANTHROPIC_BASE_URL=http://<gateway>:<port>` and a placeholder
`CLAUDE_CODE_OAUTH_TOKEN` made fresh at each start, and Home puts the real token into requests to
`api.anthropic.com`. A role's config directory therefore never holds credentials. SSH (§9) uses the same pattern:
Home holds the secret and proxies.

## 0. Summary

- A **role** is a named profile on one project: its own skills, subagents, built-in tools, MCP servers, Claude plugins,
  default permission mode and SSH grants. Several roles can share a project.
- **Isolation is enforced by what is mounted, not by repo files.** Each role runs in its own VM. The role's content is
  materialized by Home into a directory and bind-mounted read-only. The runner loads it as an SDK local plugin with
  `settingSources: []`, a `skills` allowlist, a `tools` base set and `strictMcpConfig`. The repo's `.claude/` is never
  loaded for a role. CLAUDE.md comes in through `systemPrompt.append`. Every point in this list was checked against the
  real SDK (§1).
- **Where the data lives.** Role records, versions and materialized content are kept by Rubi Home (per device, since
  projects are per device). The tier policy, approvals, per-Bot limits, audit and panel pages are in the `claude-code`
  plugin. The skill library and its signing key are in Rubi core.
- **Two tiers.** Self-service changes are applied at once, audited, and can be reverted with one click. Widening changes
  return `needs_approval` with a pending strong approval.
- **SSH.** The private key is generated and held by Home and never leaves the Mac. The VM only has a wrapper that names a
  target; Home is the SSH client and pins the host key.
- **Gateway gap.** Recommended: no IP route from the VM to the Mac at all, with egress tunnelled over the exec channel
  (no root). A live spike must confirm a networkless container first. The fallback is a pf anchor installed through the
  wizard's administrator step.

## 1. Verified SDK behaviour (spike, 2026-10-31)

Setup: `@anthropic-ai/claude-agent-sdk` 0.3.314 with its bundled `claude`, Node 26, and a scripted local Messages API
(the runner's integration-test pattern extended to record full request bodies: tool names, system prompt, messages,
headers). Fixture:
- a git repo containing `CLAUDE.md`, `.claude/skills/repo-skill`, `.claude/agents/repo-agent.md`,
  `.claude/commands/repo-cmd.md`, `.claude/settings.json` (an `env` sentinel and an allow rule), `.mcp.json`,
  `sub/.claude/skills/nested-skill`;
- `$HOME/.claude/skills/home-skill` and `$HOME/.claude/CLAUDE.md`;
- profile config dirs A and B, each with `skills/`, `agents/`, `CLAUDE.md` and `settings.json`; A's skill has
  `allowed-tools: Bash(touch:*), WebFetch`;
- local plugin dirs `roleA` and `roleB` (plugin name `role`), each with one skill and one agent.

The spike scripts (`spike.mjs`, `mock.mjs`, `mcp-stdio.mjs`, `sessions.mjs`, `fmt.cjs`) are in the session scratch
dir. They should be ported to `runner/test/roles.integration.test.ts` in R1.

Every run below used `settings: {disableAllHooks: true, apiKeyHelper: ""}` and `strictMcpConfig: true`, as the runner
does today. "Model saw" means the string appeared in some request body sent to the API.

### 1.1 Results

| # | Configuration | Observed |
|---|---|---|
| 1 | `settingSources: ["project"]` (today's runner) | `init.skills` has `repo-skill` plus 16 bundled skills; agents include `repo-agent`; model saw `repo-skill`, `repo-agent`, `repo-cmd` and the repo CLAUDE.md. After a `Read` of `sub/file.txt`, the next request also listed `nested-skill`: **subdirectory skills are discovered on the fly.** Project `settings.json` `env` applied (`REPO=1`). |
| 2 | `["user"]` with `CLAUDE_CONFIG_DIR=A` | **"user" means `$CLAUDE_CONFIG_DIR`**, not `$HOME/.claude`: `skills=[a-skill]`, agents `a-agent`, model saw A's `CLAUDE.md` as user memory and nothing from `$HOME/.claude`. Repo CLAUDE.md, repo skills and repo agents not loaded; no nested discovery. `A/settings.json` **applies** (`env` `PROF=pa`; a `permissions.deny` of `WebFetch` removed it from `init.tools`); the project's `env` did not (`REPO=`). |
| 3 | `["user"]` with `CLAUDE_CONFIG_DIR=B` | Only `b-skill`/`b-agent`. Skill tool on `repo-skill` from A: `Unknown skill: repo-skill`; on `b-skill` from A: `Unknown skill: b-skill. Did you mean a-skill?` |
| 4 | `["user","project"]` | Both sets load (A's and the repo's): a shared repo leaks its skills into every role. |
| 5 | `[]` with `CLAUDE_CONFIG_DIR=A` | No A skills, agents, CLAUDE.md or `settings.json` (`PROF=`, `WebFetch` present). **With `[]`, nothing written into the config dir is loaded.** |
| 6 | `[]` + `plugins: [{type:"local", path}]` | Skills and agents come qualified by the plugin's name (`prof-a:pa-skill`, `prof-a:pa-agent`, command `prof-a:pa-cmd`). Works with the plugin dir **read-only** (`chmod -R a-w`). The plugin's `hooks/hooks.json` (SessionStart, PreToolUse) **did not run** under `disableAllHooks`. The plugin's `.mcp.json` server **did not start** under `strictMcpConfig: true` (with or without `skipMcpDiscovery`); with `strictMcpConfig: false` it connected (`mcp__plugin_prof-a_plugmcp__echo`). |
| 7 | `agents: {...}` option | The listed agent is added to the built-ins (`claude`, `Explore`, `general-purpose`, `Plan`, `statusline-setup`). |
| 8 | `systemPrompt: {type:"preset", preset:"claude_code", append: <CLAUDE.md text>}` with `["user"]` or `[]` | The CLAUDE.md text reached the model. **This is how a role gets the repo's CLAUDE.md without the repo's `.claude/`.** |
| 9 | `skills: ["a-skill"]` | The listing shows only `a-skill`: bundled skills are hidden too. Skill tool on `dataviz`: `Skill dataviz is not in this session's skills allowlist`. `skills: []` rejects every skill, including the role's own. `init.skills` still lists everything (it is a context filter). |
| 10 | `tools: [Read, Edit, Write, Glob, Grep, Bash, Agent, WebFetch, WebSearch, Skill]` + `disallowedTools: [WebSearch]` | `init.tools = Task, Bash, Edit, Glob, Grep, Read, Skill, WebFetch, Write`. The default extras (`CronCreate/Delete/List`, `ScheduleWakeup`, `Monitor`, `Workflow`, `SendMessage`, `EnterWorktree`, `ExitWorktree`, `TaskStop`, `ToolSearch`, `ListAgents`, `ReportFindings`, `NotebookEdit`, `AskUserQuestion`, plan-mode tools) are gone. `Agent` appears as `Task` in `init.tools`. |
| 11 | `mcpServers: {stdiox: {type:"stdio", ...}}` | `mcp__stdiox__echo` is in `init.tools`. With a `tools` base set and no `ToolSearch`, the MCP tool's description went to the model directly (not deferred). |
| 12 | Skill frontmatter `allowed-tools: Bash(touch:*)`, mode `default`, `canUseTool` denies everything except `Skill` | After the skill was invoked, `Bash touch` **ran without `canUseTool` being called**: the skill widened the SDK's permissions. The `PreToolUse` hook **was still called** for that Bash. With the hook denying Bash, the call was denied (`PreToolUse:Bash hook error`). In `dontAsk` mode, Skill and Bash were refused outright. |
| 13 | Subagent definition `tools: [Bash, Read]` in a session whose base set is `[Read, Agent]` | `No such tool available: Bash. Bash is disabled for this session, in subagents as well as here.` **A subagent definition cannot widen the base set.** |
| 14 | **Recommended setup (§4.3), roles A and B on the same repo** | A: `skills=[role:a-design]`, agents add `role:a-reviewer`, `tools=Task, Bash, Edit, Glob, Grep, Read, Skill, Write`, no MCP. B: `skills=[role:b-design]`, `role:b-reviewer`, `tools=Glob, Grep, Read, Skill, WebFetch, mcp__stdiox__echo`. A invoking `role:b-design` or `b-design`: `Unknown skill`; B invoking `role:a-design`: `Unknown skill`. The repo CLAUDE.md reached both (append). No repo skill, agent or command; no nested discovery after reading `sub/`. |
| 15 | Sessions per config dir | A persisted session under config dir A: `listSessions` from A lists it; from B the list is empty. **Sessions belong to one role when each role has its own config dir.** |
| 16 | Symlinked `skills/` and `agents/` in a config dir with `["user"]` | Loaded through the symlinks (targets read-only). Not used by the design, because the agent can replace a symlink in a writable config dir. |
| 17 | `ANTHROPIC_BASE_URL=<local>` + `CLAUDE_CODE_OAUTH_TOKEN=<placeholder>`, no API key | Requests went to `<local>/v1/messages` with `Authorization: Bearer <placeholder>` and `anthropic-beta: ...oauth-2025-04-20...`; `init.apiKeySource = "none"`. No other paths were requested. **The auth gateway only needs to replace the bearer token.** |

### 1.2 Conclusions used by the design

1. Use `settingSources: []` for every role. The repo's `.claude/settings.json`, skills, agents, commands, nested skills
   and the config dir's own files are then all ignored. This also closes the known gap in agent-runner.md: the
   project's `env` no longer applies.
2. Give a role's skills and agents to the session as **one SDK local plugin on a read-only mount** (#6, #14). Do not use
   a writable `CLAUDE_CONFIG_DIR` with `["user"]`: the agent could edit `skills/` or `settings.json` there (#2, #16).
3. Always pass `skills: [<role skill names, qualified role:...>]`. It hides the 16 bundled skills and rejects anything
   else (#9). With no skills, pass `[]`.
4. Always pass `tools: <role base set>` (#10) and keep `strictMcpConfig: true` (#6). MCP servers come only from
   `mcpServers` (#11).
5. Skill `allowed-tools` **does** widen the SDK's own permission check (#12). It does not get past our `PreToolUse` gate,
   which asks `perm.ask` for every call and never returns `allow`. Defence in depth: Home strips every frontmatter key
   except `name` and `description` when it materializes a role (§6.2).
6. **Inline shell in skills runs outside the gate (observed, real `claude`).** A skill body with ``!`cmd` `` (or a
   fenced ```` ```! ```` block) is expanded by the CLI itself when the skill is invoked: no `PreToolUse` hook, no
   `perm.ask`. In `bypassPermissions` the commands ran (marker files created); in `default` mode read-only commands
   ran and a command needing a prompt failed the whole skill, still with no ask. The runner therefore passes
   `disableSkillShellExecution: true` in role mode (the commands are replaced by a placeholder), the scanner rejects
   the syntax (`hid.shell`), and the integration suite asserts none of the forms runs in either mode.
7. CLAUDE.md goes through `systemPrompt.append` (#8). Loss: `@imports` and subdirectory `CLAUDE.md` files are not
   followed (§4.4).

### 1.3 Not verified (flagged)

- `AGENTS.md`: the built-in plugin `cc-plugin-agents-md` loads in every configuration; whether it reads a repo
  `AGENTS.md` under `settingSources: []` was not tested. That is repo content shared by all roles, not a skill leak.
- WebFetch's network path in a real VM: assumed to be client-side through `HTTPS_PROXY`, possibly with a domain
  preflight to an Anthropic host. WebSearch is a server tool run by Anthropic: it goes through the auth gateway, not the
  CONNECT allow-list. To confirm with a live run (R3).
- In-process SDK MCP servers (`createSdkMcpServer`): not re-tested here. The runner's design already plans them for
  `rubi_reply`/`rubi_ask`.

## 2. Data model

### 2.1 Role (stored by Rubi Home, per device)

```json
{
  "id": "r_7f3k2m9q",                 // stable, [a-z0-9_], Home-generated
  "project": "web-app",               // an agent project id on this device
  "name": "visual-design",            // [a-z0-9-], 1-32, unique per project among non-archived roles
  "description": "Panel and site visuals",
  "status": "active" | "archived",
  "version": 12,                      // monotonically increasing; every change makes a new immutable version
  "parent": 11,
  "instructions": "You own the visual design...",   // role prompt, <= 8 KiB, appended to the system prompt
  "project_instructions": true,       // append /work/CLAUDE.md (and /work/.claude/CLAUDE.md)
  "skills": [
    {"name": "design-tokens", "source": "library", "library_id": "design-tokens", "pin": null, "resolved": "3",
     "sha256": "…", "sig": "…"},       // pin null = follow latest admitted; resolved = the version materialized
    {"name": "layout-notes", "source": "adhoc", "sha256": "…", "added_by": "bot-a", "scan": "pass", "state": "active"}
  ],
  "agents": [{"name": "reviewer", "description": "...", "prompt": "...", "tools": ["Read", "Grep"], "model": "inherit"}],
  "tools": ["Read", "Edit", "Write", "Glob", "Grep", "Bash", "Agent", "Skill"],
  "deny": [{"rule": "Bash(git push:*)", "by": "user"}],
  "mcp": [{"name": "docs", "type": "http", "url": "https://mcp.example.com/", "approval": "ap_…"}],
  "plugins": [],                      // Claude plugins with code, tier 2 only
  "hosts": ["cdn.example.com"],       // extra proxy hosts for this role only (tier 2)
  "ssh": ["web-1"],                   // granted SSH target names (tier 1 for registered targets)
  "default_mode": "default",
  "created_by": "bot-a", "created_at": "…", "changed_by": "bot-a", "changed_at": "…",
  "change": {"op": "role.skills.set", "tier": 1, "approval": null, "summary": "+layout-notes"}
}
```

Each project also gets an implicit **legacy role** `default` (id = the project id). It keeps today's behaviour:
`settingSources: ["project"]`, config dir `/work/.rubi/claude`, existing sessions. The panel marks it "uses the
repository's own .claude (shared with everyone working in the repo, not isolated)" and offers "Convert to an isolated
role" (§4.6). The legacy role cannot be curated by Bots: they create new roles instead.

### 2.2 Where each piece lives

| Data | Home (Mac, `<helper dir>`) | Plugin (Rubi vault state) | Core (Rubi) |
|---|---|---|---|
| Role records and all versions | `agent-roles/<id>/versions/<n>.json` (0600, immutable) and `index.json` | no (it only caches the list) | no |
| Ad-hoc skill content | `agent-roles/<id>/blobs/<sha256>` (content-addressed) | no | no |
| Materialized content | `agent-roles/<id>/content/v<n>/` and the `current` symlink | no | no |
| Per-role Claude config (sessions) | `agent-roles/<id>/claude/` (mounted read-write) | no | no |
| Tier policy, per-Bot limits, mode ceiling | no | config fields (user-only) | no |
| Audit (who, what, tier, approval, version numbers) | a short `history` in each version | audit entries (`audit` host method) | Rubi's audit log |
| Pending tier-2 requests | no | state (bounded: 32) | approvals |
| Skill library (entries, versions, content, signatures) | verifies signatures with the pinned library key | no | `internal/library` (encrypted under the vault DEK) |
| Library curator binding | no | no | core setting, `rubi.library.curator` (strong, locked) |
| SSH targets, private keys, host keys | `agent-ssh/` (0700; keys 0600) | target names and grants only (mirrored) | no |

Reasons:
- Projects and VMs belong to one device, and Home needs the files to mount anyway.
- Plugin state is one encrypted blob limited by the 4 MiB message size; skill content and version history would outgrow
  it.
- The library must be in core so that "admitted" can be signed by a key no plugin holds (§8).
- Home already trusts the paired plugin's decisions for `perm.ask`. It cannot tell a Bot tool from a panel button, so
  the tiers are enforced in the plugin, as they are for `usage_mode` today. Home enforces the **structural invariants**
  whatever the plugin says: schema, sizes, the stripped frontmatter, tool names from a known list, library skills only
  with a valid signature, hidden text, secrets and override or widening claims in every free text (§7), and:
  - **approvals, compared with the previous version.** A version that adds an MCP server (or changes one), a plugin, a
    host, a tool outside `SafeTools` or an accepted skill flag needs a non-empty approval id for each addition: the
    entry's own `approval` for an MCP server or plugin, `change.approval` for the rest. An id is **spent** by the
    version that commits with it (kept in `used.json`, rebuilt from the version files) and cannot authorize another
    version, a changed entry or another entry; a revert that re-adds something needs a fresh id.
  - **labels survive.** What a put leaves unchanged keeps `deny.by`, `added_by`, accepted flags and an MCP server's or
    plugin's `approval` from the previous version; what is new is labelled with the put's own `by`.

  Home does not check that an approval id is real, pending or granted, or who the caller is: the plugin stays the tier
  authority (like `perm.ask`). These checks stop a put from widening or relabelling without naming a fresh approval; they
  do not replace the plugin's.

### 2.3 Home operations (capability `claude-agent`, new ops)

- `role.list {project?}`, `role.get {id, version?}`, `role.history {id, limit}`, `role.diff {id, from, to}`
  (structured diff plus unified text diffs of skill content, clipped).
- `role.put {id?, base_version, spec, blobs: {sha256: content}}` -> `{id, version}`. Home re-validates and re-hashes,
  strips the frontmatter, verifies library signatures, refuses on a `base_version` mismatch (optimistic concurrency),
  then materializes. `role.put` is the only write op; archive, rename and revert are all puts.
- `start`, `send`, `sessions` and the others take `role` (a role id) besides `project`. A missing `role` means the
  project's legacy role, so older plugins keep working.
- `status` rows gain `role` and `role_name`. `limits` gains `per_profile`.

## 3. VM per role

One VM per active role (container `rubi-agent-<role-id>-<64 bits of hash(dir|role)>`; the name never follows a rename, and a start also removes containers of the older naming). Reasons:
- The proxy allow-list (`hosts`) and SSH grants are per role. One VM per project would let role A's Bash read role B's
  proxy credential from `/proc` (same user in the VM).
- Mounts are per role: only its own content and config dir.
- The runner's `listSessions` reads the config dir of its own process (#15).

Mounts of a role VM (the legacy role keeps today's):

| Mount | Source (Mac) | Mode | Purpose |
|---|---|---|---|
| `/work` | the project dir | rw | as today |
| `/rubi-role` | `<helper>/agent-roles/<id>/content/` | ro | `current -> v<n>`; each `v<n>/` holds `role.json` and `plugin/` (`.claude-plugin/plugin.json` named `role`, `skills/<name>/SKILL.md`, `agents/<name>.md`) |
| `/rubi-claude` | `<helper>/agent-roles/<id>/claude/` | rw | `CLAUDE_CONFIG_DIR`: sessions, `.claude.json`, backups. No credentials (auth gateway). Nothing in it is loaded (#5). |

- **The project is shielded from a role.** Nested binds on top of the read-write `/work` work with container 1.6.1
  (checked live): `/work/.rubi` is covered by an empty read-only directory (`<helper>/agent-role-empty`, so a role never
  sees or rewrites the legacy role's state) and the project's own `.claude` is bound read-only when it is a plain
  directory (a symlink or a missing one is not bound: the tool would create the mount point in the project). A single
  file can't be bound (the tool refuses a file as source, and a directory over a file), so **`/work/CLAUDE.md` stays
  writable by a role, as does a `.claude` that doesn't exist yet**: a residual, known. A role bound to a different
  directory than the one its project id names now (ids come from directory names) is refused, and a role records the
  hash of its project's directory when it is made, never from the caller.
- **Session ids stay inside the role.** Home refuses send, messages, rename, fork, interrupt, close and mode for a session
  that is neither in the role's slots nor in its runner's session list, and the runner resumes from disk only ids its own
  config directory lists. Reads (`sessions`, `messages`) of a role whose VM is stopped answer empty with `vm_state:
  "stopped"`: they never start a VM or keep one up.
- **Revocation.** A put replaces the running VM's proxy allow-list (tunnels to removed hosts are closed). Archiving
  stops the VM when nothing runs; a turn that runs continues; an archived role takes no new start or send.
- **Outside `/work`.** Today's `/work/.rubi/claude` is visible to every VM that mounts the project. A second role could
  read or tamper with another role's transcripts there, and a resumed session would read the tampered history.
- **`current` is a relative symlink inside the read-only mount.** Home writes `v<n+1>/`, then atomically renames a new
  symlink over `current`. The runner resolves `current` with `realpath` at each query start, checks that the result
  stays inside `/rubi-role`, and pins that version for the query. A change applies at the next query start (a new
  session, or a resume after a restart). Home keeps the last 5 versions plus any version a live query uses.
- **Cost.** The container default is 1 GiB and 4 CPUs per VM (`container system property list`). Two roles on one
  project mean two VMs. The existing "cap on running VMs" item (not in M2) becomes relevant; add an idle stop:
  - stop a role VM after 15 minutes with no slot and no queue item;
  - config `agent_role_vm_idle_minutes`.
- **Two VMs on one `/work`.** Concurrent git operations can collide on `index.lock`. That is acceptable for a default
  of one session per role. The panel shows "another role is working in this project".
- **Never pass `--ssh`** to `container run`: it would forward the user's SSH agent. Add a test that `RunArgs` never
  contains it.

## 4. Runner changes

### 4.1 Environment

- `RUBI_ROLE_DIR=/rubi-role` switches the runner into role mode. Unset means legacy mode.
- `CLAUDE_CONFIG_DIR=/rubi-claude` (for the runner process too, as today).
- `RUBI_AUTH_DIR` and `readSharedToken` go away with the auth gateway. `ANTHROPIC_BASE_URL` and the placeholder
  `CLAUDE_CODE_OAUTH_TOKEN` are copied into the child env.

### 4.2 `role.json` (written by Home, read by the runner at each query start)

```json
{"role": "r_7f3k2m9q", "version": 12, "skills": ["design-tokens", "layout-notes"],
 "tools": ["Read", "Edit", "Write", "Glob", "Grep", "Bash", "Agent", "Skill"],
 "deny": ["Bash(git push:*)"], "mcp": {"docs": {"type": "http", "url": "https://mcp.example.com/"}},
 "instructions": "…", "project_instructions": true, "default_mode": "default"}
```

Validation: at most 64 KiB, a symlink is refused (`lstat`), unknown keys are ignored, tool names must come from the
runner's known list. If the file is missing or bad, the runner refuses `session.start` with an error. It never falls
back to legacy mode.

### 4.3 Query options in role mode (the verified setup, #14)

```ts
settingSources: [],
plugins: [{ type: "local", path: `${ver}/plugin`, skipMcpDiscovery: true }],
skills: role.skills.map((n) => `role:${n}`),        // [] when the role has none
tools: role.tools,                                  // the base set; "Agent" (reported as Task)
disallowedTools: [...disallowedEditTools("/work"), "Edit(//rubi-claude/**)", "Read(//rubi-claude/**)", ...role.deny],
strictMcpConfig: true,
mcpServers: role.mcp ?? {},
systemPrompt: { type: "preset", preset: "claude_code", append: instructionsBlock(role) },
settings: { disableAllHooks: true, apiKeyHelper: "" },   // unchanged
// the hooks, canUseTool, env, permissionMode and the rest are as today
```

`Skill` is added to `tools` only when the role has skills. `AskUserQuestion` is always added: it is answered through the
plugin, and it widens nothing. Plan-mode tools are added when the session's mode is `plan`.

### 4.4 CLAUDE.md

`instructionsBlock`:
1. the role's `instructions`;
2. when `project_instructions` is set, the text of `/work/CLAUDE.md`, then `/work/.claude/CLAUDE.md`, each read with
   `lstat` (no symlinks) and capped at 64 KiB together, under a heading saying it is project text written in the repo.

Limitation: `@import` lines and `CLAUDE.md` files in subdirectories are not followed. They could be resolved later,
staying within `/work` and the size cap. `CLAUDE.md` is agent-writable, as today.

### 4.5 Sessions

`session.list` and the related calls read `/rubi-claude` (per role, #15). `session.fork` stays inside the role.

### 4.6 Converting a legacy role

The panel button runs `role.put` for a new isolated role on the same project, then Home copies (never moves)
`/work/.rubi/claude/projects/<key>/*.jsonl` into the new config dir. The old directory stays in place.

## 5. Slots

- New `agent_max_sessions_per_profile` (Home config, default 1, at least 1) replaces the per-project limit as the
  default rule.
- `agent_max_sessions_per_project` stays as an optional extra cap: unset or 0 means no cap. An explicit value set today
  keeps working.
- The computer's `agent_max_sessions` is unchanged: default 4 (owner decision), 0 = no cap.
- Per-Bot mode limits are unchanged.
- Queue reasons gain "waiting: role visual-design (web-app) already has 1 session".
- `fullLocked` counts by role. A queue item keeps `role`; items of one role and Bot keep their order.
- Raising any cap is tier 2: only the user, through a panel button with a strong approval. This uses the same kind of
  Home op as `usage_mode` (`claude-agent.limits`), which is never exposed as a Bot tool.

## 6. Tiers

### 6.1 Operations (plugin tools for Bots; the Bot identity comes from `rubi_verified_agent`)

| Tool | Change | Tier | Approval kind (level) |
|---|---|---|---|
| `claude_code_role_create {project, name, description?, from_role?}` | New role on a project where the Bot holds a live project trust (`projectTrust.Sister == bot`, `Project == project`) | 1 | none (audited) |
| same | On a project without such a trust | 2 | `claude-code.role_foreign` (strong, locked) |
| `claude_code_role_update {role, name?, description?, instructions?, project_instructions?, default_mode?}` | rename, text, mode up to the ceiling | 1 | none |
| same with `default_mode` above the ceiling | | 2 | `claude-code.role_widen` (strong, locked) |
| `claude_code_role_archive {role}` / unarchive | | 1 | none |
| `claude_code_role_skills_set {role, skills}`, `claude_code_role_skills_add {role, library: "id[@version]"}`, `..._remove` | ad-hoc content-only skills (scanned, §7); library skills (signed, §8) | 1 | none; a skill flagged by the scan is held until the user accepts it with `claude-code.role_skill_accept` (tap, locked) |
| `claude_code_role_tools_set {role, tools, deny?}` | tools from the safe set: Read, Edit, Write, Glob, Grep, Bash, Agent, Skill, WebFetch, WebSearch; adding deny rules; removing deny rules a Bot added | 1 | none |
| same with a tool outside the safe set (Cron*, ScheduleWakeup, Workflow, Monitor, SendMessage, worktrees), or removing a deny rule the user set | | 2 | `claude-code.role_widen` |
| `claude_code_role_agents_set {role, agents}` | subagent definitions (cannot widen the base set, #13) | 1 | none |
| `claude_code_role_mcp_set`, `claude_code_role_plugins_set`, `claude_code_role_hosts_set` | MCP servers, Claude plugins, extra network hosts | 2 | `claude-code.role_widen` |
| (any) secrets, credentials, vault entries for a role | not offered in v1 | 2 | none |
| `claude_code_role_ssh_grant` / `_revoke {role, target}` | a registered target | 1 | none (audited); revoking cuts live streams |
| `claude_code_ssh_target_register` (panel only) | new target: host, key, network | 2 | `claude-code.ssh_target` (strong, locked) |
| per-Bot limits, session caps, mode ceiling | user config fields | 2 | the plugin config's strong approval |
| git push or mandate scope | unchanged (`cred.ask`, project trust) | 2 | existing kinds |
| `claude_code_role_start {role, prompt, name?}` | session for a role | as `claude-code.start` today: strong, or none with a live project trust for that project | |
| `claude_code_role_list {project?}`, `claude_code_role_get {role, version?}`, `claude_code_role_history {role}` | read | none | |

Return values:
- `{status: "applied", role, version, audit_id}`
- `{status: "needs_approval", approval_id, link, pending: {role, base_version, changes}}`
- `{status: "rejected", reasons: [...]}`

A request is **atomic**: if any part is tier 2, nothing is applied and the whole change waits for approval. When the
approval runs, the plugin rebases the change on the current version. If the parts the change touches have moved since,
it fails with "the role changed since you asked; ask again" and is never half-applied.

**Tier is computed from the diff, not from the tool name.** The plugin compares the current version with the proposed
one. Any growth in `mcp`, `plugins` or `hosts`, a new non-safe tool, a removed user deny rule, a mode above the ceiling,
or an ungranted-to-granted change on an unregistered target makes the change tier 2. Content changes are tier 1. A skill
can carry no grants: the frontmatter is stripped, and the body is only context.

### 6.2 What a skill can never do (structural, enforced by Home)

- Materialized `SKILL.md` frontmatter keeps only `name` and `description`. `allowed-tools`, `model`, `hooks`,
  `context`, `agent`, `disable-model-invocation` and anything else are removed, and the removal is recorded in the scan
  result.
- One file per skill, no scripts.
- Agent definitions are rebuilt by Home from fields: `name`, `description`, `prompt`, `tools` intersected with the
  role's tools, `model` in {inherit, sonnet, haiku, opus}. No `mcpServers`, `hooks` or `skills`.

### 6.3 Mode ceiling

User config field `role_mode_ceiling` (default `bypassPermissions`, owner decision: the VM is the boundary, and the user's
per-Bot usage mode and the network, secret and SSH approvals stay as designed). Ladder: `plan` < `default` < `acceptEdits` < `auto` <
`bypassPermissions`. `dontAsk` is not offered for roles: Home treats it as loose, the SDK as strict. Home's
`agent_allow_bypass` remains the hard floor.

### 6.4 Per-Bot limits (user config, defaults)

| Field | Default |
|---|---|
| `role_max_per_project_per_bot` | 5 |
| `role_max_per_bot` | 20 |
| `role_changes_per_hour_per_bot` | 30 |
| `role_max_skills` | 20 per role |
| `role_max_adhoc_skills` | 10 per role |
| `role_max_total` | 50 per device (also bounds the plugin's cache) |

Over a limit, the call fails with the limit named. Raising a limit is a settings change.

### 6.5 Audit and one-click revert

- Every applied change writes a plugin audit entry `role.change {role, project, from, to, by, tier, approval?,
  summary}` (no content) and is stored in Home as an immutable version.
- The panel's role history page lists versions with the diff (`role.diff`) and a **Revert to this version** button.
- A revert is a new version, never a history rewrite. A revert whose diff from the current version widens anything
  (for example back to a version that had an MCP server) asks for the strong approval; any other revert is the user's
  own request and needs none.
- Bots may revert their own tier-1 changes through `claude_code_role_update {revert_to}` under the same rule.

## 7. Skill scanning (deterministic, `sdk/skillscan`, used by core, plugin and Home)

The scanner lives in the core module's `sdk/` so the plugin (separate repo) can import it, like `sdk/rubiplugin`.
Each check gives one of three results:
- **reject**: never activated, and no override exists;
- **flag**: needs a person (the user for ad-hoc skills) or the curator (library) to acknowledge each flag;
- **pass**.

**Reject:**
- Format: not UTF-8, NUL or control characters other than `\n` and `\t`, more than 32 KiB, more than 800 lines, a name
  not matching `^[a-z0-9][a-z0-9-]{0,47}$`, a description missing or over 300 characters, frontmatter that is not a
  simple `key: value` block.
- Hidden text: invisible characters (any format, private-use, surrogate, line/paragraph-separator category, every space
  other than U+0020, variation selectors U+FE00-U+FE0F and U+E0100-U+E01EF, U+034F, the Hangul fillers U+115F, U+1160,
  U+3164, U+FFA0, U+FFF9-U+FFFB, U+2800, U+180E, U+061C, U+00AD; a zero-width joiner too, even inside emoji), bidi
  controls (U+202A-U+202E, U+2066-U+2069), Unicode tag characters (U+E0000-U+E007F), ANSI escapes, HTML comments,
  inline shell (``!`cmd` `` and ```` ```! ```` blocks, `hid.shell`), `data:` URLs, base64 or hex runs of 200
  characters or more.
- The frontmatter is read as written: a double-quoted value with a backslash escape is refused, so what is scanned is
  what is emitted. Every per-line rule also runs over the decoded description. `name` and `description` are written back
  single-quoted (so `name: yes` stays a string), and so are the names and descriptions of subagent files.
- Secrets: `sk-ant-`, `ghp_`/`gho_`/`github_pat_`, `glpat-`, `xox[abposr]-`, `AKIA[0-9A-Z]{16}`, `AIza[0-9A-Za-z_-]{35}`,
  `npm_[A-Za-z0-9]{36}`, PEM private-key headers, `Bearer <20+ token characters>`, `password|secret|token\s*[:=]\s*\S{8,}`,
  and generic strings of 32+ characters with entropy above 4.0 bits per character. These reuse the
  `secretIn`/`secretName` lists from the plugin's `mandate.go`, moved into `skillscan`.

**Flag:**
- Exfiltration shapes:
  - network tools (`curl`, `wget`, `nc`/`ncat`, `socat`, `scp`, `rsync`, `ssh`, `ftp`, `telnet`, `dig`/`nslookup`
    with `$(` or backticks) together with upload or data options (`-d`, `--data*`, `-T`, `--upload-file`, `-F`, `@file`)
    or a pipe into them;
  - credential and auth paths and names (`~/.ssh`, `id_rsa`, `id_ed25519`, `.netrc`, `.git-credentials`,
    `/proc/*/environ`, `printenv`, `env |`, `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_`, `/rubi-claude`, `.rubi/`,
    keychain);
  - any URL (`http`, `ftp`, `ws`, `git`, `ssh`, `s3`, `tcp`, `udp`, `sftp`, `rsync`, `smb`) whose host is not in the
    role's allow-list or the base list, and a schemeless `host/path`, `host:port` or common-TLD host argument of `curl`
    or `wget` (a heuristic: `out.txt` is not a host).
- Instruction overrides: "ignore (all|previous|prior) instructions", "system prompt", "do not tell the user", "without
  asking", "bypass", "disable (hooks|approvals|sandbox)", "dangerously", "approve" next to "Rubi", "passkey".
- Widening claims: `allowed-tools`, `permissionMode`, `bypassPermissions`, `mcpServers`, `settings.json`, `.claude/`,
  `--plugin-dir`, "allow-list", `agent_allowed_hosts`, "add (this|the) host".

**Free texts.** A role's instructions, a subagent's prompt and the role and subagent descriptions go through
`skillscan.ScanText` (the same per-line rules, no frontmatter or size rules). They have no held state, so Home refuses
the put on **any** hit, reject or flag; a URL passes once its host is in the role's hosts (a widening that has its own
approval), anything else is reworded. All text fields also refuse invisible characters and C1 controls.

**Limits that keep the store bounded:** `role.json` over 64 KiB is refused before any blob is written; a role keeps at
most 4 MiB of skill text (every version's skills count, they stay as history). A stored version that the scanner would
now reject (a later scanner version) is rebuilt without that skill and logged, so the role is never unusable and a put
or revert that removes the skill works.

**Stripped (recorded, not rejected):** frontmatter keys other than `name` and `description`.

The result is stored with the skill: `{verdict, rules: [{id, line, excerpt <= 80 characters}], stripped: [...],
scanner_version}`.

**Activation:**
- Ad-hoc skill from a Bot, scan pass: active at once, tagged `added_by`, shown as "unreviewed", with "Submit to library".
- Scan flag: `held` until the user taps accept.
- Scan reject: `rejected` with the reasons.

**Honest limit:** paraphrased instructions get past any keyword scan. The real protections are structural: no grants
from skills (§6.2), every call through `perm.ask`, egress only through the allow-list and the auth gateway, and no
credentials in the VM. An optional model review can be added later; it is not in v1.

## 8. Shared skill library (Rubi core, `internal/library`)

- **Entry:** `{id, versions: [{version (1, 2, ...), sha256, content, description, tags, author_bot, submitted_at,
  admitted_by, admitted_at, scan, state: proposed|admitted|rejected|deprecated|withdrawn, sig}], latest_admitted}`.
  Versions are immutable. The store is encrypted under the vault DEK like other core data. Limits: 32 KiB per version,
  500 entries, 20 versions per entry.
- **Admission:**
  1. `skillscan` runs on submit (reject is final: the version is kept as `rejected`, without its text, and can never
     be admitted). The description and tags get `ScanText`.
  2. The curator Bot calls `admit` and must acknowledge every flag by rule id. The scan runs again at admission.
  3. Core signs Ed25519 over `"rubi-skill-library-v1\n" + canonical JSON {id, version, sha256, admitted_by,
     admitted_at, scan_verdict, scanner_version, self_admitted}` (field order fixed, no spaces; `internal/library/libsig`).
     The signature a role carries (`sig`) is one token: URL-safe base64 of a JSON object with those admission facts
     and the signature, at most 600 characters. Home rebuilds the message from the skill's own id, version and
     sha256 plus the token's facts, so none of them can be changed.
- **Signing key:** a new dedicated **library key** (Ed25519 seed in the vault, made on first use), usable only while
  Rubi is unlocked. It is not the instance identity key (stored unencrypted) and not the release key (CI-only).
- **Store:** `<rubi home>/library/<id>.enc`, one file per entry, XChaCha20-Poly1305 under a key derived from the vault
  DEK (HMAC-SHA256 with a fixed label), the instance and entry id as associated data; 0600. Nothing is readable on disk
  or while Rubi is locked, and a file moved to another id or edited fails to open. Limits: 32 KiB per version (a
  scan reject beyond that), 500 entries, 20 versions per entry (rejected ones count), 8 tags, 10 submissions per Bot
  per minute. Proposal ids are `id@version`.
- **Pinning on Home (resolved in R8):** Home pins the library key's public half on the op `library.key`
  `{public_key}` (base64), which only core sends. The first key wins; the same key again is accepted; a different one
  is refused ("pair with Rubi again to change it"); a new pairing forgets the pin. Core sends it to a newly paired Home
  right after the pairing is approved, and again (at most every 10 minutes per Home, best effort) whenever a plugin
  calls `library.get`, so homes paired before the library existed pin it too.
- **Origin question, answered:** Home **cannot** tell a core-originated op from a plugin's `home.call` relayed by
  core. Both arrive in the same envelope with the same pairing token, and Home has one token per Rubi. What
  separates them is core's own filter in `homeCall`: an op is forwarded only if it starts with `<capability>.` for a
  capability in the plugin's manifest, and the manifest validator accepts only `hue`, `shortcuts`, `claude`,
  `claude-sessions`, `claude-start`, `claude-agent` and `agent-setup`. `library.key` starts with none of them, and a
  manifest cannot declare a `library` capability, so no plugin can make core send it; only core's own
  `pinLibraryKey` does. Tests: `TestPluginCannotCallTheLibraryKeyOp` (every capability declared, odd spellings, the
  manifest check, `home.call` through the plugin handler) and `TestLibraryKeyPinning` (Home side). The consequence: the
  pin is as trustworthy as core's filter, and a plugin that is buggy or compromised cannot pin its own key; it still
  could not forge a signature without the library key.
- **What the signature buys:** a buggy or compromised plugin cannot make Home label an unreviewed skill "library", and
  library content altered at rest is detected. It does not protect against someone who controls Rubi's machine while
  Rubi is unlocked (security model §3).
- **Curator:** core setting `library_curator` (one Bot name). Assigning or changing it is `rubi.library.curator`
  (strong, locked). The curator may admit, publish new versions, deprecate (no new attachments) and withdraw (active
  roles drop the skill at their next materialization; audited per role). Other Bots only submit into the queue.
  A curator may admit its own proposal (owner decision): the automatic checks still run and must pass, the version is
  marked with the curator as author, and the panel and audit show "self-admitted by curator" (the flag is also in the
  signature). All curator actions go to Rubi's audit log (`library.submitted`, `library.admitted`, `library.published`,
  `library.deprecated`, `library.withdrawn`, `library.curator_set`, `library.refused`). "Revert" is a new admit or
  un-withdraw (`admit` on a deprecated or withdrawn version signs it again), never an edit.
  The curator is set with `Core.RequestLibraryCurator(name)`: a strong approval of kind `rubi.library.curator`
  showing the old and new curator (`""` removes it), applied only if the curator is still what the user saw. The
  panel button is in Settings, "Skills" (R10).
- **Bot tools (core MCP):**
  - `rubi_library_list {query?, tag?}`, `rubi_library_get {id, version?}`, `rubi_library_submit {id?, description,
    tags, content}` (returns `proposal_id` and the scan);
  - curator only: `rubi_library_admit {proposal_id, ack: [rule ids]}`, `rubi_library_update`,
    `rubi_library_deprecate`, `rubi_library_withdraw`.
  - Core knows the calling Bot itself, so the curator check is core's: every tool that writes takes `agent` (the Bot's
    name) and `code` (the `agent_code` of its latest Rubi webhook, the same proof `rubi_events` uses) and core
    compares the registered name with `library_curator`. `list` and `get` need no proof; with it the curator also sees
    proposals, rejections and withdrawn versions (others get the same "no admitted skill" for anything not
    admitted or deprecated). `get` without a version means the latest admitted; `update` is the curator's new
    version submitted and admitted in one step (always self-admitted); `deprecate` and `withdraw` take an optional
    `version` (default every version that applies).
  - The plugin gets the read-only host method `library.get {id, version?}` (only a plugin whose manifest declares the
    `claude-agent` Rubi Home capability) returning `{id, version, state, content, sha256, signature, description,
    tags, admitted_by, admitted_at, self_admitted, scan_verdict, scanner_version, latest_admitted}` for an admitted or
    deprecated version, to build `role.put`. A proposed, rejected or withdrawn version is an error. There is no host
    method that writes.
  - Home's check is `roles.Pinned`: no pinned key, a signature for another id, version or hash, a key that is not the
    pinned one, or a verdict other than pass or flag all refuse the library skill.
- **In roles:** `claude_code_role_skills_add {role, library: "id"}` follows the latest admitted version;
  `"id@3"` pins it. Attaching is tier 1, since admitted means trusted. When a new version is admitted, the plugin
  re-materializes each following role as a new role version (audit: "design-tokens 3 -> 4", diff, Revert).
  Updates never change tools or network.
- **Panel (R10):** Settings, "Skills" opens the library screen: search and tag filter, an entry page with every version
  (state, scan rules with line and excerpt, author Bot, admitted by, "self-admitted by curator", hash, dates, the skill in
  a text-only `<pre>`), the curator queue (proposals with their scan results) and the curator with a Change button
  (prompts for a Bot name, then the strong approval `rubi.library.curator`). Panel ops: `library.list`, `library.entry`,
  `library.version`, `library.queue`, `library.curator.set`; a full settings ticket only. The user does not admit,
  deprecate or withdraw from the panel in v1: the core has no user path for it (it is the curator Bot's job), so the
  screen is read-only apart from the curator. All Bot-supplied text is drawn as clipped text and marked untrusted.
  "Used by roles" is not shown: roles live in Home and the plugin, the core cannot see them, so it would need a plugin
  call from the panel; left for later.

## 9. SSH targets

Same pattern as the auth gateway: the secret stays in Home, and Home does the network work.

### 9.1 Registration (tier 2, panel only)

1. The user enters name, host, port, user and optional allowed commands (prefix patterns).
2. Home connects from the Mac (not through the VM proxy), reads the host key and shows its SHA256 fingerprint. The user
   compares it with the server's out of band, or pastes the expected fingerprint.
3. The strong approval `claude-code.ssh_target` carries `{name, host, port, user, fingerprint, allowed_commands,
   ask_each_time}`.
4. On execution, **Home generates** an Ed25519 key pair: `<helper>/agent-ssh/keys/<target>` (0600, directory 0700). The
   private key never passes through Rubi, the plugin, the panel or a VM.
5. The panel shows the public key as an `authorized_keys` line with `restrict` (and `command=` when exactly one command
   is allowed) for the user to install on the target.

A file is used rather than the Keychain because a LaunchAgent's Keychain access depends on the binary's code signature
and would prompt after updates. The Keychain is an option once Rubi Home is signed with a stable identity.

### 9.2 Grants

`role.ssh` lists target names (tier 1, audited). Revoking closes that role's live streams to the target at once.

### 9.3 Inside the VM

`/usr/local/bin/rubi-ssh`, linked as `ssh` early in `PATH`, takes `ssh <target> [command...]`. It refuses `-o`, `-i`,
`-L`, `-R`, `-D`, `-A` and the `user@host` form. Only target names exist; there is no TTY in v1. It speaks to the runner
over a unix socket `RUBI_SSH_SOCK` (0600, as `git-credential-rubi` does).

### 9.4 Runner to Home (new JSON-RPC, over the runner channel)

- request `ssh.open {target, command, session_id, tool_use_id?}` -> `{stream}`
- notifications `ssh.data {stream, fd, b64}` (both ways), `ssh.close {stream}`, `ssh.exit {stream, code}`

### 9.5 Home's checks on `ssh.open`

1. The target is granted to **this VM's role** (Home knows the role from the VM; the runner's claim is not trusted).
2. The command matches `allowed_commands` when set.
3. With `ask_each_time`, an `ssh.ask` item goes to the plugin and needs a `decide`.
4. Home dials with `golang.org/x/crypto/ssh` (already in go.mod), `HostKeyCallback = FixedHostKey(pinned)`. A mismatch
   is refused, audited and shown in the panel as "host key changed".
5. Home runs `command` on an exec channel and relays the streams.

Limits: 4 streams per role, 2 h per stream, 10 minutes idle, 8 MiB buffered per direction (backpressure). The Bash call
running `ssh web-1 ...` is itself gated by `perm.ask`; the plugin classifies `ssh <target>` as a remote op (always
asks, unless a mandate covers it). That is two independent checks.

### 9.5.1 As built (R11, Home side)

Package `internal/home/agent/sshtarget` (registry, keys, pinned client, relay, audit) and `internal/home/agent/ssh.go` (the
seam to the Manager). The wire contract and the `ssh.scan/register/list/remove/audit` operations are in `agent-runner.md`
("SSH relay (R11)"). Decisions beyond the text above:

- Home checks the grant from the role record (`role.ssh`, current version, role not archived) at `ssh.open`, again after an
  `ask_each_time` approval, and every 2 s on a live stream; a `role.put` and `ssh.remove` also cut at once.
- Registration pins the key found at registration time; the call fails when its fingerprint is not the one the user gave.
  Dials offer only the pinned key's type as host key algorithm, so a server with several host keys is not a false alarm.
- Output is paced by acknowledgements (`ssh.ack`), so the 8 MiB bound is real backpressure; input over the bound cuts the stream.
- `allowed_commands` prefix patterns (`tail -n *`) accept only plain characters after the prefix; anything with shell
  operators needs an exact entry.
- The audit is `agent-ssh/audit.jsonl` (rotated at 1 MiB), including refusals (result `refused`, `denied`, `host key
  changed`). Plugin-side audit entries are the plugin's to add from `ssh.audit`.
- Removing a target does not edit the role records that name it; a later target registered under the same name would be
  granted again. So `ssh.remove` keeps a tombstone of the name (persisted in `targets.json`, never expires), and
  `ssh.register` refuses a tombstoned name unless the call carries `reuse:true`, which the plugin sends only after a
  strong approval that says which roles still list the name.

### 9.5.2 As built (R12, runner and image)

The wrapper, its framing, the socket and the relay rules are in `agent-runner.md` ("In the VM: the `ssh` wrapper and the runner
socket"). Decisions: the wrapper is a node script (no netcat, no ssh client in the image); `ssh` is a symlink first in `PATH`
(`/usr/local/rubi/bin`); git over ssh is switched off (`protocol.ssh.allow never`); `tool_use_id` is passed on only when the
session has exactly one running `Bash` call that mentions `ssh`; backpressure is end to end (Home acks only what the wrapper wrote).

### 9.6 Deliberate difference from the addendum

There is no proxy allow-list entry for the target's `host:port`. The VM never reaches the target; Home is the client.
That is strictly narrower.

### 9.7 Audit and the Mac as a target

- Audit per stream: `{role, project, bot, target, command (clipped to 200, redacted), start, end, exit, bytes in and
  out}`. No output, no key material. Shown on the SSH page and as plugin audit entries.
- The Mac itself is reachable only as a registered target (for example `127.0.0.1:22` with Remote Login on), granted
  like any other.

## 10. Closing the VM-to-Mac gateway

**Status (R13, done): option D is built and is the only mode.** Every VM runs with `--network none`; the mux, the forwarder and
the Home side are described in "As built (R13)" at the end of this section. The table below is the decision record.

**The gap today.** VMs sit on the host-only network (for example `192.168.128.0/24`, gateway `192.168.128.1`). Any Mac service
listening on all interfaces is reachable from a VM through the gateway. A typical Mac has several such listeners
(system sharing services, development servers). The self-test only probes the default NAT network's gateway on port
53. With the auth gateway, the VM legitimately needs **two** Mac ports: the CONNECT proxy and the auth gateway.

| Option | How | Pros | Cons |
|---|---|---|---|
| **D. No IP route (recommended if it works)** | Run role VMs with no network attachment. An in-VM forwarder (`rubi-netpipe`, part of the runner image) listens on `127.0.0.1:3128` (proxy) and `127.0.0.1:3129` (auth gateway) and multiplexes connections over a dedicated `container exec -i` stdio channel to Home. Home applies the same allow-list and dials out. | No root, no admin dialog. Nothing on the Mac is reachable at all (no route). DNS is moot. Survives reboots with no system state. | Needed a mux (frames, flow control). **Verified (R13a):** Apple container 1.6.1 runs `--network none` with loopback only, and `exec -i` stdio is binary safe at 220-290 MiB/s. |
| **A. pf anchor through the wizard's administrator step** | Installed with the same `osascript ... with administrator privileges` path as step 1: rules in `/Library/Application Support/Rubi/pf.rules`, loaded into anchor `com.apple/250.RubiAgent` (the stock `pf.conf` already references `com.apple/*`), with `block in quick inet from <subnet> to any`, `pass in quick proto tcp from <subnet> to <gw> port {<proxy-range>, <auth-port>}`, the same for the network's IPv6 ULA prefix, and a root LaunchDaemon that runs `pfctl -e` and reloads the anchor at boot. The network is created with a fixed `--subnet`; the proxy and auth gateway use a fixed port range (`agent_gateway_ports`, for example 47300-47399) instead of `:0`. | Kernel-enforced, all protocols, independent of the VM tool. | Root. Needs the owner at the Mac (administrator dialog). Changes system state (pf enable, a LaunchDaemon) that must be uninstallable. Possible interaction with other pf users (VPNs). Subnet and ports must stay in sync with the config. |
| B. Bind only plus a firewall check | Proxy and auth gateway listen only on the gateway address (already the case); the wizard checks the macOS application firewall (`socketfilterfw --getglobalstate`, `--getblockall`). | No root. | Does not close anything: other apps' wildcard listeners stay reachable. The application firewall is per app, not per interface, and "block all" would also block Rubi Home's own listeners. A warning at most. |
| C. Apple container network policy | `network create` offers only `--internal`, `--subnet`, `--subnet-v6`, `--label`, `--plugin`, `--option` (vmnet plugin options undocumented). | | No per-port or per-host filter exists in 1.6.1. |

**Decision rule.**
- R13a is a live spike (no admin rights): try `container run` with no network, check `ip addr` and routes in the VM, and
  measure mux throughput.
- If a networkless container works, implement D.
- Otherwise implement A, which needs the owner for the administrator dialog and a live check.
- Either way, add a self-test check: the VM connects to the gateway address on every TCP port the Mac listens on with a
  wildcard address (Home enumerates them with `lsof -iTCP -sTCP:LISTEN`, or a fixed sample of ports). Every port except
  the proxy and auth ports must be unreachable (`ETIMEDOUT`, `EHOSTUNREACH` or `ENETUNREACH`). An `ECONNREFUSED` counts
  as reachable.

### As built (R13)

- **VM.** `RunArgs` has `--network none` for the legacy role, every role and the sign-in container. No network is created,
  inspected or verified any more (`verifyInternal`, `checkHostOnly` and the fixture files are gone); `agent_network`,
  `agent_proxy_listen` and `agent_proxy_vm_host` are ignored.
- **Pipe.** `internal/home/agent/netpipe` (Go) and `runner/container/rubi-netpipe` (node, in the image) speak the framing in
  agent-runner.md ("No network: rubi-netpipe"). `pipe.go` runs the forwarder with a long-lived `container exec -i`, waits for
  HELLO, restarts it up to three times in five minutes if it dies and then stops the VM.
- **Services.** `StartProxy` and `StartGateway` take a `Listener` (a `netpipe.Listener` fed by the streams): the proxy's
  CONNECT checks and the gateway's checks are unchanged; neither listens on the Mac. The manager starts them before the VM.
  The env is `HTTPS_PROXY=http://rubi:<token>@127.0.0.1:3128`, `ANTHROPIC_BASE_URL=http://127.0.0.1:3129`, `NO_PROXY=127.0.0.1`.
- **Self-test.** The gateway-port check of the decision rule is moot (the VM has no route to any address). The isolation probe
  asserts: `/proc/net/route` has no row but the header, `/proc/net/ipv6_route` none off `lo`, `/sys/class/net` is `lo` only;
  `1.1.1.1` and four gateway addresses (`192.168.64.1`, `192.168.128.1`, `192.168.0.1`, `10.0.0.1`) fail with `ENETUNREACH`
  (a timeout or `ECONNREFUSED` fails the check); DNS fails; the proxy path gives 200 for a listed host and 403 for an
  unlisted one. The wizard's network step reports "done: not needed".
- **Live test** (`RUBI_AGENT_LIVE=1 RUBI_AGENT_LIVE_RUN=netless go test ./internal/home -run TestLiveNetless`): a role VM; no
  route; `ENETUNREACH` to `1.1.1.1`, to the NAT network's real gateway, to the usual 192.168 gateways, each on 443 and on a
  port that is open on every address of the Mac; DNS fails; a real TLS request to `registry.npmjs.org` through the CONNECT
  tunnel returns 200; an unlisted host 403; no credentials 407; the gateway answers 401 to a wrong secret; no listener was
  added to the Home process; after the exec process of the pipe is killed, the pipe comes back and the proxy path works again.
  `TestLiveIsolationProbe` (package `setup`, same hook) runs the self-test's probe script in a real VM.

## 11. Panel (plugin pages)

- **Roles** (per device and project): sections per project with role rows (name, status, version, skill count, tools
  summary, "unreviewed" and "held" markers, running sessions). Buttons: New role (prompt: name), Start session
  (prompt: first message), Open, Archive or Unarchive, Convert (legacy role).
- **Role detail:**
  - skills, each with its source tag (`library id@v`, following or pinned; `unreviewed`; `held`; scan verdict) and
    buttons Remove, Accept (held), Pin or Follow, Submit to library;
  - tools as toggle items (safe set; others behind strong approval);
  - deny rules, MCP servers and Claude plugins (adding asks for strong approval);
  - extra hosts, SSH grants (Grant or Revoke from registered targets), subagents, default mode (choice up to the
    ceiling), project instructions on or off.
  - The panel's `prompt` box is limited to 200 characters, so skill and instruction **content** cannot be pasted in the
    panel. Content comes from Bots or the library. The panel can show, remove, accept and revert it. A multi-line input
    is a later panel feature.
- **Role history:** versions with who, tier and approval; a diff view; Revert.
- **Chats grouped by role:** the sessions page gets one section per role (legacy role first). The chat page header
  names the role.
- **SSH targets:** list (fingerprint, allowed commands, ask-each-time, roles granted), Add (prompts; then the strong
  approval; then the public key shown with a copy button through an `info` field), Remove, per-target audit.
- **Library** (core panel): browser, versions, scan results, used-by, curator queue.

## 12. Tests per requirement

| Requirement | Test (where) |
|---|---|
| A skill of role A is not listed or usable in role B on the same repo; MCP and tool lists differ | runner integration (real SDK, scripted API): §1.1 #14 as assertions on `init.skills`, `init.agents`, `init.tools`, `init.mcp_servers` and the Skill tool's `Unknown skill` result; the repo's `.claude` skills, agents, commands, nested skills and `env` never appear. Live VM variant: two role VMs on one project (`RUBI_AGENT_LIVE_RUN=roles`). |
| The repo's CLAUDE.md still reaches a role | integration: sentinel in the request body. |
| A skill's `allowed-tools` changes nothing | unit (Home materializer strips it); integration: a skill with `allowed-tools: Bash` and a hook that denies, so the Bash call is denied. |
| A subagent definition cannot widen | integration (#13). |
| Slot rule per role, global cap kept | `slots_test.go`: two roles on one project run together; a second session of one role queues with the role reason; the computer cap still holds; an explicit `agent_max_sessions_per_project` still applies. |
| VM per role mounts | `apple_test.go`: `RunArgs` for a role has `/rubi-role` read-only and `/rubi-claude` read-write, no `/rubi-auth`, never `--ssh`, and each project has its own container name. |
| Tier enforcement | plugin unit: each row of §6.1. A Bot without a trust gets `needs_approval`. Adding an MCP server, host or plugin through `role_tools_set`, `skills_set` (a skill containing `mcpServers`/`allowed-tools`), `agents_set` or a revert all yield tier 2. Atomic: a mixed request applies nothing. Rebase failure on approval. |
| Audit and revert | plugin and Home: each change makes a version and an audit entry; a revert makes a new version; a widening revert asks for approval. |
| Per-Bot limits | plugin: the 6th role on a project fails with the limit named; the hourly rate limit. |
| Scanner | `sdk/skillscan` corpus: each reject and flag rule with a positive and a negative sample; stripping; determinism (same input, same output). |
| Library | core: a non-curator cannot admit; a proposed or rejected version cannot be attached; Home refuses a library skill with a bad or missing signature; pin versus follow; an update creates a role version, and rollback restores it; withdraw drops the skill; changing the curator needs `rubi.library.curator`. |
| SSH | Home unit with an in-process `x/crypto/ssh` server: an ungranted role is refused; a granted role reaches only its target; a host key mismatch is refused and audited; revoke cuts a live stream; allowed commands are enforced; ask-each-time waits for `decide`; the key never appears in any RPC payload, event, log line or env file (scan all of them for the key bytes). Live: the VM filesystem and env hold no key material. |
| Gateway closed except proxy and auth | self-test check (§10) plus a live test. |
| Auth gateway assumption | integration (#17): bearer placeholder to the base URL, no other paths. |

## 13. Milestones (small, independently verifiable commits)

Order: after the auth gateway work. Each step is green on its own, and the legacy role keeps today's behaviour.

| # | Commit | Repo | Verifies |
|---|---|---|---|
| R1 | Runner role mode: `RUBI_ROLE_DIR`, `role.json` parsing, the option builder (§4.3), CLAUDE.md append; port the spike to `roles.integration.test.ts` | core/runner | unit (fake SDK options) and integration isolation matrix |
| R2 | `sdk/skillscan` with corpus tests; frontmatter stripping | core | unit |
| R3 | Home role store: `role.put/get/list/history/diff`, validation, blobs, versioned materialization, `current` symlink, GC | core/home | unit |
| R4 | Home VM per role: container name, mounts, per-role config dir, per-role proxy hosts and `agent-net.json` keyed by role, legacy role mapping, `status` rows | core/home | unit (`apple_test`, `manager_test`) and a live two-role run |
| R5 | Slots per profile (`agent_max_sessions_per_profile`), queue reasons, `claude-agent.limits` (user-only) | core/home | `slots_test` |
| R6 | Plugin: role cache, tier engine (diff-based), per-Bot limits, `claude_code_role_*` tools, new kinds (`role_foreign`, `role_widen`, `role_skill_accept`), audit | plugin | unit (tier table) |
| R7 | Plugin panel: Roles, Role detail, History with diff and Revert, chats grouped by role, Convert legacy role | plugin | `agent_panel_test` |
| R8 (done) | Core library: store, library key, signing, curator setting (`rubi.library.curator`), `rubi_library_*` tools, `library.get` host method; Home pins the key (resolve the origin question first) | core | unit |
| R9 | Plugin: library skills in roles (follow or pin, update re-materialization, rollback) | plugin | unit |
| R10 (done) | Core panel: library browser and curator queue | core/panel | panel tests |
| R11 | Home SSH: key generation, target registry, pinned-host-key client, stream relay, limits, audit | core/home | unit with an in-process SSH server |
| R12 | Runner and image: `rubi-ssh` wrapper, socket, `ssh.*` relay; plugin: `ssh_target` kind, grants, ask-each-time, SSH page | core/runner, plugin | integration and a live run |
| R13a (done) | Live spike: networkless container plus mux throughput | core | spike report (agent-runner.md) |
| R13b (done: D) | Either D (`rubi-netpipe` mux and Home side, network check replaced) or A (pf anchor, LaunchDaemon, uninstall, fixed subnet and ports; **needs the owner at the Mac for the administrator dialog**) | core | self-test gateway check and a live test |
| R14 | Docs: agent-runner.md, rubi-home.md, plugins.md, home-setup-wizard.md, security-model.md (§9a) | core | review |

## 14. Decisions and open questions

Decided by the owner (2026-10-31):
1. The computer-wide cap defaults to 4.
2. `role_mode_ceiling` defaults to `bypassPermissions` (a user setting).
3. The curator may admit its own skills, marked "self-admitted by curator"; the checks must still pass.
4. SSH: Rubi Home is the SSH client; the VM never connects to a target.

Verified: `container run --network none` gives a VM with only loopback and no routes, so option D (§10) is possible
without root; the exec-channel mux still needs a throughput check.

Open:
- Should a role be able to opt back into the repo's `.claude` skills, as the legacy role does? Proposed: no; only the
  legacy role keeps that.
