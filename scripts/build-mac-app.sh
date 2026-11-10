#!/usr/bin/env bash
# Builds and seals the universal Mac app (rubi-home + rubi-agent-exec + pinned srt runtime), then smoke-tests the sealed
# native Node. The release runs it for a tag; CI runs it on every change so a sealing failure can't first appear at release.
# Usage: scripts/build-mac-app.sh <version>   (macOS with codesign, Go and Node)
set -euo pipefail
version=${1:?usage: build-mac-app.sh <version>}
mkdir -p app work/home-darwin-universal
for arch in arm64 amd64; do
  out="work/home-darwin-$arch"
  mkdir -p "$out"
  CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -buildid= -X github.com/Deikus-LXXVII/rubi/internal/version.Version=${version}" \
    -o "$out/rubi-home" ./cmd/rubi-home
  CGO_ENABLED=0 GOOS=darwin GOARCH=$arch go build -trimpath -ldflags "-s -w -buildid=" -o "$out/rubi-agent-exec" ./cmd/rubi-agent-exec
  python3 scripts/srt-runtime/build.py --platform "darwin-$arch" --dest work/home-darwin-universal
done
lipo -create -output work/home-darwin-universal/rubi-agent-exec work/home-darwin-arm64/rubi-agent-exec work/home-darwin-amd64/rubi-agent-exec
test -d work/home-darwin-universal/srt-runtime
lipo -create -output work/home-darwin-universal/rubi-home work/home-darwin-arm64/rubi-home work/home-darwin-amd64/rubi-home
lipo -archs work/home-darwin-universal/rubi-home
RUBI_APP_SIGN=adhoc sh scripts/build-home-app.sh "$version" universal work/home-darwin-universal/rubi-home app
# Check the sealed native Node too: re-signing must retain JIT entitlements.
mkdir -p work/mac-app-smoke
unzip -q "app/rubi-home-${version}-macos-universal.zip" -d work/mac-app-smoke
pin=$(python3 -c 'import runpy; print(runpy.run_path("scripts/srt-runtime/build.py")["identity"]())')
"work/mac-app-smoke/Rubi Home.app/Contents/MacOS/srt-runtime/$pin/darwin-$(go env GOARCH)/bin/node" -e 'let f=x=>x+1; for(let i=0;i<100000;i++) if(f(i)!==i+1) process.exit(1)'
# The app is signed ad hoc only: the project does not plan a Developer ID signature, so macOS asks for
# "Open Anyway" on the first open. The Mac app is the fallback for users without a Bot; the Bot installs
# Rubi Home without it. See docs/design/rubi-home.md ("The Mac app")
