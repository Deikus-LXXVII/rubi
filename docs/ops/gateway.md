# Operating Rubi Gateway

Rubi Gateway (`cmd/rubi-gateway`, design in `internal/gateway`) is the default panel transport: a relay
that carries only Rubi's end-to-end encrypted panel traffic and the latest release announcement. Rubi knows
it only by name, `wss://gateway.rubi-panel.com`; nothing about the machine behind it is built in, published
or needed by the installer.

It also accepts hook requests (`POST /h/<route>/<id>`, at most 4 KB, rate-limited per address and per
route) and hands each to the Rubi subscribed with that route's secret; see `docs/design/plugins.md` (Hooks). The
tunnel already routes every path to the gateway, so nothing changes in the deployment.

It also watches Rubis whose users turned it on (`POST /watch`, its key at `GET /watch/key`; see
`internal/watch`): when a Rubi stops beating or stays locked, it wakes that Rubi's administrator Bot. It
keeps registrations in memory only (each beat carries one, sealed to the gateway), and its watch key in
the systemd state directory (`StateDirectory=rubi-gateway`), which must survive redeploys.

## Layout

```
browser / Rubi ──wss──▶ Cloudflare edge ──Cloudflare Tunnel (outbound from the VPS)──▶ 127.0.0.1:7447 rubi-gateway
```

- **No inbound port** for the gateway: the VPS dials out to Cloudflare. `gateway.rubi-panel.com` resolves to
  Cloudflare, so the VPS address is not tied to Rubi in DNS.
- The service listens on `127.0.0.1:7447` only and runs under systemd sandboxing (`deploy/gateway/
  rubi-gateway.service`: DynamicUser, read-only system, no capabilities, syscall filter, memory limit;
  `systemd-analyze security` exposure 1.2).
- It stores nothing on disk and logs no client addresses.
- SSH to the VPS is allowed only on the Tailscale interface; the public SSH port is closed in the firewall.

The VPS is shared with other services, so nothing here resets its firewall or SSH settings: Rubi only
adds a service with no public port, a tunnel, and the Tailscale-only SSH rule.

## Deploy or update the binary

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o /tmp/rubi-gateway ./cmd/rubi-gateway
scp /tmp/rubi-gateway deploy/gateway/rubi-gateway.service gateway-host:/tmp/
ssh gateway-host 'install -m 0755 /tmp/rubi-gateway /usr/local/bin/ && install -m 0644 /tmp/rubi-gateway.service /etc/systemd/system/ && systemctl daemon-reload && systemctl restart rubi-gateway'
```

## Cloudflare Tunnel (once the domain is on Cloudflare)

```bash
ssh gateway-host 'cloudflared tunnel login'                       # prints a URL; the owner authorizes rubi-panel.com
ssh gateway-host 'cloudflared tunnel create rubi-gateway && cloudflared tunnel route dns rubi-gateway gateway.rubi-panel.com'
```

Then `/etc/cloudflared/config.yml` routes `gateway.rubi-panel.com` to `http://127.0.0.1:7447` (WebSockets
pass through), and `cloudflared service install` runs it under systemd.

## Fresh servers

`deploy/gateway/harden.sh` hardens a fresh, dedicated Debian/Ubuntu VPS (it resets the firewall, so don't
run it on a shared one): automatic security updates, keys-only SSH without root, kernel network hardening,
Tailscale and cloudflared, and a deny-all firewall. Public SSH closes only with `CLOSE_PUBLIC_SSH=1`, after SSH
over Tailscale is confirmed.
