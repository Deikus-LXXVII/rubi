#!/usr/bin/env bash
# Tell a Claude Code session something from a script, and print its answer.
#
# Uses only Rubi Home's documented path: the message goes into Rubi Home's inbox, and the session's waiter hook
# (the asyncRewake Stop hook of the managed settings) wakes the session if it is idle. See
# docs/design/rubi-home.md. This file is an example: it is not installed anywhere. Prefer a session id
# (--session) to a title (--name) in scripts: titles can be influenced.
#
#   text="Which tests cover the parser?"
#   ./tell-session.sh "Remote control" "$text"
set -euo pipefail
name=${1:?usage: tell-session.sh SESSION_TITLE TEXT}
text=${2:?usage: tell-session.sh SESSION_TITLE TEXT}
printf '%s' "$text" | "${RUBI_HOME_BIN:-$HOME/.rubi-home/bin/rubi-home}" claude-send --name "$name" --from bot-a --wait-reply 300
