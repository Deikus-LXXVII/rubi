# Rubi-Project

Self-hosted integrations for AI agents (starting with Grok Bot), where the agent acts on your behalf only
with your consent.

- **Decentralized.** No project servers. Rubi runs on your agent's machine; the panel at
  `rubi-panel.com` is a static site.
- **Your key, not the agent's.** Everything Rubi stores is encrypted. Rubi starts locked and opens only
  when you unlock it with Face ID (passkey) or a password in the panel.
- **Real approvals.** Sensitive actions, like sending email, are approved by signing that exact action
  with your passkey. You choose the level per action; security settings always need your approval.
- **Agent-native.** Install and use it from the agent chat. The agent registers one MCP server:
  `rubi mcp`.

> **Status: pre-alpha.** Core daemon, zero-config panel connection, the web panel (Face ID pairing, unlock and approvals, settings) and the iCloud Mail integration work; packaging and signed releases (M5) are next. Not ready for real data yet.

## Integrations

| Integration | Status |
|---|---|
| iCloud Mail: read, search, drafts, approved send, reply notifications | Works; verified live against iCloud |

## Design

- [Security model and protocol](docs/design/security-model.md)
- [Agent interface and integration model](docs/design/agent-interface.md)
- [Panel protocol](docs/design/panel-protocol.md)
- [Implementation plan](docs/design/implementation-plan.md)

## Build

```bash
go build -o rubi ./cmd/rubi
./rubi status
```

`RUBI_HOME` sets the state directory (default `~/.rubi`). The panel transport needs
[`cloudflared`](https://github.com/cloudflare/cloudflared/releases) on `PATH` (or `RUBI_CLOUDFLARED`);
`RUBI_PUBLIC_URL` uses a fixed URL instead, e.g. with Tailscale.

Until the web panel exists, `rubi dev-panel '<link>'` pairs or unlocks with a password from the terminal.

## License

[MIT](LICENSE)
