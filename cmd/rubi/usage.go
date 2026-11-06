package main

// usage is rubi's --help. The Bot that installs and runs Rubi reads it; the user never needs it (project rule
// 2): everything the user does is in the panel. The development stand-in for the panel (`rubi dev-panel`) is left
// out, and is documented in the README's development section.
const usage = `rubi — self-hosted integrations for AI agents, gated by your approval

Usage:
  rubi mcp        MCP server over stdio (register this with your agent); starts the daemon if needed
  rubi daemon     run the daemon in the foreground
  rubi status     print Rubi's state
  rubi version    print the version
  rubi update [--check | <version>]
                  check for or install a signed release (the agent's rubi_update tool does this without
                  locking Rubi; this command restarts it locked)
  rubi rollback   go back to the previous version
  rubi net-check [host:port ...]
                  check that plugins can reach the internet from here (through the machine's egress proxy if
                  it has one), step by step; default: the iCloud and Gmail mail servers
  rubi transport [gateway | relays | tailscale]
                  show or choose how the panel reaches Rubi (Rubi Gateway, public relays, or your own
                  Tailscale network); Rubi restarts locked to apply a change

Environment:
  RUBI_HOME          state directory (default ~/.rubi)
  RUBI_PANEL_ORIGIN  panel origin (default https://rubi-panel.com); only before pairing
`
