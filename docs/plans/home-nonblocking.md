# B1: Non-blocking session registry and socket (Rubi Home)

Task from Цитри (audit). Goal: a hung disk or a slow file read never blocks Claude Code hooks or Bash
approvals. Branch `citri/home-nonblocking`, not pushed.

## Findings (internal/home, internal/homeproto)
- `sessionRegistry.list` scans (readDesktopStore, readTranscripts, git waits up to 2 s) while holding
  `r.mu`; every hook's `touch` needs `r.mu`. Stop even calls `touch` while holding `c.mu`
  (inbox.go ~151), so a stuck scan also blocks `Ask`, `Decide`, `connected`, `next`.
- One 32-slot semaphore in `Serve` covers event hooks and PermissionRequest; the slot is taken before
  the line is read; at capacity every connection, events included, gets a deny.
- `within` (claude.go) and `homeproto.handle` leave one goroutine per timed-out call; nothing caps them.
- Stop reads the transcript (`lastAnswer`, up to 512 KB, EvalSymlinks) on the hook path, unbounded.

## Design
1. **Two locks in the registry.** `liveMu` guards only `live` (hook-reported sessions): `touch` and
   `transcriptOf` take it for a few map operations, nothing else. `mu` guards the scan cache, git cache,
   `reading`, and scan state. No path holds both at once.
2. **Scan off the lock, single-flight.** `list` takes `mu` briefly: if the cache is stale and no scan
   runs, it starts one goroutine (`scanning` flag + `scanDone` channel). The scan reads the desktop store
   and transcripts with no lock held, gets git info through a lock-free-wait variant of the git budget
   (lock only to check/store the cache), then takes `mu` to store the result. `list` returns the cached
   result at once when there is one (stale is fine while a refresh runs); with no cache yet it waits for
   the running scan up to `scanWait` (bounded), then answers with the hook-reported sessions only.
3. **Bounded reads.** The scan has a deadline (`sessionScanBudget`, same idea as `gitScanBudget`):
   readDesktopStore/readTranscripts check it between files and stop early with what they have. A single
   read that never returns only holds the one scan goroutine (single-flight); later lists serve the
   cache. Seams: registry fields `readStore`/`readTx` (like `readGit`) so tests can block them.
4. **Hooks never wait on the scan.** Stop's `touch` moves out of the `c.mu` section. `lastAnswer` runs
   with a time limit and at most a few reads at once; past the limit, Stop goes on without the answer.
5. **Separate admission for approvals and events.** `Serve` reads the first line under a general
   connection cap with the read deadline, then classifies: PermissionRequest takes a slot of `permSem`
   (deny when full, as today); events take `eventSem` (when full, answer an empty HookReply, never a
   deny). Event handling gets its own deadline so a slot is always released.
6. **Capped leaks.** `within` becomes single-flight per key: while a call for the key still runs, new
   callers wait on the same result (bounded by their own timeout) instead of starting another goroutine.
   `homeproto.handle`: at most one handler per op may still run past its deadline; a new call of that op
   while one is stuck answers "still busy" at once.

## Acceptance (tests)
- With `reg.mu` held, `Hook()` (every event) returns in < 1 s.
- With a readDesktopStore that blocks forever, `Hook()` returns in < 1 s and a PermissionRequest sent
  through the socket still reaches `Ask` (appears in pending).
- 100 timed-out lists add ≤ 1 goroutine.
- Event hooks filling their slots don't make a PermissionRequest deny.
- `go test -race ./internal/home/... ./internal/homeproto/...` green; existing tests keep passing.

## Status
- [x] implement
- [x] tests (macOS and Linux, -race)
- [x] review (reviewer + grok-reviewer), fixes, re-review, commit

## Review follow-ups (done)
- Late Hook no longer drains after the socket answered (eventTicket commit/abandon).
- A timed-out transcript read keeps the owed reply for the next Stop.
- A hung scan is replaced after 3x the scan budget (generation counter, at most 3 left behind).
- within, scan, git reads, event hooks and homeproto handlers recover from panics.
- homeproto: per-op running/stuck state in one critical section, at most 8 handlers per op.
- Reading-gate overflow answers `{}` (approvals still fail closed); first line within 3 s.
- One list flight per includeArchived; repo filter applied per caller; distinct busy error.
- gitInfo waits on a read already in flight.
