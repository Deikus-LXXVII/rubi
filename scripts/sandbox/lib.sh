# Shared helpers for the disposable Rubi sandbox. Source this; do not execute it.
# Nothing here prints signing keys, pairing codes, session tokens or OAuth material.

sandbox_root() {
  # Scripts cd into scripts/sandbox before sourcing this file, so the repository
  # root is two levels up from that directory. $0 is not reliable after that cd.
  here=$(pwd)
  if [ -f "$here/lib.sh" ] && [ -f "$here/../../go.mod" ]; then
    CDPATH= cd -- "$here/../.." && pwd
    return
  fi
  base=$(CDPATH= cd -- "$(dirname "$0")" && pwd)
  CDPATH= cd -- "$base/../.." && pwd
}

sandbox_die() {
  printf 'rubi-sandbox: %s\n' "$*" >&2
  exit 1
}

sandbox_dir() {
  if [ -n "${RUBI_SANDBOX_DIR:-}" ]; then
    printf '%s\n' "$RUBI_SANDBOX_DIR"
    return
  fi
  printf '%s\n' "$(sandbox_root)/.sandbox"
}

# Refuse to operate on a live Rubi or Rubi Home directory.
sandbox_refuse_live() {
  dir=$1
  case "$dir" in
    ""|"/"|"$HOME"|*) ;;
  esac
  real=$(cd "$dir" 2>/dev/null && pwd -P || printf '%s\n' "$dir")
  for live in "$HOME/.rubi" "$HOME/.rubi-home"; do
    if [ -d "$live" ]; then
      live_real=$(cd "$live" && pwd -P)
      if [ "$real" = "$live_real" ]; then
        sandbox_die "refusing to use the live directory $live"
      fi
    fi
  done
  case "$real" in
    */.rubi|*/.rubi/*|*/.rubi-home|*/.rubi-home/*) sandbox_die "refusing a path that looks like a live Rubi home: $real" ;;
  esac
}

sandbox_marker() {
  printf '%s\n' "$1/.rubi-sandbox"
}

sandbox_write_marker() {
  dir=$1
  mkdir -p "$dir"
  printf '%s\n' '{"kind":"rubi-sandbox","version":1}' > "$(sandbox_marker "$dir")"
}

sandbox_check_marker() {
  dir=$1
  marker=$(sandbox_marker "$dir")
  [ -f "$marker" ] || sandbox_die "not a sandbox (no marker): $dir"
  grep -q '"kind":"rubi-sandbox"' "$marker" || sandbox_die "sandbox marker is not valid: $dir"
}

# A live daemon is one whose socket already exists. Never start one to look.
sandbox_live_socket() {
  if [ -n "${RUBI_LIVE_HOME:-}" ]; then
    printf '%s\n' "$RUBI_LIVE_HOME/rubi.sock"
    return
  fi
  printf '%s\n' "$HOME/.rubi/rubi.sock"
}

sandbox_snapshot_live() {
  out=$1
  sock=$(sandbox_live_socket)
  if [ ! -S "$sock" ]; then
    printf '%s\n' '{"present":false}' > "$out"
    return
  fi
  bin=$2
  if [ ! -x "$bin" ]; then
    printf '%s\n' '{"present":true,"read":false}' > "$out"
    return
  fi
  # The live process is already running. status only connects; it does not start a second daemon
  # when the socket accepts. Strip sandbox variables so this call cannot retarget the live Rubi.
  if env -u RUBI_HOME -u RUBI_DEV -u RUBI_PANEL_ORIGIN -u RUBI_PUBLIC_URL -u RUBI_PANEL_API_PORT \
      -u RUBI_RELAYS -u RUBI_MARKETPLACE_URL -u RUBI_TEST_RELEASE_KEY_FILE -u RUBI_TEST_PLUGIN_KEY \
      -u RUBI_DOWNLOAD_BASE -u RUBI_UPDATE_FEED -u RUBI_TUNNEL -u RUBI_CLOUDFLARED \
      "$bin" status > "$out.raw" 2>"$out.err"; then
    sandbox_scrub_status "$out.raw" "$out"
  else
    printf '%s\n' '{"present":true,"read":false}' > "$out"
  fi
  rm -f "$out.raw" "$out.err"
}

# sandbox_scrub_status writes the snapshot check.sh compares. Fields that move on their
# own (a fresh link, a clock, how many relays happen to be connected) are dropped, so
# they cannot fail the isolation check. A real change in the status still fails it.
sandbox_scrub_status() {
  raw=$1
  out=$2
  python3 - "$raw" "$out" << 'PY'
import json, sys
raw = json.load(open(sys.argv[1]))
drop = {"link", "link_error", "next_step", "webhook", "relays_connected"}
def scrub(v):
    if isinstance(v, dict):
        return {k: scrub(x) for k, x in v.items() if k not in drop and "time" not in k.lower() and not k.endswith("_at")}
    if isinstance(v, list):
        return [scrub(x) for x in v]
    return v
json.dump({"present": True, "status": scrub(raw)}, open(sys.argv[2], "w"), indent=2, sort_keys=True)
PY
}

sandbox_compare_live() {
  before=$1
  after=$2
  python3 - "$before" "$after" << 'PY'
import json, sys
a, b = json.load(open(sys.argv[1])), json.load(open(sys.argv[2]))
if a.get("present") != b.get("present"):
    sys.exit("live Rubi presence changed")
if a.get("present") and a.get("status") != b.get("status"):
    sys.exit("live rubi_status changed")
PY
}
