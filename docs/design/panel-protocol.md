# Rubi-Project — Panel Protocol (v1)

Status: implemented on the Rubi side (M2). The web panel (M3) implements the client side with WebCrypto;
`internal/panelclient` is the reference implementation.

## Links

Rubi gives the agent links of this form; everything after `#` stays in the browser:

```
https://rubi-panel.com/#v=1&e=<endpoint>&k=<instance key>&a=<purpose>&p=<pairing code>   (pair)
https://rubi-panel.com/#v=1&e=<endpoint>&k=<instance key>&a=<purpose>&t=<ticket>         (everything else)
```

| Field | Meaning |
|---|---|
| `e` | Current public URL of the instance's panel API (a quick-tunnel URL; changes on restart) |
| `k` | Instance public bundle: X25519 public key (32 bytes) ‖ Ed25519 public key (32 bytes), base64url |
| `a` | `pair`, `unlock`, `settings`, `setup:<integration>`, `approve:<approval id>` |
| `p` | One-time pairing code (15 min, burned on successful pairing) |
| `t` | Ticket (15 min). Required for every operation except `hello` and `pair`. |

On first contact the panel **pins `k` per instance id** (from `hello`). If a later link for the same
instance carries a different `k`, the panel refuses and warns the user.

## Transport

- `GET  {e}/v1/ping` → `204`, no content (reachability only).
- `POST {e}/v1/rpc`, body `{"v":1,"epk":…,"nonce":…,"ct":…}`, response `{"nonce":…,"ct":…}`.
- CORS allows only the panel origin.

## End-to-end encryption

```
eph                   = fresh X25519 key pair per request
shared                = X25519(eph.private, k.x25519)
okm                   = HKDF-SHA256(ikm=shared, salt=eph.public || k.x25519, info="rubi e2e v1", L=64)
req_key, resp_key     = okm[0:32], okm[32:64]
ct (request)          = AES-256-GCM(req_key,  nonce12, envelope_json, ad="rubi-req|v1|/v1/rpc")
ct (response)         = AES-256-GCM(resp_key, nonce12, reply_json,    ad="rubi-resp|v1|" + rid)
```

A response that fails to decrypt did not come from the pinned instance; the panel must stop.
An undecryptable request gets a plain `400 bad request` with no details.

## Envelope and reply

```json
{"op": "unlock", "ts": 1791380000, "rid": "<random, single use>", "ticket": "<t>", "args": {…}}
{"ok": true, "result": {…}}        {"ok": false, "error": "human-readable message"}
```

Requests more than 2 minutes off Rubi's clock and replayed `rid`s are rejected. After 20 failed attempts
(bad pairing code, ticket, or key) within 10 minutes, authenticated operations are refused for the rest of
the window.

## Pairing a passkey

The panel creates a discoverable passkey (RP ID = panel host, `userVerification: "required"`) with the PRF
extension evaluated on a random 32-byte `prf_salt`. If PRF output is available, a `passkey` wrap is created;
either way the credential's SPKI public key (`getPublicKey()`, sent as standard base64) and algorithm are
registered as an approver. Without PRF, a password is required for unlocking.

## Operations

| op | Needs | args | result |
|---|---|---|---|
| `hello` | — | — | `instance`, `fingerprint`, `state`, `version` |
| `pair` | pairing code | `code`, `dek`, `wraps[]`, `approvers[]?`, `locale?`, `method`, `credential_id?` | `state`, `vault_version` |
| `unlock.keys` | ticket | — | `instance`, `wraps[]`, `purpose` |
| `unlock` | ticket | `dek`, `min_vault_version`, `method`, `credential_id?` | `state`, `vault_version` |
| `status` | ticket | — | `state`, `purpose`, `vault_version`, `receipts[]`, `integrations` (when unlocked) |
| `lock` | ticket | — | `state` |
| `approval.get` | ticket for `approve:<id>` | `approval_id` | `approval` (summary, preview, question, options, state), `challenges` (per option), `approvers[]`, `password`, `rp_id` |
| `approval.decide` | ticket for `approve:<id>` | `approval_id`, `option`, `approve`, `proof` | the approval's new state and result |

### Approval proofs

The challenge for an option is
`SHA-256("rubi-approve|v1|" + instance + "|" + approval_id + "|" + option + "|" + nonce + "|" + hex(SHA-256(preview JSON)))`,
computed by Rubi; the panel only signs the bytes it is given.

- **Passkey:** `navigator.credentials.get` with that challenge, `userVerification: "required"`, limited to the
  registered credentials. Proof: `{"type":"passkey","credential_id","client_data_json","authenticator_data","signature"}`
  (base64url). Rubi checks type, challenge, origin, RP ID hash, UP+UV flags, the signature counter, and the
  signature against the public key registered at pairing.
- **Password:** `{"type":"password","mac": HMAC-SHA256(approve key, challenge)}` where
  `approve key = HKDF-SHA256(password KEK, info="rubi approve v1")`. Rubi holds the approve key, so this is
  weaker than a passkey.
- Declining (`approve: false`) needs no proof.

The panel stores the highest `vault_version` it has seen per instance and sends it as
`min_vault_version`, so Rubi refuses to open a rolled-back vault.

## Key wrapping (on the user's device only)

```
password KEK = Argon2id(password, salt16, t=3, m=64 MiB, p=1, 32 bytes)
passkey  KEK = HKDF-SHA256(ikm=PRF output, salt=prf_salt, info="rubi kek v1", 32 bytes)
wrap         = AES-256-GCM(KEK, nonce12, DEK, ad="rubi-wrap|v1|" + instance + "|" + wrap_id)
```

Wraps are stored by Rubi verbatim:

```json
{"id":"w_…","kind":"password","kdf":{"alg":"argon2id","salt":"…","time":3,"memory_kib":65536,"threads":1},
 "nonce":"…","wrapped":"…","label":"Password","created_at":"…"}
```

On unlock the panel fetches the wraps (`unlock.keys`), unwraps the DEK locally, and sends only the DEK.
KEKs, passwords and PRF outputs never leave the device.
