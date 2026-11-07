#!/bin/sh
# Stop the sandbox. Idempotent. Does not delete its directory (see reset.sh).
set -eu
cd "$(dirname "$0")"
. ./lib.sh
DIR=$(sandbox_dir)
if [ ! -d "$DIR" ]; then
  printf 'rubi-sandbox: not running\n'
  exit 0
fi
sandbox_check_marker "$DIR" || true
if [ ! -f "$(sandbox_marker "$DIR")" ]; then
  sandbox_die "refusing to stop $DIR: it has no sandbox marker"
fi

stop_one() {
  pidfile=$1
  [ -f "$pidfile" ] || return 0
  pid=$(cat "$pidfile" 2>/dev/null || true)
  if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
    kill -TERM "$pid" 2>/dev/null || true
    i=0
    while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 50 ]; do
      i=$((i + 1))
      sleep 0.1
    done
    kill -KILL "$pid" 2>/dev/null || true
  fi
  rm -f "$pidfile"
}

# The namespace's pid 1 is the supervisor. Stopping it destroys the namespace and its children.
stop_one "$DIR/run/ns.pid"
stop_one "$DIR/run/unshare.pid"
rm -f "$DIR/run/stack.ready" "$DIR/run/daemon.ready"
# A daemon that somehow escaped the namespace (it should not) is still ours if its socket is in this directory.
if [ -S "$DIR/rubi/rubi.sock" ]; then
  rm -f "$DIR/rubi/rubi.sock"
fi
printf 'rubi-sandbox: down\n'
