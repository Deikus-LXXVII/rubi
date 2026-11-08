# Task control and presentation

## Work plan

- Ground: trace Home task journals, native launches, provider shims, broker and panel.
- Sketch: compare native receipt lookup with trusted launcher supervision.
- Agree: autonomous implementation, no human checkpoint requested.
- Implement: cancellation fence, lifecycle reconciliation, authenticated task actions/card.
- Scrap: remove any sketch that cannot prove a confirmed stop.

## Overview / key concepts

Core owns confirmations and use-time vault access. Home owns workspace bindings,
immutable task journals and native thread receipts. T3 owns orchestration; provider
processes must enter through Rubi's trusted shims (Cursor through its project server).
A native turn ending is not necessarily a completed task. Publishing seals the mandate.

## How it works / where things live

`t3agent.go` records intent before Launch, but a lost response can leave no thread ID.
`t3tasks.go` replays running intents and interrupts known threads. `sandbox.go`
resolves task workspaces; it previously checked no lifecycle fence. `taskpublish.go`
in Core resolves the approved GitHub account at use time. Generic plugin pages
use the authenticated panel RPC and declarative text rendering, not agent HTML.

## Candidates and decision

1. Native receipt lookup: public T3 start/send APIs support idempotent replay,
   but no read-only clientRequestId lookup. Replaying a cancelled start can create
   work after cancellation. Do not do that; do not read T3's private database.
2. Trusted launch leases: serialize admission with a per-task OS lock, inspect the
   durable phase, and hold a lease until the confined process has exited. Stop seals
   the journal first, then drains every admitted launcher. Late launches refuse.
   This works without learning a native thread ID or signalling a recycled PID.

Choose 2. Usage: `lease := store.Admit(ctx,id); defer lease.Close();
Run(lease.Context(),...)`. Stop: save terminal fence; `store.Quiesce(ctx,id)`.
Publishing: save Publishing fence; quiesce before snapshotting. Cursor holds its
lease for the whole project server process, not just the start RPC.

## Gotchas / safety

Lease records are outside all sandbox write grants. An abandoned, incomplete lease
is never treated as successful cancellation merely because its OS lock vanished.
Stop must return pending, not a false success, if a trusted supervisor crashes.
No bare PID registry, no replay after cancellation, no T3 patches.


## Execution proof and compatibility

A process-group-only guardian was rejected: detached processes can outlive the
srt Node leader, and killing bubblewrap via parent death does not by itself wait
for its PID namespace teardown. Task srt now runs as PID 1 in an outer private
PID/user namespace; Linux reaps that leader only after all namespace processes
exit. Only the existing host UID/GID are mapped, unchanged (not root: Claude and other
root-refusing CLIs must keep working). Fixed system util-linux unshare mounts
private procfs using temporary namespace-only ambient mount/setpcap capability;
setpriv clears inheritable/ambient/bounding capabilities before srt exec. This
avoids nested bwrap's wrong /proc/PID/ns lookup without changing provider identity. Missing namespace privileges/dependencies refuse.

Consequently repository tasks are Linux-only for now: macOS has no equivalently
proved descendant supervisor in this patch. Standalone macOS sandbox sessions
are unaffected. Even direct own-tree launch on another platform refuses.
The helper capability probe and Home handshake reject mixed-version installs.
A trusted guardian crash/incomplete receipt remains unconfirmed, not discarded.

Real proof: unmodified T3 nightly, srt 0.0.81, bwrap, Git and a fake account-free
Codex app-server. Both an ordinary held run and an accepted reply masked before
Home sees its thread ID are stopped; the fixture's setsid heartbeat cannot
continue after acknowledgement. Bare-remote/mock-GitHub publication and official
plugin/card/message tests are separately verified, not a combined phone e2e.
Per-task durable result receipts replace a growing plugin-state delivery map;
transport remains at-least-once across emit/ack crashes.
