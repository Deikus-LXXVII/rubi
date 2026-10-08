#!/bin/sh
# Rubi Home installer: the helper for a computer at home (a Mac that stays on, or a Linux box) that lets
# Rubi control Philips Hue and run your Shortcuts.
#
#   curl --proto =https --tlsv1.2 -fsSL https://rubi-panel.com/install-home.sh | sh
#
# Installs ~/.rubi-home/bin/rubi-home after verifying the release's signed checksums against the key
# below, starts it at login (a LaunchAgent on macOS, a systemd user service on Linux), and prints a
# pairing code to paste into the Rubi panel. Running it again upgrades in place.
set -eu

# Everything runs from main, called on the last line: if the download is cut short, nothing runs.
main() {

REPO="Deikus-LXXVII/rubi"
DIR="${RUBI_HOME_INSTALL:-$HOME/.rubi-home}"

RELEASE_KEY='-----BEGIN PUBLIC KEY-----
MCowBQYDK2VwAyEAjH/Smxzuzy59qWgOAVCSSbeNFzaZFqnFTgVPKQlgMIU=
-----END PUBLIC KEY-----'

say() { printf 'rubi-home: %s\n' "$*"; }
fail() { printf 'rubi-home: error: %s\n' "$*" >&2; exit 1; }
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
  darwin-arm64 | darwin-amd64 | linux-amd64 | linux-arm64) ;;
  *) fail "unsupported platform: $platform" ;;
esac

version="${1:-}"
if [ -z "$version" ]; then
  version=$(curl --proto =https --tlsv1.2 -fsSL "${RUBI_RELEASE_FEED:-https://rubi-panel.com/releases/latest.json}" |
    sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' | head -n 1)
  [ -n "$version" ] || fail "couldn't find the latest release"
fi
# Only a plain release tag: it becomes part of file names and URLs below.
printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' || fail "bad version '$version'"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM
base="${RUBI_DOWNLOAD_BASE:-https://github.com/$REPO/releases/download}/$version"
archive="rubi-home-$version-$platform.tar.gz"

say "downloading Rubi Home $version for $platform"
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

mkdir -p "$DIR/bin" "$DIR/logs"
chmod 700 "$DIR"
tar -xzf "$tmp/$archive" -C "$tmp" rubi-home
mv "$tmp/rubi-home" "$DIR/bin/rubi-home.new"
chmod 755 "$DIR/bin/rubi-home.new"
mv -f "$DIR/bin/rubi-home.new" "$DIR/bin/rubi-home"
bin="$DIR/bin/rubi-home"

if [ "$os" = darwin ]; then
  # A LaunchAgent runs in your login session, where Shortcuts can run. Keep the Mac from sleeping
  # (System Settings > Energy) so Rubi can reach it.
  plist="$HOME/Library/LaunchAgents/com.rubi-project.home.plist"
  mkdir -p "$HOME/Library/LaunchAgents"
  cat > "$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>com.rubi-project.home</string>
  <key>ProgramArguments</key><array><string>$bin</string><string>run</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>$DIR/logs/rubi-home.log</string>
  <key>StandardErrorPath</key><string>$DIR/logs/rubi-home.log</string>
</dict>
</plist>
PLIST
  launchctl bootout "gui/$(id -u)" "$plist" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$plist"
  say "running at login (LaunchAgent com.rubi-project.home)"
else
  unit="$HOME/.config/systemd/user/rubi-home.service"
  mkdir -p "$(dirname "$unit")"
  cat > "$unit" <<UNIT
[Unit]
Description=Rubi Home
After=network-online.target

[Service]
ExecStart=$bin run
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
UNIT
  systemctl --user daemon-reload
  systemctl --user enable --now rubi-home.service
  systemctl --user restart rubi-home.service
  say "running as a systemd user service (for it to run without you logged in: loginctl enable-linger)"
fi

say "installed Rubi Home $version"
echo
"$bin" pair
echo
echo "Shortcuts: make a folder named \"Rubi\" in the Shortcuts app and put there only the shortcuts Rubi may run."
}

main "$@"
