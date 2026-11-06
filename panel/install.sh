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

# The release-version rule (docs/design/rubi-home.md): the version comes from signed data, the signed checksums bind
# it, and a copy that is already installed is never replaced by an older release. Same order as update.Newer in
# internal/update: numbers, then a release beats its pre-release, then pre-release names as strings.
version_re='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'
release_newer() { # is release $1 newer than release $2?
  a=${1#v}; b=${2#v}
  case "$a" in *-*) a_pre=${a#*-} ;; *) a_pre= ;; esac
  case "$b" in *-*) b_pre=${b#*-} ;; *) b_pre= ;; esac
  a=${a%%-*}; b=${b%%-*}
  ifs=$IFS; IFS='. '
  set -- $a $b
  IFS=$ifs
  if [ "$1" != "$4" ]; then [ "$1" -gt "$4" ]; return; fi
  if [ "$2" != "$5" ]; then [ "$2" -gt "$5" ]; return; fi
  if [ "$3" != "$6" ]; then [ "$3" -gt "$6" ]; return; fi
  [ "$a_pre" != "$b_pre" ] || return 1
  [ -n "$a_pre" ] || return 0
  [ -n "$b_pre" ] || return 1
  [ "$(LC_ALL=C expr "x$a_pre" '>' "x$b_pre")" = 1 ]
}
# The version the installed copy reports, or nothing: it is not a plain file of this user's, this runs as root, or it
# can't run or takes over 5 seconds.
installed_version() {
  [ -x "$1" ] || return 0
  # Only a plain file of this user's is run, and never as root (root would run a file the user can change).
  [ ! -L "$1" ] && [ -O "$1" ] && [ "$(id -u)" != 0 ] || return 0
  "$1" version >"$tmp/installed-version" 2>/dev/null </dev/null &
  iv_pid=$!
  iv_waited=0
  while kill -0 "$iv_pid" 2>/dev/null && [ "$iv_waited" -lt 5 ]; do sleep 1; iv_waited=$((iv_waited + 1)); done
  if kill -0 "$iv_pid" 2>/dev/null; then kill -9 "$iv_pid" 2>/dev/null || true; wait "$iv_pid" 2>/dev/null || true; return 0; fi
  wait "$iv_pid" 2>/dev/null || return 0
  head -n 1 "$tmp/installed-version" 2>/dev/null || true
}

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

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
printf '%s\n' "$RELEASE_KEY" > "$tmp/release-key.pem"
feed="${RUBI_RELEASE_FEED:-https://rubi-panel.com/releases/latest.json}"
version="${1:-}"
if [ -z "$version" ]; then
  # The feed is signed (latest.json.sig, by the release key): the version comes only from bytes that verify.
  curl --proto =https --tlsv1.2 -fsSL -o "$tmp/latest.json" "$feed" || fail "couldn't find the latest release"
  if ! curl --proto =https --tlsv1.2 -fsSL -o "$tmp/latest.json.sig" "$feed.sig" ||
    ! openssl pkeyutl -verify -pubin -inkey "$tmp/release-key.pem" -rawin \
      -in "$tmp/latest.json" -sigfile "$tmp/latest.json.sig" >/dev/null 2>&1; then
    fail "the release feed is not signed with the Rubi release key; not installing"
  fi
  version=$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' "$tmp/latest.json" | head -n 1)
  [ -n "$version" ] || fail "couldn't find the latest release"
fi
# Only a plain release tag: it becomes part of file names and URLs below.
printf '%s\n' "$version" | grep -Eq "$version_re" || fail "bad version '$version'"

# Never an older release than the installed one (the same version again is a reinstall).
have=$(installed_version "$RUBI_HOME/bin/rubi")
if [ -z "$have" ]; then
  [ ! -e "$RUBI_HOME/bin/rubi" ] || say "can't read the installed version; going on"
elif printf '%s\n' "$have" | grep -Eq "$version_re"; then
  if release_newer "$have" "$version"; then
    fail "$version is older than the installed $have; not installing (Rubi only updates forward)"
  fi
else
  say "the installed copy is a development build ($have); going on"
fi

base="${RUBI_DOWNLOAD_BASE:-https://github.com/$REPO/releases/download}/$version"
archive="rubi-$version-$platform.tar.gz"

say "downloading Rubi $version for $platform"
curl --proto =https --tlsv1.2 -fsSL -o "$tmp/SHA256SUMS" "$base/SHA256SUMS"
curl --proto =https --tlsv1.2 -fsSL -o "$tmp/SHA256SUMS.sig" "$base/SHA256SUMS.sig"
curl --proto =https --tlsv1.2 -fsSL -o "$tmp/$archive" "$base/$archive"

if ! openssl pkeyutl -verify -pubin -inkey "$tmp/release-key.pem" -rawin \
  -in "$tmp/SHA256SUMS" -sigfile "$tmp/SHA256SUMS.sig" >/dev/null 2>&1; then
  fail "the release signature doesn't verify; not installing (needs OpenSSL 1.1.1+ with Ed25519)"
fi
# The signed list binds the version: every versioned asset in it (rubi-<v>-..., rubi-home-<v>-...) carries this version.
# Names without one (bin/rubi-linux-amd64) are ignored. Together with the archive lookup below, a genuine list of
# another release can't be passed off as this one.
other=$(sed -n -E 's/^[0-9a-f]+[ *]+rubi-(home-)?(v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?)-(linux|darwin|macos|windows)-.*/\2/p' "$tmp/SHA256SUMS" | grep -vxF "$version" | head -n 1)
[ -z "$other" ] || fail "the signed checksums are for $other, not $version; not installing"
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
