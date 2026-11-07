#!/bin/sh
# Stop the sandbox and delete its directory. Refuses any directory that is not a sandbox.
set -eu
cd "$(dirname "$0")"
. ./lib.sh
DIR=$(sandbox_dir)
if [ ! -d "$DIR" ]; then
  printf 'rubi-sandbox: nothing to reset\n'
  exit 0
fi
sandbox_refuse_live "$DIR"
sandbox_check_marker "$DIR"
./down.sh
sandbox_check_marker "$DIR"
rm -rf "$DIR"
printf 'rubi-sandbox: reset\n'
