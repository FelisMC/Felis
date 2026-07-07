# felis CLI: break-glass recovery console + first-run setup (ledger backfill)

- **Type:** feature + fix — retroactive ledger entry
- **Date:** 2026-06-27 – 2026-06-30
- **Area:** `cmd/felis` (break-glass/setup TUI, apply, migrate), `internal/api` (audit, owner store), `deploy/`
- **Commits:**
  - `e108a37` feat(cli): break-glass emergency console TUI — root-only (`euid==0`), provisions/resets the Owner directly against Postgres, enables local login, prints a durable one-time-password summary
  - `2d0bbb0` feat(cli): attribute break-glass recovery to the SysAdmin who runs it — bootstrap / recovery (bcrypt) / root-override, each audited with an honest `verified` flag and payload
  - `a94b001` feat(deploy): break-glass Operator account provisioning
  - `eb5875a` feat(felis): Operator break-glass op behind an operation menu
  - `f5d00f3` feat(cli): `felis apply` for direct CRD creation
  - `9c46632` feat(cli): `felis setup` first-run console (shared `runConsoleTUI` model, reclaim protection, cfsetup idempotency, `[auth].admin_hostname` respect)
  - `7d91373` fix(migrate): honor `-config` placed after the `up` verb (flag.Parse stops at the first non-flag token)
- **Tasks:** #27 (B1 login→change-pw→TUI reset)

## What it did

Built the local-root recovery and first-run surface that bypasses web Zero Trust by
design. `felis breakGlass` mints or resets the Owner when the web login is unreachable;
`2d0bbb0` makes it accountable by recording *which* SysAdmin broke the glass across three
audited modes. `felis setup` is the non-emergency first-run twin sharing the same console
model. `felis apply` writes a `MinecraftServer` CRD directly, and `7d91373` fixes the
`migrate` flag parse so a configured DB path after `up` is honored.

## Why

An operator with root on the node and a kubeconfig must always be able to recover the
platform — that is break-glass's whole job, so it never refuses. Attribution
(`2d0bbb0`) closes the gap that root is machine authority, not a human identity: the root
gate is necessary but not sufficient for the audit trail.

> **Backfill note.** Reconstructed 2026-07-07 from the commit history. The core logic was
> covered by Go unit tests over a fake owner store at each commit (auth match/non-match,
> the three audit modes, headless TUI drive). The bubbletea TUI glue is untested by house
> convention. Not independently re-verified for this doc; current tree green at `9911b8c`.
