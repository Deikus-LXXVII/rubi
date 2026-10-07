# Rubi-Project — Core Security Model and Protocol

Status: **draft for discussion** (v0.1, 2026-10-07). Nothing here is implemented yet.

## 1. Goals

1. **Decentralized.** The project runs no servers. Every user's data lives on their own agent machine.
2. **Agent-native setup.** Installation and day-to-day use happen through the agent (Grok Bot) chat.
   A web panel is used only where the agent must be kept out of the loop: creating the key, unlocking,
   approving sensitive actions, entering service passwords, and changing security settings.
3. **The user holds the key.** All Rubi data on the agent machine is encrypted. Rubi cannot start, read
   secrets, or change its own security settings without the user's key.
4. **Strong, configurable approvals.** Sensitive actions (e.g. sending email) can require a biometric
   (passkey) or password approval bound to the exact action. Users choose the level for each action type,
   except for a small set of actions that always require approval.

## 2. Platform facts this design depends on

From the Grok Bot docs and a hands-on test on the agent machine (2026-10-07):

| Fact | Consequence |
|---|---|
| The agent runs on a persistent cloud computer with passwordless `sudo`. | OS-user isolation is impossible. Anything readable by any process is readable by the agent. |
| The platform's "secure secret request" stores values **in plaintext** on the machine, injects them into every new process, and lets the agent reuse them without the user being notified. | Key material must **never** pass through the platform secret store. |
| Routines support **webhook triggers** (`POST` + `Authorization: Bearer crsr_…`, JSON body delivered to the agent). | Rubi can wake the agent on events without a custom receiver. |
| Auto Review / "Ask first" rules can require approval for plugin calls. | Available as an additional, lighter approval level ("chat"). |

## 3. Threat model

**In scope: an honest but manipulable agent.** The agent follows instructions, but it can be misled by
prompt injection (emails, web pages, documents) or simply make mistakes. Rubi must ensure that such an
agent **cannot**:

- start or unlock Rubi without the user;
- perform an approval-gated action without the user's approval of *that exact action*;
- weaken security settings (approval levels, webhook targets, integrations, secrets);
- read the user's key or derive it from anything stored on the machine.

Also in scope: leaked files, backups, or snapshots of the agent machine; other bots on the same account;
a malicious network path between the panel and Rubi; a phishing link to a fake panel.

**Out of scope (documented limitations):**

- **A deliberately malicious agent with root.** It can read Rubi's process memory while Rubi is unlocked,
  or replace Rubi's binary so that it records the next unlock. No design can prevent this on a machine the
  attacker controls. Signed binaries make this harder and easier to detect, but they cannot prevent it.
- **Secrets in use.** To talk IMAP/SMTP, Rubi must hold a service password in plaintext while unlocked.
- **Connection metadata** (timing, IP addresses) visible to the tunnel provider.

## 4. Components

```
 User devices (iPhone / Mac / any browser)           Agent machine (Grok Bot cloud computer)
 ┌──────────────────────────────────────┐            ┌───────────────────────────────────────────┐
 │ Panel  https://rubi-panel.com        │  E2E over  │ Rubi daemon (signed static binary)        │
 │  static HTML/JS, no backend, no data │◀─────────▶ │  • vault (encrypted at rest)              │
 │  • passkey / password                │  tunnel or │  • policy engine & approvals              │
 │  • key derivation (never leaves)     │  Tailscale │  • integrations (iCloud Mail, …)          │
 │  • action previews & approvals       │            │  • event bus → routine webhook            │
 └──────────────────────────────────────┘            │  • MCP server ◀── agent (local only)      │
                                                     └───────────────────────────────────────────┘
```

- **Rubi daemon** is distributed as a signed binary built in CI with provenance attestations. The installer
  verifies the signature before installing. Language: Go (single static binary).
- **Panel** is a static site served from `rubi-panel.com`, built from the public repo, versioned, published
  with integrity hashes. It has no server-side logic and stores nothing server-side. Advanced users can
  self-host it (their passkeys are then bound to their own domain).
- **Agent** talks to Rubi over MCP on the loopback interface only. The agent never talks to the panel.

## 5. Keys

| Key | Where it lives | Purpose |
|---|---|---|
| `IK` instance identity (X25519 + Ed25519) | Agent machine, unencrypted (needed before unlock) | Lets the panel authenticate Rubi and encrypt to it. Pinned by the panel on first pairing. |
| `DEK` data encryption key (256-bit, random) | Agent machine, **only wrapped**; plaintext only in Rubi's memory while unlocked | Encrypts the vault (secrets, config, policy, state). |
| `KEK_pk` | **Never stored anywhere.** Re-derived in the browser from the passkey's PRF output | Wraps `DEK`. |
| `KEK_pw` | **Never stored anywhere.** Re-derived in the browser with Argon2id from the password | Wraps `DEK` (fallback). |
| `AK` approval credential (passkey public key) | Inside the vault | Verifies approval signatures. |

Properties:
- **KEKs never leave the user's device.** The panel downloads the wrapped `DEK`, unwraps it locally, and
  sends `DEK` to Rubi over the end-to-end channel. The KEKs, from which new unlocks could be made, never
  reach the agent machine.
- **Vault integrity.** The vault uses AEAD (AES-256-GCM or XChaCha20-Poly1305) with the vault version as
  associated data. Without `DEK`, the agent can neither read nor forge config or policy changes, and
  rollback to an older vault is detected by a monotonic version counter kept inside the vault and mirrored
  in the panel.

## 6. Transport

- Rubi exposes its panel API over HTTPS. **The user configures nothing:** Rubi picks the first transport
  that works, in this order:
  1. A Cloudflare **quick tunnel** (`cloudflared tunnel --url …`). No account, no config; Rubi starts and
     supervises it. The address changes on every restart, so the agent always sends the current link.
     Traffic is tiny (pairing, unlocks, approvals), well within quick-tunnel limits, but there is no uptime
     guarantee.
  2. Other account-free tunnel providers as fallbacks.
  3. **Optional:** Tailscale (`tailscale serve`, stable `*.ts.net` address, private to the tailnet), for
     users who opt in.
- A changing endpoint is harmless: the panel identifies the instance by its pinned `IK`, not by its URL.
- **End-to-end encryption on top of HTTPS:** every panel request is encrypted to `IK` (HPKE-style:
  X25519 + HKDF-SHA256 + AES-256-GCM, via WebCrypto) and every response is authenticated by `IK`. The
  tunnel provider sees only ciphertext.
- CORS allows only `https://rubi-panel.com` (or the configured self-hosted panel origin).
- The MCP endpoint for the agent listens on loopback only and is never exposed through the tunnel.

## 7. Protocols

### 7.1 Pairing (first run)

1. The agent installs Rubi through chat. Rubi starts in the **unpaired** state, generates `IK`, opens the
   tunnel, and creates a one-time **pairing code** (128-bit, valid for 15 minutes, single use).
2. The agent sends the user a link:
   `https://rubi-panel.com/#v=1&e=<endpoint>&k=<IK public key>&p=<pairing code>`
   Everything after `#` stays in the browser and is never sent to `rubi-panel.com`.
   Every other link carries a short-lived **ticket** instead of a pairing code. The panel API serves
   nothing beyond a bare `hello` without a valid pairing code or ticket, so someone who discovers the
   tunnel URL can't even fetch the wrapped keys. Wire details: [panel-protocol.md](panel-protocol.md).
3. The panel connects to `<endpoint>`, verifies that Rubi proves possession of `k`, and pins `k`
   (trust on first use).
4. The user creates a passkey (RP ID `rubi-panel.com`, PRF extension) and/or a password.
5. The panel generates `DEK`, derives the KEK(s), wraps `DEK`, and sends Rubi: the wrapped `DEK`(s), the
   passkey public key (`AK`), and `DEK` itself to unlock the first session. The pairing code is burned.
6. The user adds integrations in the panel. Service passwords (e.g. an iCloud app-specific password) are
   typed **only in the panel** and go straight into the vault.

### 7.2 Unlock (after every restart)

1. Rubi starts **locked**. Every MCP tool returns `{"status": "locked", "unlock_url": …}`; the agent relays
   the link.
2. The panel fetches a fresh challenge and the wrapped `DEK`; the user authenticates (Face ID / password);
   the panel derives the KEK, unwraps `DEK`, and sends it to Rubi bound to the challenge.
3. Rubi decrypts the vault, starts integrations, and records an unlock receipt (time, panel origin,
   method). Receipts are visible in the panel, so unexpected unlocks stand out.

### 7.3 Approving an action

1. The agent calls a gated tool (e.g. `icloud_mail_prepare_send`). Rubi stores the exact action and returns
   `{"status": "awaiting_approval", "approval_url": …}` (level "strong") or chat buttons (level "chat").
2. **Strong level:** the panel shows a preview rendered **from Rubi's stored action**, not from anything
   the agent says. The WebAuthn challenge is
   `SHA-256(canonical_json({instance, action_id, integration, kind, params_digest, nonce, expires}))`.
   The user confirms with Face ID; the panel returns the assertion.
3. Rubi verifies the signature with `AK`, the RP ID hash, the user-presence and user-verification flags,
   the sign counter, and the challenge, then executes exactly the stored action, once.
4. **Password approvals** use an HMAC over the same challenge with a password-derived approval key. This
   is weaker: the verifier must live in the unlocked vault, so a root attacker could forge approvals. The
   panel says so when the user picks a password.
5. Optional "trust for N minutes" per action kind: after one strong approval, later actions of the same
   kind skip re-approval until the window ends.

### 7.4 Key rotation

`DEK` is rotated when the set of credentials changes (a passkey or the password is added or removed) and on
demand from the panel. Rotation runs in the panel with every remaining credential present, so all wraps
are refreshed together. There is no automatic re-key on every unlock: it would invalidate wraps for
credentials not used in that session (e.g. the password after a passkey unlock), and it only defends
against a deliberately malicious agent, which is out of scope (§3).

## 8. Policy

| Action | Default level | User can change |
|---|---|---|
| Unlock after restart | strong | **No** |
| Change approval levels, integrations, service passwords, webhook target | strong | **No** |
| Send email | strong | Yes (`chat` or `none`) |
| Create draft | none | Yes |
| Read / search | none | Yes |

Levels: `none` (runs immediately), `chat` (agent shows buttons; optionally enforced by a Grok Bot
"Ask first" rule), `strong` (panel approval with passkey or password).

The policy lives inside the vault. Changing it is itself a strong-approved action, and it can only be done
from the panel; no MCP tool can change policy. Locking Rubi is always allowed from chat.

## 9. Events and waking the agent

- Integrations emit events (e.g. "reply received"). Rubi delivers them to the configured Grok Bot routine
  webhook (`POST`, `Authorization: Bearer crsr_…`), with metadata only and fields from third parties
  marked as untrusted.
- The webhook URL and key are stored in the vault; changing them needs a strong approval, so a manipulated
  agent can't redirect events elsewhere.
- Background work (e.g. watching for replies) runs only while Rubi is unlocked. When an event can't be
  checked because Rubi is locked, the agent tells the user on their next conversation.

## 10. Updates

- **Discovery:** Rubi reads `https://rubi-panel.com/releases/latest.json` (published by the Pages workflow on
  every release) at start and every six hours, and tells the agent once per version
  (`rubi.update.available`). The feed is only a hint; nothing in it is trusted.
- **Approval:** updating changes the code that holds the user's key, so it's a strong action (`rubi.update`,
  locked). Before asking, Rubi verifies the release's signed checksums, so the approval screen states a
  checked fact.
- **Install:** after approval Rubi downloads the archive, verifies `SHA256SUMS` against the embedded Ed25519
  release key and the archive against its checksum, smoke-tests the binary, swaps it in, and keeps the old
  one as `rubi.prev` (`rubi rollback`).
- **Restart without re-unlocking:** the running daemon replaces itself with the verified binary (`exec`) and
  passes the vault key through an inherited pipe; the key never touches disk or the environment. This
  extends trust only to a binary that the running, trusted code has just verified. A deliberately malicious
  agent with root could already read the key from memory (§3), so this adds no new exposure.
- Test builds (version `dev` or `*-test`) may override the release key, download base and feed with
  environment variables, for end-to-end tests. Official builds ignore these variables.

## 11. Recovery

If the user loses every passkey and the password, the vault is unrecoverable by design. They reset Rubi
and pair again; service passwords such as iCloud app-specific passwords are re-issued by the service.
Users are encouraged to register at least two credentials (e.g. a passkey and a password).

## 12. Decisions and open questions

Decided:
- **Zero-config transport** (§6). Tailscale is optional.
- **Password fallback is automatic** when the device's passkey lacks PRF support (PRF is supported by
  iCloud Keychain passkeys on iOS/macOS 18+). The panel explains that a password is weaker.
- **Notifications go through the agent.** Rubi sends events to the Grok Bot routine webhook; the agent
  messages the user (and the Grok Bot app pushes it). No Web Push and no project server.
- **The Rubi binary checks its own integrity** at unlock against the published release hash, and the panel
  shows the result.
- **One user and one agent per instance** in v1.

- **The agent platform provides no inbound public address** (checked 2026-10-07): the machine sits behind
  NAT (private `172.30.0.0/16` address, shared egress IPv4, no IPv6). Only outbound-initiated transports
  (tunnels, Tailscale) can work.

- **Quick tunnels work from the agent machine** (tested 2026-10-07 with cloudflared 2025.9.1 linux/amd64):
  the tunnel came up over QUIC in about 5 s, was reachable from an iPhone over the public internet, and
  needed no account.

Open:
1. Long-run quick-tunnel stability (reconnects, URL changes). We will measure it with the prototype.
