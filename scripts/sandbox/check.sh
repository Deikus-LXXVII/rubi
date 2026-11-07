#!/bin/sh
# Non-UI sandbox check: up, the panel and catalog answer, the daemon is unpaired inside its
# own home, down, up again, reset, and a live Rubi (if one was already running) is unchanged.
set -eu
cd "$(dirname "$0")"
. ./lib.sh
DIR=$(sandbox_dir)
./up.sh
./run.sh python3 - << 'PY'
import urllib.request
urllib.request.urlopen("http://127.0.0.1:18080/", timeout=5).read(200)
urllib.request.urlopen("http://127.0.0.1:18081/marketplace/catalog.json", timeout=5).read(200)
print("http ok")
PY
python3 - "$DIR/run/status.json" << 'PY'
import json, sys
body = json.load(open(sys.argv[1]))
st = (body.get("structured") or {}).get("state")
if st != "unpaired":
    raise SystemExit(f"daemon state is {st!r}, want unpaired")
print("daemon unpaired")
PY
# The sandbox daemon's environment must not point at a live home, and it must be in another network namespace.
python3 - "$DIR" << 'PY'
import os, pathlib, sys
d = pathlib.Path(sys.argv[1])
ns = (d / "run/ns.pid").read_text().strip()
host = os.readlink("/proc/self/ns/net")
sand = os.readlink(f"/proc/{ns}/ns/net")
if host == sand:
    raise SystemExit("sandbox network namespace is the host namespace")
print("namespace separated")
PY
./down.sh
./up.sh
./down.sh
before="$DIR/run/live-before.json"
# reset deletes the directory, so compare first if a before-file exists.
if [ -f "$before" ]; then
  bin="$DIR/bin/rubi"
  sandbox_snapshot_live "$DIR/run/live-after.json" "$bin"
  sandbox_compare_live "$before" "$DIR/run/live-after.json"
  printf 'rubi-sandbox: live rubi_status unchanged\n'
fi
./reset.sh
[ ! -d "$DIR" ]
# A second reset is a no-op.
./reset.sh
printf 'rubi-sandbox: check ok\n'
