#!/bin/sh
# Rubi Home installer: the helper for a computer at home (a Mac that stays on, or a Linux box) that lets
# Rubi control Philips Hue and, on a Mac, run your Shortcuts. (Windows has its own installer, install-home.ps1.)
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
  darwin-arm64 | darwin-amd64 | linux-amd64 | linux-arm64) ;;
  windows-* | mingw* | msys* | cygwin*) fail "this is the installer for macOS and Linux; on Windows the Bot runs install-home.ps1 (see https://rubi-panel.com/install.md)" ;;
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
have=$(installed_version "$DIR/bin/rubi-home")
if [ -z "$have" ]; then
  [ ! -e "$DIR/bin/rubi-home" ] || say "can't read the installed version; going on"
elif printf '%s\n' "$have" | grep -Eq "$version_re"; then
  if release_newer "$have" "$version"; then
    fail "$version is older than the installed $have; not installing (Rubi only updates forward)"
  fi
else
  say "the installed copy is a development build ($have); going on"
fi

base="${RUBI_DOWNLOAD_BASE:-https://github.com/$REPO/releases/download}/$version"
archive="rubi-home-$version-$platform.tar.gz"

say "downloading Rubi Home $version for $platform"
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
  # The systemd user unit is made by Rubi Home itself (the same code the apps use): the unit file, then daemon-reload,
  # enable --now and restart, as this script used to do.
  "$bin" install-autostart --bin "$bin" || fail "couldn't set Rubi Home up to start at login"
  say "running as a systemd user service (for it to run without you logged in: loginctl enable-linger)"
fi

say "installed Rubi Home $version"

# Claude Code's hooks run a root-owned copy of rubi-home that this installer can't (and shouldn't) replace
# without you: it only changes when you re-copy it, so it can silently fall behind. Warn, never fail.
hook=/usr/local/bin/rubi-home
if [ -x "$hook" ]; then
  new_v=$("$bin" version 2>/dev/null | head -n 1) || new_v=""
  # The old copy is run in the background and killed after 3 seconds, so a hung one can't hold the installer.
  old_v=""
  "$hook" version >"$tmp/hook-version" 2>/dev/null </dev/null &
  hook_pid=$!
  waited=0
  while kill -0 "$hook_pid" 2>/dev/null && [ "$waited" -lt 3 ]; do sleep 1; waited=$((waited + 1)); done
  if kill -0 "$hook_pid" 2>/dev/null; then
    kill -9 "$hook_pid" 2>/dev/null || true
    say "warning: $hook did not answer 'version' within 3 seconds"
  else
    old_v=$(head -n 1 "$tmp/hook-version" 2>/dev/null) || old_v=""
  fi
  wait "$hook_pid" 2>/dev/null || true
  if [ "$old_v" != "$new_v" ]; then
    say "warning: the Claude Code hook binary $hook is ${old_v:-of an unknown version}, Rubi Home is now ${new_v:-$version}."
    if [ "$os" = darwin ]; then
      say "update it in the Rubi panel (Settings, Home); macOS asks for your administrator password there."
    else
      say "update it in the Rubi panel (Settings, Home); your computer asks for the administrator password there, in its own window."
    fi
  fi
elif [ -e "$hook" ]; then
  say "warning: $hook exists but isn't an executable file; check it, then install the hook again from the Rubi panel (Settings, Home)."
else
  say "Claude Code hook (optional): not set up. To let Bots ask for approvals and message your Claude Code sessions, install the hook from the Rubi panel (Settings, Home)."
fi
echo
"$bin" pair
echo
if [ "$os" = darwin ]; then
  echo "Shortcuts: make a folder named \"Rubi\" in the Shortcuts app and put there only the shortcuts Rubi may run."
fi
}

main "$@"
