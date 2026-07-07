# §B3 username-collision reclaim + account migration (ledger backfill)

- **Type:** feature — retroactive ledger entry
- **Date:** 2026-06-27 – 2026-07-05
- **Area:** `internal/api` (internal-face reclaim/blacklist, account migrate), `internal/store` (migration 0006)
- **Commits:**
  - `a29571d` feat(api): reclaim squatted usernames for Mojang-priority players (§B3, 正版优先) — `POST /internal/player/reclaim` bars the squatter UUID + stashes its data (30-day hold) in one transaction, idempotent, returns the *first* reclaim's expiry; `GET /internal/player/blacklist/{mc_uuid}` is the login-gate check
  - `fdb6efb` feat(account): migrate a live account's owned servers to a new account (§B3 inherit)
- **Tasks:** #30 (B3 game-login + username-collision reclaim)

## What it did

Built the data layer of the Mojang-priority collision flow: when the configured
third-party Yggdrasil and official Mojang issue the same username under different UUIDs,
the non-genuine squatter is displaced in favour of the real Mojang owner. Both tables are
keyed by `mc_uuid`, so the genuine player — identical username, *different* UUID — is
never caught by the bar. `fdb6efb` adds the inherit half: migrating an existing account's
owned servers onto a new account.

## Why

Two players cannot hold one username across two Yggdrasils; the spec resolves it in the
genuine Mojang owner's favour with a 30-day data hold for the displaced squatter, told the
truth about how long their data is kept (the first hold's window, never a fresh `now()+30d`
on retry).

> **Backfill note.** Reconstructed 2026-07-07 from the commit history. Handlers + the
> in-memory repo contract were unit-tested at each commit; the Postgres SQL path is
> integration-only, and the velocity collision-routing / limbo prompt / authlib
> dual-backend are code-only (Java) and out of this data-layer slice. Not independently
> re-verified for this doc; current tree green at `9911b8c`. Related: the operator-facing
> `/felis migrate` command has its own doc ([felis-migrate-command](2026-07-05-felis-migrate-command.md)).
