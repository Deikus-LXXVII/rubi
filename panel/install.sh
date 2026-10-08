#!/bin/sh
# Rubi-Project installer.
#
#   curl --proto =https --tlsv1.2 -fsSL https://rubi-panel.com/install.sh | sh              # latest release
#   curl --proto =https --tlsv1.2 -fsSL https://rubi-panel.com/install.sh | sh -s -- v0.1.0 # a specific version
#
# Installs into $RUBI_HOME (default ~/.rubi) without root:
#   - the rubi binary, after verifying the release's signed checksums against the key embedded below;
#   - cloudflared (for the zero-config panel connection), verified against a pinned checksum.
# Running it again upgrades in place.
set -eu

# Everything runs from main, called on the last line: if the download is cut short, nothing runs.
main() {

REPO="Deikus-LXXVII/rubi"
RUBI_HOME="${RUBI_HOME:-$HOME/.rubi}"

# Public key that signs every release's SHA256SUMS (the same key is embedded in the binary).
RELEASE_KEY='-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAjH/Smxzuzy59qWgOAVCSSbeNFzaZFqnFTgVPKQlgMIU=
-----END PUBLIC KEY-----'

CLOUDFLARED_VERSION="2026.10.0"
cloudflared_asset() {
  case "$1" in
    linux-amd64) echo "cloudflared-linux-amd64 d33ff2d14475178d2012c2c56beba87389ac5ded27649519f198a7d3134a99db" ;;
    linux-arm64) echo "cloudflared-linux-arm64 e6422b9d4f72d3194bc5a38676f13667c06666523217b842a877d72a80b5ac08" ;;
    darwin-arm64) echo "cloudflared-darwin-arm64.tgz a2f79ff7b9420aa537d74af239f376da170bbabeb529aec416002adac6a72e70" ;;
  esac
}

say() { printf 'rubi: %s\n' "$*"; }
fail() { printf 'rubi: error: %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || fail "'$1' is required but not installed"; }

need curl
need tar
need openssl
if command -v sha256sum >/dev/null 2>&1; then
  sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  fail "sha256sum or shasum is required"
fi

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) fail "unsupported CPU: $(uname -m)" ;;
esac
platform="$os-$arch"
case "$platform" in
  linux-amd64 | linux-arm64 | darwin-arm64) ;;
  *) fail "unsupported platform: $platform" ;;
esac

version="${1:-}"
if [ -z "$version" ]; then
  # The same release feed Rubi uses to find updates (GitHub's "latest release" API skips pre-releases).
  version=$(curl --proto =https --tlsv1.2 -fsSL "${RUBI_RELEASE_FEED:-https://rubi-panel.com/releases/latest.json}" |
    sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' | head -n 1)
  [ -n "$version" ] || fail "couldn't find the latest release"
fi
# Only a plain release tag: it becomes part of file names and URLs below.
printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' || fail "bad version '$version'"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
base="${RUBI_DOWNLOAD_BASE:-https://github.com/$REPO/releases/download}/$version"
archive="rubi-$version-$platform.tar.gz"

say "downloading Rubi $version for $platform"
curl --proto =https --tlsv1.2 -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"
curl --proto =https --tlsv1.2 -fsSL -o "$tmp/SHA256SUMS.sig" "$base/SHA256SUMS.sig"
curl --proto =https --tlsv1.2 -fsSL -o "$tmp/$archive" "$base/$archive"

printf '%s\n' "$RELEASE_KEY" > "$tmp/release-key.pem"
if ! openssl pkeyutl -verify -pubin -inkey "$tmp/release-key.pem" -rawin \
  -in "$tmp/SHA256SUMS" -sigfile "$tmp/SHA256SUMS.sig" >/dev/null 2>&1; then
  fail "the release signature doesn't verify; not installing (needs OpenSSL 1.1.1+ with Ed25519)"
fi
expected=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/SHA256SUMS")
[ -n "$expected" ] || fail "no checksum for $archive"
[ "$(sha256 "$tmp/$archive")" = "$expected" ] || fail "checksum mismatch for $archive; not installing"
say "signature and checksum verified"

mkdir -p "$RUBI_HOME/bin"
chmod 700 "$RUBI_HOME"
tar -xzf "$tmp/$archive" -C "$tmp" rubi
mv "$tmp/rubi" "$RUBI_HOME/bin/rubi.new"
chmod 755 "$RUBI_HOME/bin/rubi.new"
mv -f "$RUBI_HOME/bin/rubi.new" "$RUBI_HOME/bin/rubi"

# cloudflared, pinned by checksum.
cf="$RUBI_HOME/bin/cloudflared"
if [ ! -x "$cf" ] || ! "$cf" --version 2>/dev/null | grep -q "version $CLOUDFLARED_VERSION"; then
  set -- $(cloudflared_asset "$platform")
  cf_file=$1
  cf_sum=$2
  say "downloading cloudflared $CLOUDFLARED_VERSION"
  curl --proto =https --tlsv1.2 -fsSL -o "$tmp/$cf_file" "https://github.com/cloudflare/cloudflared/releases/download/$CLOUDFLARED_VERSION/$cf_file"
  [ "$(sha256 "$tmp/$cf_file")" = "$cf_sum" ] || fail "cloudflared checksum mismatch; not installing"
  case "$cf_file" in
    *.tgz) tar -xzf "$tmp/$cf_file" -C "$tmp" cloudflared && mv "$tmp/cloudflared" "$cf.new" ;;
    *) mv "$tmp/$cf_file" "$cf.new" ;;
  esac
  chmod 755 "$cf.new"
  mv -f "$cf.new" "$cf"
fi

# An upgrade takes effect when the daemon restarts. It restarts locked, so the user unlocks it again.
restarted=""
if pkill -f "$RUBI_HOME/bin/rubi daemon" 2>/dev/null; then
  restarted=1
fi

# How the user's browser reaches the panel: RUBI_TRANSPORT=gateway (default), relays or tailscale.
if [ -n "${RUBI_TRANSPORT:-}" ]; then
  "$RUBI_HOME/bin/rubi" transport "$RUBI_TRANSPORT" >/dev/null || fail "unknown RUBI_TRANSPORT: $RUBI_TRANSPORT"
fi
transport=$("$RUBI_HOME/bin/rubi" transport 2>/dev/null | sed -n 's/^transport: //p')

say "installed Rubi $version to $RUBI_HOME/bin/rubi (panel connection: ${transport:-gateway})"
cat <<EOF

Next steps for the agent:
  1. Add a custom MCP server named "rubi" that runs:
       $RUBI_HOME/bin/rubi mcp
  2. Call the rubi_status tool and give the user the link it returns.
     The user finishes setup in the Rubi panel with a passkey or a password.

Panel connection (ask the user if they haven't chosen): $RUBI_HOME/bin/rubi transport gateway|relays|tailscale
  gateway    Rubi Gateway, with public relays as a fallback (default, recommended)
  relays     public Nostr relays only
  tailscale  the user's own Tailscale network; the panel opens only on their Tailscale devices
EOF
if [ -n "$restarted" ]; then
  echo
  echo "Rubi was running and will restart locked with the new version on its next use; the user unlocks it again."
fi
}

main "$@"
