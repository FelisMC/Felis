# Auto-update subsystem: decision core + sources + gatherer + window API (ledger backfill)

- **Type:** feature + fix — retroactive ledger entry
- **Date:** 2026-07-01 – 2026-07-05
- **Area:** `internal/updates` (pure decision core), `internal/updater` (release sources, gatherer), `internal/api` (window admin API)
- **Commits:**
  - `c01f133` feat(updates): pure I/O-free decision core — each tracked component is Pinned (Minecraft, left alone), Notify, or Scheduled (apply only inside a SysAdmin window); never force-applied, never a downgrade, never an auto-applied prerelease
  - `3673af6` feat(api): admin API for the maintenance window (`GET`/`PUT /updates/window`), stored as JSON under `platform_settings` — API + persistence only, nothing consumes it yet
  - `7464fa7` fix(updates): tag `Window` JSON so the persisted window round-trips (the obvious decode is correct by construction; a zero window fails closed to notify-only)
  - `96b3cc9` feat(updater): wire `updates.Run` to a caller with PaperMC v3 release discovery
  - `7d27640` feat(updater): GitHub Releases source, routing felis-api/k3s/cloudflared
  - `7db57b9` feat(updater): `VersionGatherer` extraction core + CLI gather seam
- **Tasks:** #38 (auto-update: Felis/k3s/components/Velocity, pin Minecraft)

## What it did

Built the auto-update spine as a pure decision core plus the release-discovery sources
(PaperMC, GitHub Releases) and the version gatherer, with a SysAdmin-set maintenance
window read/written through an admin API. Version parsing tolerates the real feeds (leading
`v`, k3s `+k3s1` suffix, calendar versions, prerelease tails) and orders by SemVer
precedence.

## Why

The red lines are `不要强制自动更新` (never force auto-update) and `能不动的就别动`
(Minecraft stays pinned). The design encodes them structurally: a component may be applied
*only* inside a window the operator explicitly set, and Minecraft is Pinned so it is never
touched. `7464fa7`'s fail-closed zero-window (decodes to notify-only, never a rogue apply)
is the safety property for the not-yet-built runner.

> **Backfill note.** Reconstructed 2026-07-07 from the commit history. The load-bearing
> invariants (pinned never changes, no downgrade, no auto-prerelease, apply-only-in-window)
> and the JSON round-trip contract were unit-tested at their commits. This subsystem is
> deliberately **report-only / integration-deferred**: the concrete Notifier/Applier,
> the `felis update` CLI + CronJob, and the current-version producing seams are declared
> but not wired (see `internal/updater/doc.go`, `openapi.yaml`). Not independently
> re-verified for this doc; current tree green at `9911b8c`.
