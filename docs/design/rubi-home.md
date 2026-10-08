# Rubi Home

Rubi runs on the agent's machine, far from the user's home. Some things can only be reached from the home
network: a Philips Hue Bridge, or Apple Home, which Apple opens only to apps on the user's own devices.
Rubi Home is a small helper for a computer at home (a Mac that stays on, or a Linux box) that does those
things on Rubi's request.

## Connection

- The helper is the server. It keeps a fixed routing key on the relays (Rubi Gateway and the public ones)
  and its own identity key (X25519 + Ed25519), stored in its directory (`~/Library/Application Support/Rubi
  Home` on a Mac).
- Rubi calls it with the panel protocol's encryption (`internal/e2e`, path `/home/v1/rpc`): only the helper
  can read a request, and only the helper can produce an answer Rubi accepts. Relays see ciphertext.
- Requests carry a random token. The helper keeps only a hash of each token it accepted; a request without
  a known token gets nothing. Requests are single-use and must be within 2 minutes of the helper's clock.

## Pairing

1. On the home computer: `curl -fsSL https://rubi-panel.com/install-home.sh | sh`. The installer verifies
   the release signature, starts the helper at login (LaunchAgent / systemd user service) and prints a
   pairing code: the helper's routing key, public identity and relays, plus a one-time secret valid for
   15 minutes. `rubi-home pair` prints a new one.
2. The user pastes the code into the panel (Settings > Rubi Home > Add a home computer).
3. Rubi calls `pair` with the secret and a new token, then asks the user to approve adding the computer
   (passkey or password). Removing it later also tells the helper to forget the token.

A new pairing replaces every earlier one on that computer: codes are made there by its owner, so a pairing
someone obtained from an older code ends at the next `rubi-home pair`. `rubi-home status` lists the
pairing with its time. The helper introduces itself by its model ("MacBook Pro"), not by a hostname that
often carries its owner's name.

## What it does

The helper does only what it was built for, whatever Rubi asks:

- **Philips Hue** (`hue.discover`, `hue.pair`, `hue.request`): finds bridges (mDNS, then Signify's
  discovery service), pairs when the user presses the link button, and passes GET and PUT requests to the
  bridge's resource API (`/clip/v2/resource/…`) only. Bridge addresses must be on the home network
  (private or link-local), and no request follows a redirect. The bridge's certificate is recorded at pairing
  (its name must be the bridge id) and must match on every later connection. The application key stays in
  Rubi's vault and comes with each request. Requests to one bridge are at least 100 ms apart.
- **Shortcuts** (`shortcuts.list`, `shortcuts.run`): lists and runs only the shortcuts in one folder (Rubi,
  by default; `rubi-home folder NAME`), by identifier, with a 90-second limit and 64 KB of output. This is
  how Apple Home is reached: HomeKit is only available to App Store or Mac Catalyst apps signed with a paid
  developer account, so the user builds shortcuts with Apple Home actions instead.

Plugins reach the helper through Rubi (`home.call`), limited to the capabilities their manifest declares;
the install screen shows them.
