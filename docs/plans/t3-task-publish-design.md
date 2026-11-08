# Durable task publication

## Grounding
Home owns the enrolled project, provider shims and task copies. Its thread JSON
persists T3 run IDs but previously had no task terminal state. Native T3 receipts
make launch/send retries idempotent. Core owns encrypted account secrets in its
vault (the RubiPod deployment uses this same Core), and Home RPC is end-to-end
encrypted. Providers must never acquire publishing credentials.

## Two shapes considered
1. Export task packs to Core and publish there. This moves large Git storage over
   JSON RPC and requires a new Git installation/pure-Go implementation in Core.
2. Freeze a private, validated bare snapshot in the trusted Home supervisor,
   resolve a signed target's vault token in Core at publish time, and deliver it
   only over encrypted Home RPC to a credential FD for trusted Git. Chosen: keeps
   Git where the repository already lives, with no agent credentials or new T3
   transport. HTTP/Git error bodies are never surfaced.

## Boundary and caller usage
A confirmed task start may contain a target (repository, base, credential account)
and a stable message ID. Core signs that exact device/request/project/role/
requester/target tuple using the vault key. Home persists the target and proof;
Core verifies it against an owned task inspection before resolving the fixed
`t3agents` account's `github_token` secret. A plugin cannot replace the target or
send a raw credential. The official plugin action/card is the next slice.

Task state is an atomic, file-locked journal outside all writable provider roots.
Launch/send intent precedes native RPC; replay uses the same native receipt ID.
A completed turn waits for explicit task finalization rather than guessing that
an assistant's final answer authorizes publication. Stop/failure are immutable
terminal states. Publication is serialized with mutation/stop, uses a fixed task
refspec and a private bare snapshot, and records pushed/PR progress for retry.
No project config, hooks, alternates, symlinks or worktree-common Git database is
executed by the host publisher. A changed/default/main destination is refused.
GitHub reconciliation after a lost POST response reuses the exact existing PR;
there is no merge call. Terminal notification IDs are stable; transport delivery
is at-least-once and the next UI slice must deduplicate those IDs.
