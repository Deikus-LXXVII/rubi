#!/bin/sh
# Build this checkout and start a disposable Rubi, panel, fake Home and OAuth stub
# inside a rootless user/network/PID namespace. Safe to run again: if that sandbox is
# already up, this exits 0.
set -eu
cd "$(dirname "$0")"
. ./lib.sh

ROOT=$(sandbox_root)
DIR=$(sandbox_dir)
sandbox_refuse_live "$DIR"

if [ -f "$DIR/run/ns.pid" ] && [ -f "$DIR/run/daemon.ready" ]; then
  pid=$(cat "$DIR/run/ns.pid" 2>/dev/null || true)
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    printf 'rubi-sandbox: already up\n'
    exit 0
  fi
fi

command -v unshare >/dev/null 2>&1 || sandbox_die "unshare is required (rootless user, network and PID namespaces)"
command -v go >/dev/null 2>&1 || sandbox_die "go is required"
command -v python3 >/dev/null 2>&1 || sandbox_die "python3 is required"

mkdir -p "$DIR/bin" "$DIR/keys" "$DIR/state" "$DIR/run" "$DIR/files" "$DIR/logs" "$DIR/tmp" "$DIR/home" "$DIR/rubi" "$DIR/src"
sandbox_write_marker "$DIR"
chmod 700 "$DIR" "$DIR/keys" "$DIR/state" "$DIR/run" "$DIR/rubi" "$DIR/home"

# Keys first so the snapshot helper can use the binary we are about to build, and so a live
# status call never sees the sandbox's variables (it is invoked with them unset).
"$DIR/bin/rubi-sandbox" keys --dir "$DIR/keys" >/dev/null 2>"$DIR/logs/keys.err" || {
  # binary may not exist yet
  true
}

(
  cd "$ROOT"
  go build -o "$DIR/bin/rubi" ./cmd/rubi
  go build -o "$DIR/bin/rubi-sandbox" ./cmd/rubi-sandbox
)
"$DIR/bin/rubi-sandbox" keys --dir "$DIR/keys" >/dev/null

sandbox_snapshot_live "$DIR/run/live-before.json" "$DIR/bin/rubi"

pub=$(tr -d '\n' < "$DIR/keys/plugin.pub.b64")
plugin_source="fallback"
if command -v git >/dev/null 2>&1; then
  if [ ! -d "$DIR/src/rubi-t3agents/.git" ]; then
    git clone --depth 1 https://github.com/Deikus-LXXVII/rubi-t3agents.git "$DIR/src/rubi-t3agents" >>"$DIR/logs/plugin-build.log" 2>&1 || true
  fi
  if [ -d "$DIR/src/rubi-t3agents/.git" ]; then
    # The setup-action fix is not on the plugin's main yet (this environment cannot push to that
    # repo). Apply it onto a clean clone so the sandbox runs that version.
    if ! (
      cd "$DIR/src/rubi-t3agents"
      git checkout --force HEAD
      git clean -fd
      git apply "$ROOT/scripts/sandbox/t3agents-setup.patch"
      go mod edit -replace "github.com/Deikus-LXXVII/rubi=$ROOT"
      go build -mod=mod -ldflags "-X main.PublisherKey=$pub" -o "$DIR/bin/t3agents" .
    ) >>"$DIR/logs/plugin-build.log" 2>&1; then
      printf 'rubi-sandbox: T3 Agents plugin did not build with scripts/sandbox/t3agents-setup.patch (see logs/plugin-build.log)\n' >&2
      exit 1
    fi
    plugin_source="https://github.com/Deikus-LXXVII/rubi-t3agents"
  fi
fi
if [ "$plugin_source" = "fallback" ]; then
  (
    cd "$ROOT"
    go build -ldflags "-X main.PublisherKey=$pub" -o "$DIR/bin/t3agents" ./scripts/sandbox/plugin
  )
  printf 'rubi-sandbox: using the in-repo T3 stand-in (the GitHub plugin did not build; see logs/plugin-build.log)\n'
fi
printf '%s\n' "{\"id\":\"t3agents\",\"source\":\"$plugin_source\"}" > "$DIR/plugin.json"

"$DIR/bin/rubi-sandbox" publish \
  --keys "$DIR/keys" \
  --plugin "$DIR/bin/t3agents" \
  --out "$DIR/files" \
  --source "http://127.0.0.1:18081"

cat > "$DIR/run/supervisor.sh" << EOF
#!/bin/sh
set -eu
"$DIR/bin/rubi-sandbox" lo
"$DIR/bin/rubi-sandbox" stack --state "$DIR/state" --run "$DIR/run" --files "$DIR/files" >>"$DIR/logs/stack.log" 2>&1 &
i=0
while [ ! -f "$DIR/run/stack.ready" ]; do
  i=\$((i + 1))
  [ "\$i" -lt 100 ] || { echo "stack did not become ready" >&2; exit 1; }
  sleep 0.1
done
python3 -m http.server 18080 --bind 127.0.0.1 --directory "$ROOT/panel" >>"$DIR/logs/panel.log" 2>&1 &
env -i \\
  PATH="\$PATH" \\
  HOME="$DIR/home" \\
  TMPDIR="$DIR/tmp" \\
  LANG="\${LANG:-C.UTF-8}" \\
  RUBI_HOME="$DIR/rubi" \\
  RUBI_DEV=1 \\
  RUBI_PANEL_ORIGIN="http://127.0.0.1:18080" \\
  RUBI_PUBLIC_URL="http://127.0.0.1:18790" \\
  RUBI_PANEL_API_PORT=18790 \\
  RUBI_RELAYS="ws://127.0.0.1:18791" \\
  RUBI_MARKETPLACE_URL="http://127.0.0.1:18081/marketplace/catalog.json" \\
  RUBI_TEST_RELEASE_KEY_FILE="$DIR/keys/release.pub.pem" \\
  RUBI_TEST_PLUGIN_KEY="$pub" \\
  "$DIR/bin/rubi" daemon >>"$DIR/logs/daemon.log" 2>&1 &
i=0
while [ ! -S "$DIR/rubi/rubi.sock" ]; do
  i=\$((i + 1))
  [ "\$i" -lt 100 ] || { echo "daemon socket did not appear" >&2; exit 1; }
  sleep 0.1
done
env -i \\
  PATH="\$PATH" \\
  HOME="$DIR/home" \\
  RUBI_HOME="$DIR/rubi" \\
  "$DIR/bin/rubi-sandbox" mcp --socket "$DIR/rubi/rubi.sock" --tool rubi_status --args null --out "$DIR/run/status.json" \\
  >>"$DIR/logs/status.log" 2>&1 || true
touch "$DIR/run/daemon.ready"
wait
EOF
chmod 700 "$DIR/run/supervisor.sh"

# Drop inherited signing material before entering the namespace. The supervisor sets only its own.
unset RUBI_TEST_RELEASE_KEY_FILE RUBI_TEST_PLUGIN_KEY RUBI_TEST_ANNOUNCE_PUB \
  RUBI_DOWNLOAD_BASE RUBI_UPDATE_FEED RUBI_HOME RUBI_HOME_DIR RUBI_HOME_INSTALL \
  RUBI_MARKETPLACE_URL RUBI_PUBLIC_URL RUBI_PANEL_ORIGIN RUBI_RELAYS RUBI_TUNNEL RUBI_CLOUDFLARED \
  RUBI_HOOK_BINARY || true

rm -f "$DIR/run/stack.ready" "$DIR/run/daemon.ready"
nohup unshare --user --map-root-user --net --mount --pid --fork --mount-proc "$DIR/run/supervisor.sh" >>"$DIR/logs/supervisor.log" 2>&1 &
echo $! > "$DIR/run/unshare.pid"
i=0
ns=""
while [ "$i" -lt 50 ]; do
  ns=$(ps -o pid= --ppid "$(cat "$DIR/run/unshare.pid")" 2>/dev/null | awk 'NR==1{print $1}')
  if [ -n "$ns" ]; then
    break
  fi
  i=$((i + 1))
  sleep 0.1
done
[ -n "$ns" ] || sandbox_die "the sandbox namespace did not start"
printf '%s\n' "$ns" > "$DIR/run/ns.pid"

i=0
while [ ! -f "$DIR/run/daemon.ready" ]; do
  i=$((i + 1))
  if [ "$i" -gt 150 ]; then
    printf 'rubi-sandbox: daemon did not become ready. Last log lines:\n' >&2
    tail -n 40 "$DIR/logs/supervisor.log" "$DIR/logs/stack.log" "$DIR/logs/daemon.log" >&2 || true
    exit 1
  fi
  if ! kill -0 "$ns" 2>/dev/null; then
    sandbox_die "the sandbox namespace exited before it was ready"
  fi
  sleep 0.2
done
printf 'rubi-sandbox: up (%s)\n' "$plugin_source"
printf 'rubi-sandbox: dir %s\n' "$DIR"
