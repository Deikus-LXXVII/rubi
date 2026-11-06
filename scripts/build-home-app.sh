#!/bin/sh
# Builds the Rubi Home app for macOS: OUTDIR/rubi-home-VERSION-macos-ARCH.zip holding
# "Rubi Home.app" (Info.plist and the rubi-home binary). ARCH is universal for the release (one binary for
# Apple silicon and Intel, made with lipo; the panel links to that zip), or arm64 or amd64 for a local build.
#
#   scripts/build-home-app.sh v0.15.0 universal path/to/rubi-home dist
#
# POSIX sh, for the Linux release runner and for macOS. Building twice gives the same bytes: fixed file
# times, sorted entries, no extra fields.
#
# RUBI_APP_SIGN=adhoc seals the bundle with an ad-hoc signature (codesign, so macOS only). The release does
# this on a macOS runner: an unsealed bundle fails `codesign --verify`, and macOS may then call the app
# damaged instead of offering "Open Anyway" (see docs/design/rubi-home.md).
set -eu

fail() { printf 'build-home-app: %s\n' "$*" >&2; exit 1; }

[ "$#" -eq 4 ] || fail "usage: build-home-app.sh VERSION ARCH BINARY OUTDIR"
version=$1
arch=$2
binary=$3
outdir=$4

# Both go into file names and into the Info.plist: only a plain release tag and a known CPU.
case $version in
  *[!0-9A-Za-z.v-]*) fail "bad version" ;; # also refuses a newline, which a line-based grep would let through
esac
printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' || fail "bad version '$version'"
case "$arch" in
  universal | arm64 | amd64) ;;
  *) fail "bad arch '$arch' (universal, arm64 or amd64)" ;;
esac
[ -f "$binary" ] || fail "no binary at '$binary'"
[ -d "$outdir" ] || mkdir -p "$outdir"
outdir=$(cd "$outdir" && pwd)
command -v zip >/dev/null 2>&1 || fail "zip is required"

short=${version#v}
zipname="rubi-home-$version-macos-$arch.zip"

work=$(mktemp -d)
tmpzip="$outdir/.build-$$-$zipname" # ends in .zip, or zip would add the suffix itself
trap 'rm -rf "$work"; rm -f "$tmpzip"' EXIT
trap 'exit 130' INT TERM
app="$work/Rubi Home.app"
mkdir -p "$app/Contents/MacOS"
cp "$binary" "$app/Contents/MacOS/rubi-home"
chmod 755 "$app/Contents/MacOS/rubi-home"

cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleIdentifier</key><string>com.rubi-project.home</string>
  <key>CFBundleName</key><string>Rubi Home</string>
  <key>CFBundleDisplayName</key><string>Rubi Home</string>
  <key>CFBundleExecutable</key><string>rubi-home</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
  <key>CFBundleShortVersionString</key><string>$short</string>
  <key>CFBundleVersion</key><string>$short</string>
  <key>LSMinimumSystemVersion</key><string>13.0</string>
  <key>LSUIElement</key><true/>
</dict>
</plist>
PLIST
chmod 644 "$app/Contents/Info.plist"
chmod 755 "$app" "$app/Contents" "$app/Contents/MacOS"

case "${RUBI_APP_SIGN:-}" in
  "") ;;
  adhoc)
    command -v codesign >/dev/null 2>&1 || fail "RUBI_APP_SIGN=adhoc needs codesign (macOS)"
    out=$(codesign --sign - --force --timestamp=none "$app" 2>&1) || fail "codesign failed: $out"
    out=$(codesign --verify --strict "$app" 2>&1) || fail "the signed app does not verify: $out"
    ;;
  *) fail "RUBI_APP_SIGN is adhoc or unset" ;;
esac

# The same times in the zip whatever the machine's clock and zone.
TZ=UTC
export TZ
find "$app" -exec touch -t 200001010000 {} +

(
  cd "$work"
  find "Rubi Home.app" -type f | LC_ALL=C sort | zip -q -X -D "$tmpzip" -@
)
mv -f "$tmpzip" "$outdir/$zipname"
printf 'build-home-app: wrote %s\n' "$outdir/$zipname"
