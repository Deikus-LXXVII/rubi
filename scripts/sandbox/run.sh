#!/bin/sh
# Run a command inside the sandbox network and PID namespace.
set -eu
cd "$(dirname "$0")"
. ./lib.sh
DIR=$(sandbox_dir)
sandbox_check_marker "$DIR"
[ -f "$DIR/run/ns.pid" ] || sandbox_die "sandbox is not up"
pid=$(cat "$DIR/run/ns.pid")
kill -0 "$pid" 2>/dev/null || sandbox_die "sandbox is not up"
exec nsenter -t "$pid" --user --net --mount --pid --preserve-credentials -- "$@"
