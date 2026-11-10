#!/bin/sh
# No credentials required. All state, remotes and services are disposable.
set -eu
cd "$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
if [ "$(uname -s)" != Linux ]; then echo 'SKIP: Linux-only acceptance'; exit 0; fi
for tool in bwrap srt node unshare setpriv python3 git socat rg; do
 if ! command -v "$tool" >/dev/null 2>&1; then echo "SKIP: missing $tool"; exit 0; fi
done
if ! bwrap --unshare-user --ro-bind / / /bin/true >/dev/null 2>&1; then echo 'SKIP: bwrap/user namespaces unavailable'; exit 0; fi
if [ ! -x "${RUBI_REAL_T3:-}" ] || [ ! -f "${RUBI_T3_COMPANION_DIR:-}/go.mod" ]; then
 echo 'SKIP: set RUBI_REAL_T3 to unmodified pinned native T3 and RUBI_T3_COMPANION_DIR to the companion source checkout'; exit 0
fi
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
CGO_ENABLED=0 go build -o "$work/rubi-agent-exec" ./cmd/rubi-agent-exec
export RUBI_REAL_RELAY="$work/rubi-agent-exec"
go test -tags rubi_acceptance -timeout 10m -count=1 -v ./internal/core -run '^TestT3AgentsCombinedAcceptance$'
