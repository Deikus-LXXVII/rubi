# Disposable T3 Agents acceptance

Run on Linux with user namespaces enabled. No installed Rubi service/settings,
provider login, GitHub credential or real remote is used. Do not point the
fixture at a live Home or T3 data directory.

```bash
RUBI_REAL_T3=/absolute/path/to/unmodified/pinned/t3 \
RUBI_T3_COMPANION_DIR=/absolute/path/to/rubi-t3agents \
  scripts/test-t3-agents-acceptance.sh
```

Prerequisites: Go, git, Node, srt 0.0.81, bwrap, socat, ripgrep, Python 3,
util-linux unshare/setpriv. Use bundled/pinned runtime dependencies. Missing
platform, executable or usable user namespaces produces an explicit SKIP.
The script builds the current helper into a disposable directory.

The tagged test builds the real companion with a generated **test public key**,
installs it through Core's real plugin store/runner and calls the MCP dispatcher.
It pairs disposable Home via an encrypted local relay and pairs an unmodified
native T3 server. The fake Codex app-server edits/commits inside actual srt/bwrap,
probes only generated other-project/HOME fixtures and denied network access.
It verifies bare-remote exact refs, mocked GitHub PRs, actual plugin panel data
and requester HTTP delivery, append, confirmed stop, missing-receipt cancellation,
provider failure and recovery after reconstructing Home with the same state.

Fakes: verified phone/passkey gesture; generated publisher trust; provider/model;
loopback Nostr relay; GitHub API and GitHub credential; publication destination.
Real: Core approval/dispatcher/vault, companion subprocess, encrypted Home RPC,
native T3, helper/leases, srt/bwrap, git copy/branch/commit/push, finalization,
plugin card and requester delivery. Daemon restart is an in-process crash/reopen
simulation with the independent native server left running, not SIGKILL of a
released executable. It does not prove real provider authentication or browser
WebAuthn/panel HTTP ingress.

Local publication and native bootstrap hooks require `rubi_acceptance`; release
builds cannot configure a local publication API/remote or invoke those hooks.
Default tests remain account-free and platform portable. Linux CI can run the
script with pinned native T3 and companion source supplied; unsupported runners
skip cleanly rather than weakening the sandbox.


## Combined cleanup acceptance

The native test was rerun with VM startup/backend/image embedding removed and the companion's native setup correction applied. All five scenarios passed on the box: publish/card/requester, append and confirmed stop, provider failure, Home restart/recovery and publication, and stop after a masked native admission response before the thread ID is available. A second subscribed Bot received no task results; card text contained no fixture credential. This is an RPC/panel-data acceptance run, not a phone browser or real-provider-account test.

The disposable combined proof applies the independent VM-cleanup slice alongside the acceptance slice, retaining the native feature-gate regression and discard-audit shutdown fix. Overlapping platform-test/changelog changes are reconciled without altering product behavior. The resulting Home binary has no dependency on the retired VM packages, the embedded SDK runner image or legacy wizard test fixtures. Legacy serializers/protocol fixtures remain for settings compatibility only.
