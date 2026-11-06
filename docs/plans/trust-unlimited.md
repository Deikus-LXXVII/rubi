# B11: project trust until revoked

Task from the user (approved). Branches: core `feature/trust-unlimited` (docs only), plugin `feature/trust-unlimited`
(both from `feature/project-trust`). Not pushed. Builds on B8 (`docs/plans/project-trust.md`); every B8 guard stays.

## Design (plugin only; Rubi Home and Rubi core need no change)
- A second mode of `pt_…`: `unlimited` (saved as `"unlimited": true`, `Expires` zero, `MaxUses` 0). Live until revoked.
  An older plugin that loads such a record sees a zero end and drops it (fails closed).
- Granting: (a) a fourth option on run approvals, `trust_project_until_revoked`, "Allow and trust the project
  <root> until I revoke it"; the 04:00 option stays the default (third). (b) `claude_code_trust_project` gets
  `until` = "0400" (default) | "revoked"; still strong + locked; the approval carries one option of that mode.
- Text: label and Meaning say "UNTIL REVOKED — no end time", that it stays on across restarts of Rubi Home, the
  plugin and Rubi and across days, that nothing ends it but a revoke (panel, `claude_code_mandates`) or Rubi
  being locked, and the risk (desktop sessions that skip permission prompts; the Bot may open sessions in the
  project without asking). The preview, panel and Bot messages show "until revoked", never an end time.
- Use count: still counted and each allow still audited, but **no cap** for an unlimited trust (a cap would end
  it silently; the audit and the visible count are the control). A 04:00 trust keeps 2000.
- Pruning: an unlimited trust is always live, so `pruneLocked` keeps it; the 20-trust cap evicts the oldest
  *timed* trust and refuses a new grant (nothing changes) when all 20 are unlimited. Only revoke drops one.
- Replacement: one live `pt_` per device+root; a new grant replaces the old one of either mode (audited, with
  both modes in the audit).
- Age: `claude_code_mandates` and the panel show `granted` (time) and `granted_ago` ("3 d 4 h ago") and `uses`.
- Locked: the allow is a `claude.decide` through Rubi, which the helper denies while Rubi is locked; unlimited
  changes nothing there (unit-checked by the mode being only a record; e2e Case 4c uses an unlimited trust).
- Clock seam: `sisters.now`. The rubie2e build adds a file `clock-offset` (duration) in the plugin's data dir
  that shifts `now`; release builds have no such code.

## Tests
Unit (-race): grant via option and tool, reload, valid after 25 h and 40 days, revoke, always-ask list, age,
replacement between modes, cap/prune, failed save. e2e (`e2e/_driver`): restart, +25 h/+40 d via clock offset,
revoke, lock, always-ask.
