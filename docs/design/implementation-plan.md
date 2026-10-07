# Rubi-Project — Implementation Plan

Companion to [security-model.md](security-model.md) and [agent-interface.md](agent-interface.md).

## Milestones

| # | Milestone | Done when |
|---|---|---|
| M1 | **Core daemon** — vault, instance identity, state machine (`unpaired` / `locked` / `unlocked`), audit log, approvals engine, event store, MCP over a Unix socket, `rubi mcp` stdio proxy with lazy daemon start, core tools | An MCP client can run `rubi mcp`, see core tools, and get the correct state; vault and approvals have tests |
| M2 | **Panel API and transport** — cloudflared supervisor, end-to-end channel (X25519/HKDF/AES-GCM), pairing, unlock, credential management, unlock receipts | Pair and unlock work end to end from a test client through a quick tunnel |
| M3 | **Panel web app** (static, `rubi-panel.com`) — pairing with passkey (WebAuthn PRF) or password (Argon2id), unlock, approvals, settings, policy editor, integration setup forms | A real iPhone pairs, unlocks, and approves |
| M4 | **iCloud Mail integration** — port of the prototype: read/search/drafts, gated send with "send and notify on reply", reply tracking, events to the Grok Bot routine webhook | Mail works end to end with strong approvals |
| M5 | **Distribution** — signed releases from GitHub Actions with provenance, `install.sh`, binary self-check, Grok Bot skill, user docs | A fresh Grok Bot installs Rubi from one chat message |

## Repository layout

```
cmd/rubi/                 CLI entry point: daemon | mcp | status | version
internal/paths/           filesystem layout ($RUBI_HOME, default ~/.rubi)
internal/vault/           sealed vault (XChaCha20-Poly1305), wrapped-key store
internal/identity/        instance identity keys (X25519 + Ed25519)
internal/core/            state machine, pairing codes, wiring
internal/approvals/       approval engine (levels none / chat / strong)
internal/events/          event store
internal/audit/           audit log
internal/mcpserver/       MCP tools exposed to the agent
internal/daemon/          Unix-socket server, single-instance lock
internal/mcpproxy/        stdio <-> Unix socket proxy with lazy daemon start
internal/integrations/    integration API and built-in integrations
panel/                    static web panel (M3)
docs/design/              design documents
```

## Conventions

- Go 1.26, standard library first; external dependencies: the MCP Go SDK and `golang.org/x/crypto`.
- English everywhere, including comments and commit messages.
- No secret is ever logged or returned through MCP.
- Every security-relevant behavior gets a test.
