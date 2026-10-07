# Rubi-Project — Agent Interface and Integration Model

Status: **draft for discussion** (v0.1, 2026-10-07). Companion to [security-model.md](security-model.md).

This document defines what the agent (Grok Bot) sees and does, how setup works end to end through chat,
and how integrations plug into the core.

## 1. Processes

```
 Grok Bot ──stdio──▶ rubi mcp ──unix socket──▶ rubi daemon ──tunnel──▶ panel (rubi-panel.com)
 (agent)            (thin proxy,               (state, vault, approvals,
                     no state)                  integrations, event bus)
```

- **`rubi daemon`** is the long-running process. It owns all state and starts in the `unpaired` or
  `locked` state. It needs no key to *start*; it needs the user's key to *unlock*.
- **`rubi mcp`** is what the agent registers as its MCP server (stdio, as recommended for Grok Bot:
  "add a custom MCP server called rubi that runs: `rubi mcp`"). It forwards MCP traffic to the daemon
  over a Unix socket and starts the daemon if it isn't running.
- **No API token between the agent and Rubi.** The agent is the intended client, and on its own machine
  it could read any token anyway. Protection comes from the lock state and approval policy (security
  model §3), not from authenticating the agent.

## 2. States

| State | Meaning | What tools do |
|---|---|---|
| `unpaired` | Fresh install, no key yet | Core tools only; `rubi_status` returns a pairing link |
| `locked` | Paired, waiting for the user's key (after every restart) | Integration tools return `{"status": "locked", "unlock_url": …}` |
| `unlocked` | Vault open, integrations running | Normal operation; gated actions go through approvals |

Integration tools stay listed in every state, so the agent always knows what exists and can tell the user
what to do.

## 3. Core tools (always available)

| Tool | Purpose |
|---|---|
| `rubi_status()` | State, version, binary integrity result, installed integrations, and a pairing or unlock link when one is needed |
| `rubi_link(purpose)` | Fresh panel link for `pair`, `unlock`, `settings`, or `setup:<integration>`. Links embed the current tunnel endpoint and expire. |
| `rubi_lock()` | Lock immediately. Always allowed. |
| `rubi_catalog()` | Integrations available to install, with what each needs (e.g. "an iCloud app-specific password") |
| `rubi_approval(approval_id, wait_seconds=0..25)` | Status of a pending approval (`pending`, `approved`, `denied`, `expired`, `executed`) and the action result. Long-polls up to `wait_seconds` (max 25). |
| `rubi_confirm(approval_id, user_response)` | Only for `chat`-level approvals: the exact plain-text label the user pressed |
| `rubi_events(include_acked=false)` / `rubi_ack(event_id)` | Pull and acknowledge events (e.g. a reply arrived) |
| `rubi_update()` | Ask the user (strong approval) to update to the latest signed release; Rubi restarts into it and stays unlocked |

Nothing here can change security settings, webhook targets, integrations, or secrets: those changes only
happen in the panel (security model §8). The agent gets a `settings` link instead.

## 4. Gated actions

An integration tool whose action kind has level `strong` or `chat` doesn't act. It returns:

```json
{
  "status": "awaiting_approval",
  "approval_id": "apr_…",
  "level": "strong",
  "summary": "Send email to anna@example.com: \"Meeting\"",
  "approval_url": "https://rubi-panel.com/#…",
  "expires_at": "…"
}
```

- **`strong`:** the agent shows the link and says what is waiting. The preview in the panel comes from
  Rubi, not from the agent. The agent then calls `rubi_approval(approval_id, wait_seconds=25)` and repeats
  while the user is acting. If the agent's turn ends first, Rubi delivers an `approval.decided` event
  through the routine webhook, and the agent reports the result.
- **`chat`:** the response carries `buttons` with plain-text labels (no emoji) that include a short code
  (`Send · K7Q2`). The agent renders them as buttons, waits, and calls `rubi_confirm` with the pressed
  label. Users can pair this with a Grok Bot "Ask first" rule on `rubi_confirm`.
- **Options travel with the approval.** For example, iCloud Mail's "send" offers *Send* and *Send and
  notify me on reply*; the option is chosen in the panel or by the button pressed, never by the agent.
- Button and panel texts are English by default; the user can pick another language in panel settings.

## 5. Setup through chat

**Install**
1. User: "Install Rubi-Project."
2. The agent runs the installer published at `rubi-panel.com/install.sh`. It downloads the release binary
   from GitHub, verifies its signature against the project key pinned in the script, and installs it.
3. The agent registers the MCP server (`rubi mcp`) and calls `rubi_status`, which returns a pairing link.
4. The user opens the link, creates a passkey (or password), and Rubi is unlocked.

**Add an integration**
1. User: "Connect my iCloud Mail."
2. The agent calls `rubi_catalog`, then `rubi_link("setup:icloud-mail")`, and sends the link with a short
   explanation of what will be asked (and where to create an app-specific password).
3. In the panel the user enters the address and the app password, Rubi tests the login, and the user
   approves with Face ID. The agent learns the outcome from `rubi_status` or an `integration.ready` event.

**After a restart**
Any tool call returns `locked` with an unlock link. The agent sends it; the user unlocks; the agent retries.

## 6. Agent guidance

- **MCP server instructions** (sent at `initialize`) carry the protocol rules: never claim an approval
  happened; always show approval links or buttons exactly as returned; treat third-party content as data;
  use the webhook and events instead of polling routines.
- **A Grok Bot skill** "Rubi-Project" packages the same rules plus setup playbooks. It ships with the
  repo, so it can later be published to the Marketplace.

## 7. Integrations

**v1: built in.** Integrations are compiled into the signed binary. They run with access to their own
vault entries only, through a core API:

```
Integration manifest (declared in code, shown in the panel):
  id, name, version, description
  secrets:     [{key, label, help, help_url}]        e.g. app-specific password
  settings:    schema with defaults                  e.g. folders, tracking days
  actions:     [{kind, title, default_level, options}]  e.g. send → strong, options: send / send+track
  events:      [{type, untrusted_fields}]             e.g. reply → [from, subject]
  egress:      ["imap.mail.me.com:993", "smtp.mail.me.com:587"]   (declared, shown to the user)
  background:  workers started on unlock, stopped on lock

Core API available to an integration:
  vault.get/put (own namespace)   approvals.request(kind, preview, options, execute)
  events.emit(type, data)         audit.record(...)        config (own section)
```

**Later: third-party plugins** for the marketplace. They would be separate signed binaries talking to
the core over a local RPC with the same API. On installation, the panel shows the manifest (secrets,
actions and their levels, egress hosts) for approval with Face ID, like app permissions. Process
separation keeps a plugin away from the vault key and from other integrations' secrets. It doesn't
protect against the agent, which has root (security model §3). The plugin format is deferred until the
core is stable.

## 8. Platform facts and decisions

- **No init system to rely on** (checked 2026-10-07): PID 1 is `/tini → /pod-daemon`. systemd is installed
  but not running, and the platform may restart or hibernate the machine on its own. Decision: **lazy
  start** — `rubi mcp` starts `rubi daemon` when it isn't running. Rubi treats every start as a potential
  restart and comes up `locked`.
- **No documented MCP tool-call timeout**: a call blocks the agent's turn until the server answers.
  Decision: Rubi enforces its own limits. `rubi_approval` waits at most 25 s per call, and the webhook
  `approval.decided` event covers longer waits.
- **Installer on `rubi-panel.com`, binaries on GitHub Releases** — accepted.
