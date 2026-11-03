# Project rules

Five rules set by the owner. They outrank convenience for the developer, and each one is held by a check that fails
the build, except rule 4, which a person reviews. A change that needs to bend a rule says so in its pull request and
in the allow-list of the check (with a task reference); it does not skip the check.

The pull request checklist is `.github/pull_request_template.md`.

| # | Rule | Enforced by |
| - | ---- | ----------- |
| 1 | Rubi is fully usable from a phone: approvals, sign-ins (for example into T3 Code) and everything else. The design also supports desktop proportions. | `internal/repocheck/panelcss_test.go`, `internal/repocheck/panelflows_test.go`, manual `scripts/panel-viewport-check.md` |
| 2 | The user is never made to use a terminal on their devices, neither to use nor to set up Rubi. | `internal/repocheck/usertext_test.go` with `testdata/usertext_allow.txt` |
| 3 | Uncompromising security and privacy of user data. | the guard tests listed below, `internal/repocheck/secretlog_test.go`, `internal/core/secretlog_test.go`, `internal/audit/audit_test.go` |
| 4 | An intuitive, convenient UI beats visuals. | review only (checklist line for the reviewer) |
| 5 | The minimum possible number of user actions: a batch of requests is approved once, not five times. | `internal/core/botbatch_test.go`, `TestBotRequestsMergeAndPartialApproval`; "Known gaps" below |

Every check below was break-checked: the rule was broken on purpose, the test failed, the change was reverted.

## Rule 1: phone first, desktop supported

How it is applied:

- The panel is one static page (`panel/`), mobile first, with a 20px page padding; wide layouts are added in
  `@media (min-width: ...)` blocks only (900px and 1000px today).
- Every flow that needs the user is a panel screen reachable from a link the user receives in chat: pair, unlock,
  settings, `setup:<plugin>`, `approve:<id>`, `agent:<name>`. Plugins add their own pages through the page
  description (`view`), which the panel draws, so a sign-in step of a plugin (a link to open and a code to copy) is a
  page section and needs no terminal.

What the tests enforce:

| Check | File and test | What fails it |
| ----- | ------------- | ------------- |
| V1 viewport | `panelcss_test.go` `TestPanelPagesAreForPhones` | a page without `width=device-width, initial-scale=1`, or one that turns zoom off; a `<table>` |
| W1-W3, N1 | `panelcss_test.go` `TestPanelStylesFitAPhone` (the checker's own cases: `TestPanelCSSCheckerHasTeeth`) | `width`/`min-width`/`flex-basis` over 360px outside a wide `@media (min-width: N)`; fixed grid tracks over 360px; `100vw`; `white-space: nowrap` or `pre` without overflow handling in the rule (the few allowed selectors are in `nowrapOK`, each with its reason) |
| J1, T1 | `panelcss_test.go` `TestPanelScriptsKeepToAPhone` | a script that sets a width over 360px in an inline style, or builds a table |
| F1 | `panelflows_test.go` `TestEveryPanelLinkHasARoute` | a link purpose that `Core.validPurpose` accepts, but `panel/js/app.js` `main()` does not route |
| F2 | `panelflows_test.go` `TestApprovalScreenCollectsEverythingAnApprovalAsks` | a field of `approvals.Detail`, `Snapshot` or `Input` that the panel never reads (a new thing an approval can ask for, with no screen for it); a level or a proof (passkey, password) the panel does not know; an approval RPC the panel never calls |
| F3 | `panelflows_test.go` `TestPanelKnowsOnlyKindsThatExist` | an approval kind named in the panel that no code in `internal/core` raises (a screen left behind) |
| F4 | `panelflows_test.go` `TestEveryPanelRPCHasACaller` | an RPC the panel server answers that no panel script calls (`unused` lists the exceptions, with reasons) |
| F5 | `panelflows_test.go` `TestPanelDrawsEverythingAPluginPageCanSay` | a part of a plugin page or a type of setup field that the SDK documents and the panel does not draw |

The rule of thumb behind the numbers: 360px is the narrowest phone supported; a fixed width above it is allowed only
where a wide-screen `@media` block says the screen is wider.

Not automated (there is no headless browser in the dependencies): how it looks. `scripts/panel-viewport-check.md` is
the manual check at 375x812 and 1280x800; run it before a release and after changes to `panel/`.

## Rule 2: no terminal

How it is applied: setup and use go through the panel and the agent's chat. The agent runs commands; the user never
types one. Home helpers that need administrator rights use the operating system's dialog.

What the test enforces: `internal/repocheck/usertext_test.go` `TestNoTextTellsTheUserToUseATerminal` reads

- every string literal with a space in the non-test Go sources (next steps, instructions, hints, error texts, log
  lines),
- every string in `panel/js/*.js` (comments dropped),
- every line of `docs/user/*.md`,

and refuses `sudo`, "run `...`", "Terminal", "command prompt", `openssl`, shell prompts, `curl -...`, `| sh`,
package-manager installs, `launchctl`/`systemctl`/`chmod`, and `rubi <subcommand>` or `rubi-home <subcommand>` (the
subcommand names are read from `cmd/rubi/main.go` and `cmd/rubi-home/main.go`). `TestUserTextPatterns` holds the
patterns to examples.

Existing violations are in `internal/repocheck/testdata/usertext_allow.txt` (file, fragment, task). A new violation
fails, and so does an allow-list line that matches nothing any more, so the list only shrinks.

Exempt by path, with the reason in `userTextExempt` in the test:

- `docs/ops/`: maintainer docs (hosting the gateway, releasing, signing plugins with openssl). Their readers are
  the people who run Rubi's infrastructure, not its users.
- `cmd/t3-bump/`, `internal/t3bump/`, `cmd/rubi-gateway/`, `cmd/rubi-announce/`: tools for maintainers and CI.
- `internal/home/localsend.go` and `internal/home/agent/t3vm/client.go`: command lines run by a Claude session and by
  T3, never typed by a person.
- `sdk/skillscan/`: the scanner names `sudo` in order to flag it.
- A program's own usage lines and `rubi-home <command>: ...` prefixes, and statements such as "rubi daemon is already
  running", are not instructions and are skipped by the patterns.

### Current violations (rule 2)

| Task | Where | What |
| ---- | ----- | ---- |
| T-01 | `panel/js/app.js` (3 texts), `panel/js/demo.js`, `internal/core/devices.go`, `internal/home/home.go` (2), `cmd/rubi-home/main.go` | installing Rubi Home asks for `curl ... \| sh` in a terminal; pairing is `rubi-home pair`; the fingerprint is read with `rubi-home status` |
| T-02 | `internal/home/hookver.go`, `hookcheck.go`, `claudemcp.go`, `installhook.go`, `cmd/rubi-home/main.go` | installing and updating the root-owned Claude Code hook: `rubi-home install-hook`, `sudo mkdir -p`, "--sudo ... in this terminal" |
| T-03 | `internal/home/hue.go` | forgetting a Hue bridge: `rubi-home hue-forget` |
| T-04 | `internal/core/hooks.go`, `internal/daemon/daemon.go` | choosing the transport: `rubi transport` |
| T-05 | `internal/kvm/kvm.go` | the KVM device needs a sudo setting |
| T-06 | `docs/user/getting-started.md` | the install message holds a `curl ... \| sh` line (the Bot runs it, the user types nothing in a terminal, but the user sees and pastes a shell line) |
| T-07 | `cmd/rubi/main.go` | the `--help` text names `rubi dev-panel` and the terminal; a development tool, to keep out of what a Bot shows |

Not found in this branch: the roster pin instruction (`rubi-home roster pin`) from the pod work; when it lands, the
test fails until it is replaced by a panel step or listed here.

## Rule 3: security and privacy

How it is applied: the design is in `docs/design/security-model.md`. Secrets live in the vault (sealed), never in
logs, the audit log, approvals' results or chat; the audit log seals its details with a key derived from the vault key
and writes only the event name while Rubi is locked; the agent cannot complete a panel approval.

New checks added for the "secrets never reach logs or the audit log" part:

| File | Test | What it guards |
| ---- | ---- | -------------- |
| `internal/repocheck/secretlog_test.go` | `TestNoSecretIsHandedToALogOrTheAudit` | no `log.Print*`, `Logf` or audit `Record` call is handed a value whose expression names a secret (secret, password, DEK, bearer, `.Key`, token, pairing code, credential, seed, wrap); a call that names one without writing it goes in `secretNameOK` with the reason |
| `internal/core/secretlog_test.go` | `TestSecretsNeverReachLogsOrTheAuditLog` | a routine key (also as `Authorization: Bearer ...`), a key the user pastes, a link ticket and a Bot's library code go through the real propose, approve, deny and link paths; none is in the process log, the audit file, the texts returned to the Bot, or any file under Rubi's home in the clear |
| `internal/core/secretlog_test.go` | `TestOpenedAuditHoldsNoSecret` | the audit log opened with the vault key holds no routine key and no Bot code |
| `internal/audit/audit_test.go` | `TestRecordNeverWritesAValueInPlainText` | secret-shaped values given to `Record`, nested or too large, are never in the audit file in plain text, locked or unlocked |

### Existing security guard tests

| File | Test | What it guards |
| ---- | ---- | -------------- |
| `internal/core/homegate_test.go` | `TestHomeGateRefusesFoldedDuplicateKeys`, `TestHomeGateSSHRegisterRefusesFoldedDuplicateKeys` | strict JSON for Home ops: keys that fold together (case, the Kelvin sign, long s) cannot let core check one spec while Home reads another or smuggle `attested_kinds` |
| `internal/core/homegate_test.go`, `homegate_t3_test.go` | `TestHomeGateAttestedKinds`, `TestHomeGateRolePutApprovals`, `TestHomeGateSSHRegister(AllFields)`, `TestHomeGateProjectAdd`, `TestHomeGateSkillAcceptID`, `TestHomeGateT3*`, `TestHomeCallRunsTheGate`, `TestHomeOpMapsT3OpsToTheClaudeOnes` | the home gate (attestation): only real, user-given approvals of the right kind, from the right plugin, in the right state let a role change, an SSH target, a project or a skill through; core, not the plugin, says which kind an id is |
| `internal/home/hookver_test.go` | `TestWrongTypedStampIsRefused`, `TestProtocolMismatchIsRefused`, `TestInstallHookRefusesUnsafePaths`, `TestInstallRefusesInsecureDirectoriesAndSymlinkTargets`, `TestInspectRefusesFifoAndDevices` | the root-owned hook: a wrongly typed version stamp, a protocol mismatch, unsafe or symlinked paths and FIFOs/devices in place of the binary are refused |
| `internal/repocheck/repocheck_test.go` | `TestRetiredTokenAndGatewayNamesStayOut`, `TestRemovedFilesAreGone` | the stored subscription token and the auth gateway stay removed: a grep over the sources for the retired names, with a reasoned allow-list that only shrinks |
| `internal/repocheck/repocheck_test.go` | `TestHomeNeverOpensTheSignInFolderOrItsCredential` | AST scan: no call into package `os` names the sign-in folder or the credential file except the two functions that make the folder and stat the file |
| `internal/home/t3signin_unix_test.go` | `TestHomeNeverOpensTheCredentialFile` | a FIFO in place of the credential file: any `open()` by Home would block and fail the test |
| `internal/home/agent/auth_test.go` | `TestProjectVMsNeverMountTheSignInFolder`, `TestSignInContainerHasNoGatewayAndTheProxyAllowsOnlySignInHosts` | project VMs never see the sign-in folder; the sign-in container reaches only the sign-in hosts |
| `internal/home/setup/login_test.go` | `TestLoginURLAllowList`, `TestLoginRefusesAnUnexpectedLink`; `internal/home/t3login_test.go` `TestT3LoginRefusesAForeignLink`, `TestT3LoginRefusesALinkThatCarriesACredential`, `TestT3LoginCodeIsNeverLogged` | a sign-in link is shown only for the expected host and shape, never with a credential, and the code is never logged |
| `internal/home/agent/t3vm/env_test.go` | `TestFilterEnvKeepsOnlyTheAllowList`, `TestEnvLinesRefuseOtherProxyVariables`, `TestFilterEnvIsAListNotAPrefix` | the environment passed into a VM is an allow-list (the retired token variable and proxy variables are dropped) |
| `internal/home/agent/t3vm/argv_test.go` | `TestClampArgv*` | T3's `claude` command line is clamped: permission mode lowered to the ceiling, skip-permission flags dropped, nothing that could turn the hook off, only T3's own MCP server and tools, `--add-dir` limited to the workspace |
| `internal/t3/client_test.go`, `mode_test.go` | `TestLaunchClampsToTheCeiling`, `TestLaunchRefusesAThreadAboveTheCeiling`, `TestClamp`, `TestTransportRefusesOtherHosts`, `TestBearerNeverLeavesTheBase`, `TestClientPrintHidesTheToken`, `TestBadTokenIsRefusedWithoutLeaking` | the T3 client clamps modes, talks only to its own host, and never prints or leaks its bearer token |
| `internal/t3/oauth_test.go`, `server_unix_test.go` | `TestTokenIsNeverPrinted`, `TestPairingCodeFailureDoesNotLeak`, `TestServerRefusesAnUnrelatedRuntimePid` | the T3 token is redacted when printed, a failed pairing code never appears in the error, a runtime pid file for another process is not trusted |
| `internal/home/agent/t3vm/gate_test.go` | `TestGateDeniesAnythingButAnExplicitAllow`, `TestGateDeniesMalformedIncompleteAndUnknownInputWithoutAsking`, `TestGateDeniesWhenItHasNoWayToAsk`, `TestGateDeniesWhenTheDecisionTakesTooLong`, `TestGateServesAskOnlyByPost` | the permission hook fails closed: anything but an explicit allow, a malformed input, no way to ask and a timeout all deny |
| `internal/home/agent/t3vm/hook_test.go` | `TestHookScriptRefusesAnAddressThatIsNotLoopbackHTTP` | the hook script talks only to a loopback HTTP address |
| `internal/home/agent/pipe_test.go` | `TestNetlessStartFailsClosedWhenTheForwarderNeverListens` | a VM without a network fails to start rather than start with one |
| `internal/home/sistertools_test.go` | `TestPeerIsFixedBeforeTheLineIsRead`, `TestPeerMapIsBounded`, `TestToolsRefusedWhenThePeerIsUnknown`, `TestHookLinesAttestOverTheSocket`, `TestRealPeerPID` | peer checks: the calling process is read from the socket right after accept and not followed if it changes; tools refuse an unknown peer |
| `internal/approvals/approvals_test.go` | `TestExpiryFollowsTheWallClock` | approval expiry uses the wall clock (no monotonic reading), so an approval cannot outlive its expiry across a sleep |
| `internal/panelapi/panelapi_test.go` | `TestReplayAndStaleRequestsRejected` | replayed or stale panel requests are refused |
| `internal/home/agent/proxy_test.go` | `TestProxyRequiresAuthAndRefusesWildcard`, `TestProxyRefusesNonPublicAddresses`, `TestGuardedDialerRefusesLoopback`, `TestAllowListNeverHasRefusedHosts` | the egress proxy needs authentication, has no wildcard, and refuses private and loopback addresses |
| `internal/home/agent/ssh_test.go`, `sshtarget/hub_test.go` | `TestSSHPrivateKeyNeverAppearsAnywhere`, `TestPrivateKeyNeverLeaves`, `TestRedactBearer`, `TestSSHHostKeyChangeIsRefusedAndAudited`, `TestOnlyAllowedCommandsRun`, `TestRefusedReuseLeavesNoKeyFile` | SSH targets: the private key never leaves, commands are redacted and limited to the allowed ones, a changed host key is refused and audited |
| `internal/home/setup/ops_test.go` | `TestProgressNeverCarriesSecretsEvenFromCommands`, `TestImageJobFailureShowsTheLastLinesWithoutSecrets`, `TestRunAllNeverRunsThePrivilegedSteps` | setup progress shows no secrets, and the privileged steps never run unasked |
| `internal/home/agent/secrets_test.go` | `TestLooksSecret`, `TestSpawnStderr` | lines that look secret are filtered from what Home hands to the plugin |
| `internal/core/botpaste_test.go`, `keyfile_test.go`, `botrequest_test.go` | `TestBotKeyFileReferenceRefused`, `TestBotMayProposeOnlyRoutineWebhooks`, `TestProposeRefusesRedactedKey`, `TestBotRequestDenyAndLockDiscardTheKey`, `noKeyIn` | a Bot's webhook key is taken only for a routine address, is never on disk in the clear, and is discarded on deny, expiry and lock |
| `internal/plugins/plugins_test.go`, `internal/library/library_test.go`, `internal/update/update_test.go`, `internal/mcpserver/fullstack_test.go` | `TestExtractRefusesUnsafeEntries`, `TestSignatureTokenChecks`, `TestRejectIsFinal`, `TestDownloadRejectsTampering`, `TestMarketplaceRefusesUnreviewedOrForged` | signed plugins, skills and releases: unsafe archive entries, bad signatures, tampered downloads and forged marketplace entries are refused |
| `sdk/skillscan/scan_test.go` | `TestExcerptsMaskSecrets`, `TestHostAllowList`, `TestNormalizedNeverHasEscapes` | the skill scanner masks secrets in what it shows and flags hosts and escapes |

Not found in this branch: roster tests (the roster pin from the pod work) and a test named "R5 guard". When they
land, add them here. The T3 Agents plugin (`rubi-t3agents`) has its own wall-clock guards in
`wallclock_guard_test.go`.

## Rule 4: intuitive over pretty

Review only. The reviewer asks, for each screen or message that changed: can a first-time user find the next step
without reading anything else? Is there one obvious action? Are the words plain (no jargon, no commands)? Does any
decoration (animation, mascot, colour) cost clarity or touch targets? Visual polish is accepted only after these.

## Rule 5: fewest user actions

How it is applied: everything one agent asks for while a request of its own still waits joins that request
(`botBundle`/`botApproval` in `internal/core/botrequest.go`: the earlier approval is cancelled and its items move into
the new one), so the user approves once and can untick items. Plugin updates are one approval with an item per plugin.
The `approvals` engine supports batches for any caller (`Request.Items`; `Execute` receives `items:<key>,...`).

What the tests enforce:

| File | Test | What it guards |
| ---- | ---- | -------------- |
| `internal/core/botbatch_test.go` | `TestSeveralAsksOfOneBotAreOneApproval` | a webhook and two mailboxes asked in three calls are one waiting approval, with three items, and one proof applies all three |
| `internal/core/botrequest_test.go` | `TestBotRequestsMergeAndPartialApproval` | the later ask joins the waiting request, the earlier approval is cancelled, a subset can be approved |
| `internal/core/botpaste_test.go` | `TestBotConnectWithoutKey` | the connect request and an access request fold into one approval, with the user's paste on the same screen |

### Known gaps (rule 5)

Flows that create approvals one by one where a batch would be possible. These are candidates, not defects found by a
test; none is held by a check yet.

| Where | Flow | One by one today | Batch that would be possible |
| ----- | ---- | ---------------- | ---------------------------- |
| core `internal/core/market.go` (`rubi.plugin.install`, `.remove`, `.rollback`) | store | one approval per plugin | install or remove several plugins in one approval with an item each (updates already do this) |
| core `internal/core/update.go` (`rubi.update`) and `market.go` (`rubi.plugin.update`) | updates | Rubi's own update and the plugins' update are separate approvals | one "Update everything" approval with Rubi as one item |
| core `internal/core/library.go` (`rubi.library.withdraw`) | skill library | one approval per withdrawn skill | withdraw several skills at once |
| core `internal/core/host.go` (`rubi.settings`: policy, watch, webhook changes) | settings | one approval per change (`policy.set` already takes all levels at once) | one save for several settings in a visit |
| core `internal/core/access.go` (`rubi.access`) and `botrequest.go` (`rubi.bot.request`) | Bot access | two kinds of approval for similar asks; one per Bot | fold access into the Bot request path everywhere, and one approval for several Bots' access |
| plugin `rubi-t3agents` `agent_perm.go` (`Claude (<tool>): ...`) | Claude tool permissions | one approval per tool call; simultaneous asks of one session are separate (trust options reduce, not group, them) | group the pending asks of one session into one approval with an item each |
| plugin `rubi-t3agents` `roles_change.go` (role put / widen / foreign; skill accept) | agent roles | one approval per role change, and one per flagged skill to accept | a change with its skill acceptances as items of one approval |
| plugin `rubi-t3agents` `roles_ssh.go` (SSH target) | SSH targets | one approval per target | several targets in one approval |
| plugin `rubi-t3agents` `agent_sisters.go` (bind, start, trust project), `roles_start.go`, `project_add.go`, `agent_fork.go`, `agent_mode.go`, `sessionname.go` (rename) | sister session control | each is its own approval, and start, bind and trust commonly come one after another for one new session | "start this session and trust the project" as one approval; renames and mode changes as items |
| plugin `rubi-t3agents` `agent_setup.go` | first-time setup of the agent path | install step and project folders are separate approvals | one setup approval with the steps as items |
| plugin `rubi-t3agents`, all of the above | | no `Submit` in the plugin sets `Items`, so none of its flows is a batch | use `Request.Items` where the list above says |

## Adding to this list

A new check goes next to the rule it enforces, is break-checked (break the rule, see the test fail, restore), and is
named in this file. A new allow-list line needs a task reference and a reason; the lists only shrink.
