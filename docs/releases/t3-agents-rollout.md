# T3 Agents: next Rubi Home rollout (draft)

This is a checklist, not an executed rollout. Never mix old VM launchers with the new host-sandbox task daemon.

## Release notes

Repository tasks ask once, create their own worktree/branch, use native T3 and the pinned srt runtime, survive daemon restarts, and publish only their own branch and a pull request. Append/stop, task cards and requester-only results are supported. No task merges a PR or writes main. Linux is the supported repository-task host. Native macOS T3 sessions remain on the existing sandbox path; repository tasks explicitly refuse on macOS/Windows. Legacy SDK/VM execution is retired rather than silently running on the host.

## Operator checklist (not performed)

- Merge the acceptance/cleanup slices and companion changes after CI; run the repeatable acceptance script on the exact combined build.
- Build/checksum/sign a Rubi Home release including matching `rubi-agent-exec`, pinned srt/Node and Linux socat/ripgrep. No release is made by these source changes.
- Linux box: verify bwrap and usable user namespaces, unshare/setpriv, git and runtime permissions; install the pinned native T3 build. Keep managed shims/policies outside all agent-writable folders.
- Stop active legacy sessions and remove old Apple/container or microVM processes/assets manually as an operator migration. This code does not touch existing VM installations or live configuration.
- Update the box Rubi daemon and Mac Rubi Home together with matching helper/runtime packaging. Preserve settings backups; verify pairing and native macOS sandbox-exec with a disposable instance before enabling sessions. Intel Macs with no upstream pinned T3 build explicitly refuse.
- Build/update the official companion plugin using the matching Home version; recheck signed plugin verification and encrypted pairing.
- Register the GitHub fine-grained account in RubiPod/vault, restricted to the intended repositories with Contents and Pull Requests permissions. Bind the account to the requester. Tokens resolve at publication time, never enter agent argv/environment, and are not copied to worktrees.
- Sign in to Claude once on the T3 server from the plugin setup page ("Sign in to Claude": open the link, paste the code). One sign-in is shared by every project; it is not entered per project, and no operator HOME profile is imported. The sign-in runs natively under srt. Verify it with a real Claude account before offering a live task.
- Register the GitHub fine-grained token on the plugin's account form in the panel (Settings → T3 Agents → Add account, or "GitHub token" on the plugin page), as the plugin secret `github_token`.
- Smoke: one confirmation, isolated edit/test/commit, exact branch push, PR link, escaped panel card, requester-only result; then append, stop before admission, failure and restart recovery. Confirm no write to main and no merge.
- Rollback: stop tasks and confirm descendants stopped before reverting binaries; retain task journals for reconciliation. Do not re-enable legacy VM execution in parallel or reuse sandbox credentials outside their project.

## Verification boundary

The box acceptance uses real companion/Core/Home/native T3, srt/bwrap, git and encrypted relay. Model CLI execution is a faithful fake, remote is a local bare repository, GitHub is a mock and requester HTTP delivery is a local receiver. It does not prove real provider login, real GitHub API authorization, a phone browser, signed release packaging or macOS sandbox-exec on hardware. See `docs/testing/t3-agents-acceptance.md` after the acceptance slice is merged.

Legacy SDK runner/protocol fixtures and settings serializers are retained for compatibility testing only; there is no production VM backend or embedded VM runner in Home. They are not a supported alternative to native sandbox sessions.
