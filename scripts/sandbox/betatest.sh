#!/bin/sh
# Opt-in UI beta test. Installs Playwright locally (no root) and drives the sandbox panel.
set -eu
cd "$(dirname "$0")"
. ./lib.sh
ROOT=$(sandbox_root)
DIR=$(sandbox_dir)
./up.sh
BT="$ROOT/scripts/betatest"
if [ ! -d "$BT/node_modules/playwright" ]; then
  (cd "$BT" && npm install --no-fund --no-audit)
fi
export PLAYWRIGHT_BROWSERS_PATH="$DIR/browsers"
(cd "$BT" && npx playwright install chromium)
stamp=$(date -u +%Y%m%dT%H%M%SZ)
if mkdir -p /workspace/betatest 2>/dev/null; then
  out="/workspace/betatest/$stamp"
else
  out="$DIR/betatest/$stamp"
fi
mkdir -p "$out"
export BETATEST_OUT="$out"
export RUBI_SANDBOX_DIR="$DIR"
export RUBI_REPO="$ROOT"
# Chromium and the panel are inside the namespace so 127.0.0.1 is the sandbox, not the host.
./run.sh env \
  PLAYWRIGHT_BROWSERS_PATH="$PLAYWRIGHT_BROWSERS_PATH" \
  BETATEST_OUT="$out" \
  RUBI_SANDBOX_DIR="$DIR" \
  RUBI_REPO="$ROOT" \
  PATH="$PATH" \
  HOME="$DIR/home" \
  node "$BT/run.mjs"
printf 'rubi-sandbox: beta test output %s\n' "$out"
