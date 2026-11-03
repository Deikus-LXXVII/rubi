# Batch approvals for the core flows (project rule 5)

Branch `citri/batch-approvals`. Goal: close the core rows of the "Known gaps (rule 5)" table in
`docs/project-rules.md`, one at a time. The plugin rows (`rubi-t3agents`) are a later stage and are not touched.

The engine already supports batches for any caller (`approvals.Request.Items`; `Execute` receives
`items:<key>,...`, or `<option>|items:<key>,...` when the request has several options). The pattern each stage
follows: prepare everything (download, verify, check) before asking, put one `Item` per thing with its own preview,
and in `Execute` do only the chosen items, each on its own, so that a failure of one is reported by item and does not
hide the others. A list of one stays the ordinary single approval, so existing callers and the older panel keep
working.

## B1: store (`rubi.plugin.install`, `.remove`, `.rollback`)

Status: done.

What changes:
- `RequestPluginInstalls(refs)`: store ids or source URLs. One approval, an item per plugin. Each item's preview is
  the single approval's (`permissions()`: publisher and key fingerprint, review line, `warning` for an unreviewed
  one, capabilities, version), and an unreviewed plugin's label ends in "(not reviewed)". The batch also warns at the
  top when any item is not from the store. The install choice (notify about updates or not) stays the approval's
  option and applies to every installed item.
- `RequestPluginRemoves(ids)` and `RequestPluginRollbacks(ids)` share `requestOps` (one approval, an item per
  plugin). Rollback of several makes sense: the case is one "Update everything" that broke more than one plugin, and
  each item shows its own current and target version; a plugin with no earlier version is reported, not offered.
- MCP: `rubi_plugin_install` takes `plugins: [...]` besides `plugin`; `rubi_plugin_remove` and `rubi_plugin_rollback`
  take `ids: [...]` besides `id`. `plugin` and `id` became optional in the schema (a loosening: every old call is
  still valid). Panel ops `plugin.install` (`plugins`), `plugin.remove` and `plugin.rollback` (`ids`) take the same.
- A ref that can't be fetched or verified, or an id that isn't installed, is reported under `failed` and does not
  hold back the others; with nothing left the answer is `nothing_to_install` / `nothing_to_remove` /
  `nothing_to_roll_back`.
- Panel: the approval screen renders items already. Two fixes: the "notify about updates" switch of the install
  screen was ignored in a batch (the items sync overwrote it), and a long item (a plugin's permissions) now wraps
  inside the card at 375 px (`.batch-item > div`).
- A latent bug found on the way: a download cleans the staging folder of anything not registered as waiting, and a
  candidate was registered only after the whole loop, so with two plugins the second download deleted the first
  one's files (this hit "update several plugins" too, never seen because the old tests had one plugin). A candidate
  is now registered the moment it is downloaded (`stage`).

Security notes:
- Nothing is installed, removed or switched before the user's proof; the proof signs the chosen items
  (`items:<ids>` is part of the signed option), the engine refuses keys that aren't in the request, and `Execute`
  walks the chosen keys only (a plugin that was not ticked is never touched, and its staged files are deleted).
- Each item is verified at request time exactly like a single install (signature, reviewed digest, pinned key); the
  tree hash is checked again when it is committed.
- An unreviewed or unofficially signed plugin keeps `review`, `warning` and its source in its own item, and its label
  says so; the batch's own preview repeats the warning.
- Two refs that resolve to one plugin id are one item (the second is reported).
- At most 20 plugins per request.

Tests: `internal/mcpserver/batchstore_test.go`: `TestSeveralPluginsInstallWithOneApproval`,
`TestInstallOnlyInstallsTheTickedItems`, `TestRefusedBatchInstallsNothing`, `TestOneFailingPluginDoesNotHideTheOthers`,
`TestSinglePluginInstallStaysTheOrdinaryApproval`, `TestSeveralPluginsRemoveWithOneApproval`,
`TestSeveralPluginsRollBackWithOneApproval`, `TestBatchOptionRejectsUnknownItems`,
`TestPanelAsksForSeveralPluginsAtOnce`. The test registry (`plugintest`) can now publish the demo plugin under other
ids (`PublishAs`, `ReviewAs`, `SourceOf`).

## B2: "Update everything" (`rubi.update`, `rubi.plugin.update`)

Status: done.

What changes: `RequestUpdateAll` (MCP `rubi_update` with `everything: true`; panel op `updates.all`; the Updates tab
shows **Update everything (Rubi and N plugins)** when both have updates). One approval of kind `rubi.update.all`;
items: `rubi` and `plugin:<id>` for each plugin. With only Rubi or only plugins to update it falls back to the
ordinary approval of that kind, so nothing changes for them. Repeating the ask returns the same link; a waiting
plugin-update approval is cancelled once the combined one exists.

Order when approved: plugins first, then Rubi. Why:
1. Rubi's step ends with `armRestart` and the daemon stops 1.5 s later. Anything not done by then is lost: the
   verified plugin releases are staged in this process. Plugins first means every plugin is applied before the
   restart is armed.
2. The Rubi step is the one that can fail for outside reasons (download, signature, smoke test). Done last, its
   failure cannot undo or hold back the plugin updates, and then no restart is armed (`rubi_failed` in the result).
3. Plugins that were released for a newer Rubi are not offered (the catalog picks versions the running Rubi
   accepts), so updating them under the old Rubi is safe; the restart then starts the new Rubi with them.
The restart is armed once, by Rubi's own step (`applyUpdate`), and not at all when the Rubi item is unticked. The
agent is not told about a combined approval that armed the restart (the new process reports `update.completed`),
exactly as for `rubi.update` (`restartsForUpdate`).

Other changes: `Core.InstallRelease` is a seam for tests (nil in the daemon: download, verify, replace the
executable), so that a test never replaces the test binary. `plugintest.Registry.PublishRubi` serves the release
feed and signed checksums.

Security notes: same as B1 for the plugin items; Rubi's item shows the verified release signature as `rubi.update`
does. The Rubi release is verified (signature of the checksums) before the question is asked and again when
downloaded. The policy level is Strong, as every `rubi.*` kind.

Tests: `internal/mcpserver/batchupdate_test.go`: `TestUpdateEverythingIsOneApproval` (one approval, plugins updated
before Rubi installs, one restart armed and one stop), `TestUpdateEverythingWithoutRubi`,
`TestUpdateEverythingWithoutPlugins`, `TestUpdateEverythingKeepsPluginsWhenRubiFails`,
`TestUpdateEverythingWithOnlyOneKindOfUpdate`.

## B3: skill library withdraw (`rubi.library.withdraw`)

Status: done.

What changes: `RequestLibraryWithdraws(refs)`; panel op `library.withdraw` takes `skills: [{id, version}]` besides
`id`/`version`. One approval, an item per skill (key `id`, or `id@version`); a skill that can't be withdrawn is
reported under `failed`. The library screen has a tick box per skill and a **Withdraw N selected skills** button. The
Bots' own `rubi_library_withdraw` is unchanged: it is the curator's direct power and asks nobody.

Security notes: the user's withdrawal only ever removes skills from use (it can't admit or edit anything). Per item
the preview lists the versions that would be withdrawn. `Execute` withdraws the chosen items only and pushes the
revocation to Rubi Home once.

Tests: `internal/core/librarybatch_test.go`: `TestSeveralSkillsAreWithdrawnWithOneApproval`,
`TestOneSkillThatCantBeWithdrawnDoesNotHideTheOthers`, `TestOneSkillWithdrawalIsTheOrdinaryApproval`;
`internal/panelapi/library_test.go`: `TestPanelLibraryWithdrawsSeveralSkillsWithOneApproval`.

## B4: settings (`rubi.settings`)

Status: skipped, on purpose.

There is no natural "several changes in one visit" in the panel today:
- `policy.set` already takes every level in one call (one approval).
- A plugin's settings (`config.go`) are one form, one approval.
- Adding a Bot already takes its webhook and the sources it hears about together (`AddAgent(name, url, key,
  sources)`), and a Bot's own asks are folded by `botBundle`.
- The rest (watchdog, transport, Rubi Home devices, the Hue Bridge, the hook) are separate screens that each do one
  thing a user does once.
A generic "save everything" would need a draft layer across screens, a UI we don't have, to save one approval in a case
that rarely happens. Not worth it now. Revisit if the panel gets a page that edits several of these at once.

## B5: Bot access (`rubi.access` and `rubi.bot.request`)

Status: analysis only; left for the owner, because folding them changes security semantics.

The two paths look alike but give different things:

| | `rubi.access` (`RequestAccess`, tool `rubi_access`) | `rubi.bot.request` (`ProposeBot`, tools `rubi_access_request`, `rubi_agent_connect`) |
| --- | --- | --- |
| What the Bot gets | A time-limited access code (the Bot asks for up to 120 minutes) for a task it describes; only used while two or more Bots share Rubi | A permanent assignment: the Bot is added to the account's `Agents` list ("for good") |
| Who chooses how long | The user, on the approval: this task, a day, a week, always | Nobody: there is one option, and the result is always permanent |
| Accounts already assigned | Granted at once with no approval (a code) | Reported as `already_allowed` |
| Pending asks | Every call is its own approval | One pending request per Bot; a new ask cancels the old approval and merges its items |
| What Rubi sends | An `access.granted` event with the code, to that Bot's webhook | Assigned-account codes after approval, plus the webhook test |

Making every access ask go through `botBundle` would therefore mean one of two things. Either a one-off "read this
mailbox for my task" becomes a permanent assignment (a Bot gets more, and for longer, than it asked for and than the
user chose today), or the Bot-request approval gains the four duration options and the task text, which is a new
approval design (the user would then pick a duration for the webhook item too, where none applies). Either way it
changes what a Bot can get, for how long, and what the user is asked, and it touches the access-code issuing and its
throttle. That is the owner's decision. What would be contained once decided: a `duration` on `botItem` for access
items, the existing `Question`/`Options` plus `<option>|items:` mechanism (already used by `rubi.access` itself) to
carry the choice, and `issueAccess` for the code. "One approval for several Bots' access" is a further step on top: it
needs a request that spans Bots, while both paths key their pending state per Bot.

## Review fixes

Status: done. Each has a test that fails without it; M1, M1b, m1, m4 and the engine rule were break-checked.

- **M1, stale plugin update.** `applyPluginUpdate` refuses unless the installed record is still at `u.from` and the new
  version is newer ("changed since you were asked; nothing was done"). Check and commit are one step (`applyMu`). A
  stale item of "update everything" fails by itself; the other items still apply, and the rollback target is not
  touched. Rule for the shared slot: a plugin update and the combined approval share one slot, and the one asked for
  last is the one that waits (the earlier is cancelled). Tests: `TestStaleOrRepeatedPluginUpdateIsRefused`,
  `TestStaleItemOfUpdateEverythingIsRefusedAndTheOthersApply`, `TestPluginUpdateAndUpdateEverythingShareOneSlot`
  (`internal/core/batchfixes_test.go`). Because of the one slot, a stale combined approval is reached by another
  route in the test (the plugin is removed and installed again meanwhile), and the "same version twice" case is
  tested on `applyPluginUpdate` directly.
- **M1b.** `pluginInstall.apply` refuses when the plugin is already installed at commit. `TestSecondInstallOfTheSamePluginIsRefused`
  (record, files and an account stay).
- **m1.** `applyUpdate` runs one at a time and refuses when the target is not newer than the running version or a
  restart for an update is already armed; a transport reason alone does not block (reasons add up). The new binary
  is therefore never installed twice, so `.prev` stays the old one. Tests: `TestOnlyOneRubiUpdateInstalls`,
  `TestUpdateToAnOlderOrSameVersionIsRefused`, `TestATransportRestartDoesNotBlockAnUpdate`. The test seam replaces
  the installer, so `.prev` itself is not exercised; the guard that protects it is.
- **m2.** Plugins beyond the 20 of one approval are counted: the combined preview and the answer say "N more
  plugins ... left out", the result carries `left_out`, and the Updates tab says "Rubi and the first 20 plugins"
  when there are more. `TestUpdateEverythingSaysHowManyPluginsWereLeftOut` (the limit is a variable the test lowers).
- **m3.** `failed` of a library withdrawal is keyed by the item key (`id` or `id@version`); a version ref next to
  a whole-skill ref of the same skill is folded into it and listed under `covered`; the whole-skill item says it
  covers "every version, including any admitted before you approve".
- **m4.** Downloads are serialized from the cleaning of the staging folder to the registration of the result
  (`dlMu`), and a result is registered at once. `TestOverlappingDownloadsKeepTheirFiles` uses a registry that
  serves the archive slowly (`SlowDownloads`); without the lock the first request fails with a checksum mismatch.
- **m5.** The old cleaner gave up after a fixed 20 minutes, 5 minutes past the 15-minute approval time, which an
  execution longer than that would outlast. It now waits for the approval to end and stops only at its expiry
  plus an hour (`stagingDeadline`). `TestStagingLivesAsLongAsTheApproval` checks the bookkeeping;
  `TestEndedBatchLeavesNoStagedFiles` (refused, cancelled, expired) checks that nothing stays.
- **Unreviewed plugins start unticked in a batch install.** The engine had no such thing, so `approvals.Item` got
  `DefaultOff` (JSON `default_off`): an option key standing for "all", a chat button and level none leave such an
  item out; it is chosen only when its key is named in `items:` (the panel names the ticked ones). If every item is
  off, "all" is an error. The panel starts those unticked. A single unreviewed install is unchanged.
  Tests: `TestDefaultOffItemsNeedToBeNamed` (engine), `TestUnreviewedPluginInABatchStartsUnticked`,
  `TestTickingTheUnreviewedItemInstallsIt`, `TestSingleUnreviewedInstallKeepsItsWarning`. `#demo=installmany` shows it.
- **Unreviewed warning in `rubi.update.all`.** A sideloaded plugin can be in it (its update is offered from its
  source, with the publisher key pinned); its item keeps the warning and review line. `TestUpdateEverythingKeepsTheUnreviewedWarning`.
  Updates stay ticked: the plugin was installed on purpose and its publisher key is pinned.

## Cross-cutting

- Panel: every new kind has demo data in `panel/js/demo.js` (`#demo=installmany`, `removemany`, `rollbackmany`,
  `updateall`, `withdrawmany`) and the approval screen renders them with the existing items UI (no new fixed widths;
  `internal/repocheck/panelcss_test.go` passes).
- `docs/project-rules.md`: the rule 5 table lost the closed rows; the tests are listed under "What the tests
  enforce".
- Break-checks (each reverted after): ignoring the chosen items in the install batch, the remove batch and the
  library batch; Rubi before the plugins; the restart armed with the Rubi item unticked; no early staging.
