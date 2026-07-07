# Backup/restore data-safety mutation audit (round 2) — 7 gates pinned, 1 gap closed

- **Type:** test-quality audit + one test added (code change: `internal/api/handlers_backups_test.go`)
- **Date:** 2026-07-08
- **Area:** `internal/api` (`handlers_backups.go`), `internal/backupjob`
- **Task:** continues the #82 test-quality integrity audit onto the on-demand
  backup/restore surface, which postdates the 18-gate round-1 audit
  ([mutation audit `4626ab5`](2026-07-07-test-quality-mutation-audit.md)). The
  backup/restore endpoints (`7a7c0d5` / `f2fc57c` / `fc748d3`) were not in that pass.

## Method

Same as round 1: apply a one-line mutation to a fail-open gate in the source, run the
package tests, confirm the **specifically-named** test reddens with an *assertion*
failure (`--- FAIL: <subtest>`), then revert. A build break (`declared and not used`,
`undefined`) is not a valid verdict, so mutations are operator-flips that keep every
operand referenced (`!=`→`==`, drop a `!`, a literal→`true`, or `if false && <orig>` to
disable a gate without orphaning its variables). Airtightness: each mutation is re-run
with `-run` scoped to the intended subtest and `grep -- "--- FAIL: <subtest>"`, so a
reddening sibling can't be mistaken for the gate under test. Oracle: WSL Fedora-44,
go1.26.4.

## Fail-open gates mutation-verified (all CAUGHT at the named subtest)

| # | Gate (file:line) | What it guards | Mutation | Subtest that reddened |
|---|---|---|---|---|
| A | `handlers_backups.go:282` enqueueBackup stopped-gate | RWO double-mount / torn archive while the world is up | `!=`→`==` | `TestBackupNow/starting_server_->_409_not_stopped` |
| B | `:151` restore stopped-gate | restore Job can't mount a live world's RWO PVC | `!=`→`==` | `TestRestoreBackup/starting_server_->_409_not_stopped` |
| C | `:136` restore former-owner match | a fresh claimant resurrecting the previous owner's world | `!=`→`==` | `TestRestoreBackup/current_owner_who_is_not_former_owner_->_403` |
| D | `:116` restore cross-server guard | restoring server A's backup onto server B | `!=`→`==` | `TestRestoreBackup/restore_by_backup_id_cross-server_->_403` |
| E | `:214` backup owner-or-admin authz | a stranger backing up someone else's world | drop `!` | `TestBackupNow/non-owner_->_403,_no_backup` |
| F | `:26` list cross-user scope | a user seeing other tenants' backups | `p.IsAdmin()`→`true` | `TestListBackups/user_sees_only_own_former-owned_present_backups` |

## The gap this audit found — and closed

**Restore's owner-or-admin gate (`handlers_backups.go:85`) was not pinned by any test.**
It is the twin of gate E, but the two are *not* symmetric. Disabling it
(`if false && !a.isOwnerOrAdmin(p, rec)`, which keeps `rec` referenced so the package
still builds) reddened **nothing** — `go test ./internal/api/` stayed `ok`. The same
disable applied to backup's L214 (gate E) reddened `non-owner` immediately, proving the
technique valid and the asymmetry real.

Root cause: the former-owner gate at L136 backstops every non-owner case the suite
exercised (a stranger and a wrong-backup current owner both fail L136 *and* L85, so
L136's 403 masks a broken L85). The one case only L85 catches went untested: a
**superseded former owner** — a user who owned a server, took this backup
(`FormerOwner=them`), then released it to a *new* owner. They still pass L136 (they *are*
the former owner) but must be stopped by L85, or they could roll the new owner's live
server back onto their old world (cross-tenant clobber). The handler comment names this
the "must re-claim first" rule (`handlers_backups.go:77-79`).

**Fix (code):** added `TestRestoreBackup/former owner after release -> 403, no restore`,
the mirror of the existing L136 test. Verified both directions: green on the clean tree,
and it is the sole subtest that reddens when L85 is disabled — so it now pins the owner
gate specifically, not L136. No production code changed; `handlers_backups.go` is a pure
test addition away from where it was.

## Enumeration — covered vs. scoped (so "the gates" means all of them)

- **Fail-open data-safety gates — all pinned:** A–F above, plus L85 (now closed). 7/7.
- **Accountability, not fail-open (verified non-vacuous):** the `os_user` attribution at
  `handlers_backups.go:254` — a supplied operator name overrides the default `break-glass`
  audit actor. Mutating `u != ""`→`u == ""` reddens
  `TestInternalBackup/os_user_body_attributes_the_audit_to_the_operator`, so the
  attribution test isn't vacuous. A failure here degrades the audit actor; it grants no
  bypass, so it is out of the fail-open bucket.
- **Contract/behavioral (tested, out of mutation scope):** nil `Backuper`/`Restorer` → 503;
  a failed backup/restore → 500 **not** audited; `backup_ref` never serialized to the wire;
  the internal-face actor defaults to `break-glass`. Each has a direct test; none is a
  fail-open safety gate.
- **`internal/backupjob` (glanced, not mutated):** orchestration only — each backup gets a
  unique Job name (`BackupJobName` + random suffix) so a repeat "立即备份" tap can't collide
  with a just-finished Job still inside its TTL; `ErrAlreadyExists` is a defensive no-op;
  `Backup` returns once the Job is created (the async 202 is honest). Unit-tested against a
  fake `Jobs`; the controller-runtime `k8sjobs.go` and the `jobspec.go` Pod shape (weak SA
  with its token un-mounted, config Secret mounted for the self-recorded row, read-only
  world mount) are integration-verified per the package doc — not fail-open API gates.

## Coverage nuance (documented, not a gate failure)

The stopped-gate is `if info.Ready || info.DesiredState != DesiredStopped`. The `!=`→`==`
mutation pins the `DesiredState` operand (both A and B reddened), but `info.Ready` is not
*independently* pinned: no test sets `Ready=true` together with `DesiredState=Stopped` —
the stopping-but-still-up race. Low risk because in practice `Ready` drops as
`DesiredState` leaves `Stopped`, but the belt-and-suspenders `Ready` operand rides on
coverage of the operand beside it rather than its own case.

## Verdict

The backup/restore data-safety surface is a coherent unit, and this closes it: **7/7
fail-open gates pinned** (6 pre-existing, 1 added this round), one accountability gate
shown non-vacuous, one coverage edge documented. Not extended to every handler — that
would be an unbounded "continue the audit."
