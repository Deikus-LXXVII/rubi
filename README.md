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

> **Status: pre-alpha.** Milestone M1 (core daemon) is in progress. Nothing here is ready for real data yet.

## Integrations

| Integration | Status |
|---|---|
| iCloud Mail: read, search, drafts, approved send, reply notifications | Planned (M4); a working single-user prototype exists |

## Design

- [Security model and protocol](docs/design/security-model.md)
- [Agent interface and integration model](docs/design/agent-interface.md)
- [Implementation plan](docs/design/implementation-plan.md)

## Build

```bash
go build -o rubi ./cmd/rubi
./rubi status
```

`RUBI_HOME` sets the state directory (default `~/.rubi`).

## License

[MIT](LICENSE)
