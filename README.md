# Rubi-Project

Self-hosted integrations for AI agents (starting with Grok Bot), where the agent acts on your behalf only
with your consent. Rubi starts bare; integrations are plugins from the Rubi store.

- **Decentralized.** No project servers. Rubi runs on your agent's machine; the panel at
  `rubi-panel.com` is a static site.
- **Your key, not the agent's.** Everything Rubi stores is encrypted. Rubi starts locked and opens only
  when you unlock it with Face ID (passkey) or a password in the panel.
- **Real approvals.** Sensitive actions, like sending email, are approved by signing that exact action
  with your passkey. You choose the level per action; security settings always need your approval.
- **Agent-native.** Install and use it from the agent chat. The agent registers one MCP server:
  `rubi mcp`.
- **A store, not a monolith.** Plugins are separate signed packages with their own updates. Store plugins
  are reviewed; installing one shows its permissions and needs your approval. Anyone can build one, in any
  language ([plugin design](docs/design/plugins.md), Go SDK in `sdk/rubiplugin`).

## Quick start

Tell your Grok Bot:

> Install Rubi-Project: run `curl -fsSL https://rubi-panel.com/install.sh | sh` and follow its instructions.

Then open the link it sends you and set up Face ID. See [Getting started](docs/user/getting-started.md).
The agent-side playbook is in [skills/grok-bot/rubi-project](skills/grok-bot/rubi-project/SKILL.md).

> **Status: pre-alpha.** The core, the panel, signed releases with one-tap updates, and the plugin store work. Not ready for real data yet.

## Plugins

| Plugin | Repository | Status |
|---|---|---|
| iCloud Mail: read, search, drafts, approved send, reply notifications | [rubi-icloud-mail](https://github.com/Deikus-LXXVII/rubi-icloud-mail) | Reviewed; verified live against iCloud |

## Design

- [Getting started (users)](docs/user/getting-started.md)
- [Releasing (maintainers)](RELEASING.md)
- [Security model and protocol](docs/design/security-model.md)
- [Agent interface](docs/design/agent-interface.md)
- [Plugins and the store](docs/design/plugins.md)
- [Panel protocol](docs/design/panel-protocol.md)
- [Implementation plan](docs/design/implementation-plan.md)

## Build

```bash
go build -o rubi ./cmd/rubi
./rubi status
```

`RUBI_HOME` sets the state directory (default `~/.rubi`). The panel reaches Rubi through public Nostr
relays by default (`RUBI_RELAYS` overrides the list). If no relay is reachable, Rubi falls back to a
Cloudflare quick tunnel ([`cloudflared`](https://github.com/cloudflare/cloudflared/releases) on `PATH`, or
`RUBI_CLOUDFLARED`; `RUBI_TUNNEL=1` starts it always). `RUBI_PUBLIC_URL` adds a fixed URL, e.g. with
Tailscale.

`rubi dev-panel '<link>'` pairs or unlocks with a password from the terminal (for development).

## License

[MIT](LICENSE)
