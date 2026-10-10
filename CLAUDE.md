# Rubi-Project

## Goal
Self-hosted, plugin-based integrations for AI agents that act on the user's behalf only with consent (MCP server: rubi mcp).

## Stack
- Language/runtime: Go 1.26 (module github.com/Deikus-LXXVII/rubi)
- Frameworks/libraries: <...>
- Tooling: go modules

## Commands
- Install: `go mod download`
- Build: `go build ./...`
- Test (all): `go test ./...`
- Test (single): `<...>`
- Lint/format: `go vet ./...`
- Run locally: `<...>`

## Architecture
- <Top-level layout: dir → responsibility>
- <Key modules and how they interact>
- Rules:
  - <e.g. UI never talks to DB directly>
  - <e.g. no new dependencies without approval>

## Conventions
- Naming: <...>
- Error handling: <...>
- Tests: <where they live, what must be covered>
- Commits/branches: <...>

## Docs
- Plans: `docs/plans/*.md`
- Architecture notes: `docs/<...>.md`
- <Other links>

## Current status
- Done: <...>
- In progress: <...>
- Next: <...>
- Known issues: <...>
