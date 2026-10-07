# Rubi-Project — Plugins and the Marketplace

Status: **accepted** (v0.3, 2026-10-07). Companion to [security-model.md](security-model.md) and
[agent-interface.md](agent-interface.md).

Rubi ships bare: the core holds the keys, approvals, panel, events and its own updates, but no
integrations. Integrations are **plugins**, installed from a marketplace like apps from an app store. A
plugin has its own releases, its own updates and its own permissions. Updating Rubi doesn't touch plugins,
and updating a plugin doesn't restart Rubi.

## 1. Decisions

| Question | Decision |
|---|---|
| Who publishes | **Reviewed plugins** in the catalog, plus **sideloading** from any source with an explicit warning |
| Languages | **Any language.** An open JSON-RPC protocol over stdio; a Go SDK ships with Rubi |
| Where plugins live | **Each plugin in its own repository**, including first-party ones (iCloud Mail: `Deikus-LXXVII/rubi-icloud-mail`) |
| Isolation | **One process per plugin.** Secrets, approvals and events go through the core |
| Servers | **None.** The catalog is a signed static file on `rubi-panel.com`; packages are GitHub release assets |

## 2. Package

A plugin release (a GitHub release in the plugin's repository, tag `vX.Y.Z`) contains:

| Asset | Content |
|---|---|
| `rubi-plugin.json` | The manifest (§3), identical for all platforms |
| `<id>-<os>-<arch>.tar.gz` | The plugin for one platform (`linux-amd64`, `linux-arm64`, `darwin-arm64`), or `<id>-any.tar.gz` for platform-independent plugins (scripts). The archive holds the `entry` executable and any files it needs |
| `SHA256SUMS` | SHA-256 of every asset above |
| `SHA256SUMS.sig` | Ed25519 signature of `SHA256SUMS` by the **publisher key** declared in the manifest |

Plugin releases must not be marked as pre-releases, so `releases/latest/download/` always points at the
newest one. Plugins hosted elsewhere use any static host that serves `<source>/latest/` and
`<source>/<version>/` with the same files.

## 3. Manifest

```json
{
  "schema": 1,
  "id": "icloud-mail",
  "name": "iCloud Mail",
  "version": "v1.0.0",
  "description": "Read, search and draft iCloud email; send after your approval.",
  "needs": "Your iCloud email address and an app-specific password ...",
  "publisher": { "name": "Rubi-Project", "key": "MCowBQYDK2VwAyEA...", "url": "https://github.com/..." },
  "source": "https://github.com/Deikus-LXXVII/rubi-icloud-mail",
  "min_rubi": "v0.3.0",
  "api": 1,
  "entry": "icloud-mail",
  "fields":  [{ "key": "address", "label": "iCloud email address", "type": "email", "required": true }],
  "secrets": [{ "key": "app_password", "label": "App-specific password", "help": "...", "help_url": "..." }],
  "actions": [{ "kind": "icloud-mail.send", "title": "Send email", "default_level": "strong",
                "options": [{ "key": "send", "label": "Send" }] }],
  "events":  [{ "type": "reply", "untrusted_fields": ["reply.from", "reply.subject"] }],
  "egress":  ["imap.mail.me.com:993", "smtp.mail.me.com:587"],
  "tools":   [{ "name": "icloud_mail_send", "description": "...", "input_schema": { "type": "object" } }]
}
```

Rules the core enforces:

- `id`: 3–32 characters, `[a-z0-9-]`, starting with a letter. `rubi` and ids starting with `rubi-` are
  reserved.
- Action kinds start with `<id>.`. Tool names start with the id with `-` replaced by `_`, plus `_`
  (`icloud_mail_…`). A plugin can't define or trigger anything outside its own namespace.
- Tools are declared in the manifest, so Rubi lists them to the agent even while it is locked and the plugin
  isn't running.
- `publisher.key` is a base64 Ed25519 public key (SubjectPublicKeyInfo DER, the body of a PEM file).

The Go SDK generates the manifest from code: `<plugin> --manifest` prints it, and the release workflow
publishes that output.

## 4. Trust

**Catalog.** `rubi-panel.com/marketplace/catalog.json` and `catalog.json.sig`. The catalog is signed with
the Rubi release key, which is embedded in the core. It is generated from `marketplace/catalog.json` in
the Rubi repository, where changes go through review.

```json
{
  "schema": 1,
  "updated_at": "2026-10-07T00:00:00Z",
  "plugins": [{
    "id": "icloud-mail", "name": "iCloud Mail", "summary": "...",
    "publisher": { "name": "Rubi-Project", "key": "MCowBQYDK2VwAyEA..." },
    "source": "https://github.com/Deikus-LXXVII/rubi-icloud-mail",
    "versions": [{ "version": "v1.0.0", "sums_sha256": "<sha256 of SHA256SUMS>", "min_rubi": "v0.3.0" }]
  }]
}
```

**Reviewed plugins.** Rubi only installs versions listed in the catalog, and checks two independent
signatures:

1. the catalog signature, with the Rubi release key;
2. `SHA256SUMS.sig`, with the publisher key the catalog lists.

It also checks that the SHA-256 of `SHA256SUMS` equals the catalog's `sums_sha256`. Each version is
reviewed: a new plugin version only becomes available after its catalog entry is updated.

**Sideloaded plugins.** These install from a source URL: a GitHub repository (the latest release), or a
release download base. The publisher key comes from the manifest and is pinned at install
(trust-on-first-use). Rules:

- Later updates must be signed with the same key. A changed key means remove and reinstall.
- The install approval carries a prominent **"Not reviewed by Rubi-Project"** warning, the source and
  the key fingerprint.
- An id that exists in the catalog can only be installed from the catalog. A sideloaded plugin can't
  impersonate a reviewed one.

**On disk.** Installed files are not trusted on their own. At install Rubi computes a tree hash of the
extracted plugin. It stores the hash in the vault, together with the version, source, publisher key and
review status. Before every start, Rubi recomputes the tree hash. If it doesn't match, the plugin isn't
started and the agent gets a `plugin.tampered` event. A plaintext index (`plugins/index.json`) only serves
to list tools while Rubi is locked; nothing runs on its word.

## 5. Install, update, remove

All three are **strong approvals** with locked levels (`rubi.plugin.install`, `rubi.plugin.update`,
`rubi.plugin.remove`). The approval screen shows what the plugin gets, like app permissions:

| Line | Example |
|---|---|
| Review | Reviewed by Rubi-Project / **Not reviewed**, installed from `<source>` |
| Publisher | Rubi-Project · key fingerprint |
| Will ask you for | iCloud email address, App-specific password |
| Can | Read and search mail (no approval), Send email (Face ID) |
| Connects to | imap.mail.me.com:993, smtp.mail.me.com:587 |
| Can notify about | reply |

The flow:

1. **Install.** The user asks the agent ("install iCloud Mail") or taps **Install** in the panel's
   **Store**. The plugin is downloaded and verified *before* the approval is created, so the approval shows
   the real manifest. Once the user approves, Rubi installs the plugin and offers setup (`setup:<id>`).
2. **Update.** Rubi checks the catalog and sideloaded sources at startup and every 6 hours, like core
   updates but separately. Each new version is announced once with a `plugin.update_available` event.
   The update approval lists **new permissions** separately. Rubi stops the plugin, swaps the files and
   starts it again; secrets, settings and state stay.
3. **Remove.** Rubi stops the plugin and deletes its files, secrets, settings and state.

The agent can't skip any of these approvals: `rubi.*` kinds are always strong.

## 6. Runtime

- **Lifecycle.** A plugin process runs while Rubi is unlocked and the plugin is installed. On lock it is
  asked to shut down and killed after 2 s, so secrets it fetched leave memory with it. A crashed plugin
  is restarted with backoff (1 s, 5 s, 30 s). After 5 crashes in 10 minutes it stays down, and the agent
  gets a `plugin.crashed` event.
- **Process.** The working directory is `plugins/<id>/data`. The environment holds only `PATH`, `HOME`
  (that directory), `LANG`, `RUBI_PLUGIN_ID` and `RUBI_PLUGIN_API`. Plugins run in their own process
  group and die with the daemon (Linux: `PR_SET_PDEATHSIG`). stderr goes to `logs/plugin-<id>.log`.
- **Daemon hardening.** On Linux the daemon marks itself non-dumpable (`PR_SET_DUMPABLE=0`). A plugin
  running as the same user then can't read the daemon's memory through ptrace or `/proc/<pid>/mem`.
- **What a plugin can reach.** Only its own settings, secrets and state, and only approvals, events and
  audit entries in its own namespace. It never sees the vault key, other plugins' data, the panel or the
  webhook key.
- **What it can't be stopped from doing.** Egress hosts are declared and shown, but not enforced. A
  plugin is code the user chose to run on the agent's machine, and the agent has root there anyway
  (security model §3). Review in the catalog is what protects against a malicious plugin; process
  separation protects against a buggy one.

## 7. Protocol (API 1)

JSON-RPC 2.0, one message per line, over the plugin's stdin (host to plugin) and stdout (plugin to host).
Both sides send requests. A message may be up to 4 MiB.

**Host → plugin**

| Method | Params | Result |
|---|---|---|
| `initialize` | `{api, rubi_version, plugin_id}` | `{api}` |
| `validate` | `{fields, secrets}` | `{settings, account}`. Check what the user entered (for example, log in). Errors are shown in the panel |
| `start` | `{}` | `{}`. The plugin is connected (settings exist). Begin background work |
| `stop` | `{}` | `{}`. The user disconnected. Stop background work |
| `tool` | `{name, arguments}` | Any JSON object, returned to the agent. Only called while connected |
| `execute` | `{kind, option, payload}` | Any JSON object. An approved action runs; `payload` is what the plugin submitted |
| `shutdown` | `{}` | `{}`, then exit |

**Plugin → host**

| Method | Params | Result |
|---|---|---|
| `settings.get` | `{}` | `{settings}` |
| `secret.get` | `{key}` | `{value}`. Only keys declared in the manifest |
| `state.get` / `state.set` | `{}` / `{state}` | `{state}` / `{}`. Private state, encrypted in the vault |
| `approval.submit` | `{kind, summary, question?, preview, options, payload}` | When the kind needs no approval, the host calls `execute` right away and returns its result. Otherwise it returns `{status: "awaiting_approval", approval_id, …}` for the agent |
| `level.get` | `{kind}` | `{level}` |
| `event.emit` | `{type, data}` | `{event_id}`. The type must be declared; `untrusted_fields` come from the manifest |
| `audit` | `{event, fields}` | `{}` |

Plugins log to stderr; Rubi saves it to `logs/plugin-<id>.log`.

Errors use JSON-RPC error objects. `execute` must perform exactly the action described by its `payload`.
Review checks this, because the user approves what the preview shows.

## 8. Agent and panel

**Core tools** (added to agent-interface §3):

| Tool | Purpose |
|---|---|
| `rubi_store(query?)` | Catalog plugins, with installed version and available updates |
| `rubi_plugin_install(id or source)` | Verifies the package and returns an approval link |
| `rubi_plugin_update(id)` / `rubi_plugin_remove(id)` | Same, for update and remove |
| `rubi_call(tool, arguments)` | Calls a plugin tool by name, for agents that don't refresh their tool list after an install |

Plugin tools are added to the MCP server when a plugin is installed. The server sends
`notifications/tools/list_changed`.

**Panel.** A **Store** screen lists the catalog: install, update, open setup. Settings lists installed
plugins with their version, review status, Update and Remove. Approval screens render the permission
table from §5.

## 9. Compatibility

- `min_rubi` is checked before install and update. If the core is too old, the store says
  "Requires Rubi vX" and the agent is told to update Rubi first.
- Core releases keep supporting every published plugin API version. A plugin declaring an unknown `api`
  is refused.
- Core updates leave plugins installed and running; only the daemon restarts (exec handoff).

## 10. Migration from v0.2

Through v0.2, iCloud Mail was compiled into the core. From v0.3 it is the first plugin, in
`Deikus-LXXVII/rubi-icloud-mail`, built with the Go SDK and listed in the catalog as reviewed. No
release before v0.3 was installed outside testing, so there is no data migration: after updating, a
tester reinstalls iCloud Mail from the store and sets it up again.

## 11. Implementation

| Part | Where |
|---|---|
| Go SDK (protocol, `Plugin`, `Host`, `--manifest`) | `sdk/rubiplugin` |
| Manifest checks, catalog, download and verification, store on disk, process runner | `internal/plugins` |
| Install / update / remove approvals, update checks, host side of the protocol, tool routing | `internal/core` (`market.go`, `plugins.go`) |
| Dynamic MCP tools, `rubi_store`, `rubi_plugin_*`, `rubi_call` | `internal/mcpserver` |
| Store screen, plugin list, permission previews | `panel/js/app.js` |
| Catalog source and signing | `marketplace/catalog.json`, `.github/workflows/pages.yml` |
| Signed test releases and catalog; a demo plugin covering the whole protocol | `internal/plugins/plugintest` |
