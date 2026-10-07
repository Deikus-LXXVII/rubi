# Rubi panel

The static web panel served at `https://rubi-panel.com`. It has no backend: it talks only to the user's own
Rubi instance, over the end-to-end encrypted channel described in
[docs/design/panel-protocol.md](../docs/design/panel-protocol.md).

- No build step, no framework, no third-party requests. Plain ES modules plus one vendored library
  (`vendor/hash-wasm`, Argon2id; see `vendor/hash-wasm/SOURCE` for version and checksum).
- Nothing received from Rubi or third parties is ever inserted as HTML.
- One-time codes are removed from the address bar as soon as the page reads them.
- The panel pins each instance's public key in `localStorage` and refuses links whose key changed.

## Local development

```bash
python3 -m http.server 8080 --bind 127.0.0.1 --directory panel
RUBI_HOME=/tmp/rubi-dev RUBI_DEV=1 \
RUBI_PANEL_ORIGIN=http://localhost:8080 RUBI_PUBLIC_URL=http://127.0.0.1:8790 RUBI_PANEL_API_PORT=8790 \
  rubi status
```

Open the `link` from `rubi status` in a browser. With `RUBI_DEV=1`, the MCP tool
`rubi_dev_request_approval` creates a fake approval for testing the approval screen.

Passkeys need HTTPS (or `localhost`) and an authenticator. To try them on a phone before the domain is
live, serve `panel/` through any HTTPS tunnel and set `RUBI_PANEL_ORIGIN` to that origin.

## Deployment

Any static host works. Serve the directory as is, over HTTPS, at the origin configured as Rubi's
`RUBI_PANEL_ORIGIN` (default `https://rubi-panel.com`). Passkeys are bound to that domain, so it must stay
stable.
