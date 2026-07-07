# Console auth: local-password login → passwordless migration (ledger backfill)

- **Type:** feature + refactor — retroactive ledger entry
- **Date:** 2026-06-27 – 2026-07-04
- **Area:** `internal/api` (auth handlers, sessions), `internal/store` (users schema)
- **Commits:**
  - `af14f02` feat(api): local-password authentication backend — login/logout/change-password on `op.console`; HttpOnly+Secure+SameSite=Lax host-only server-side sessions (SHA-256, 12h TTL); anti-enumeration uniform bcrypt; JSON-only credential writes (415 otherwise); fails closed unless `local_auth_enabled`
  - `0c1cc59` feat(auth): migrate console login to passwordless
  - `3b43f05` refactor(api): drop the dead login concurrency limiter and reconcile passwordless comments
  - `c20b12c` refactor(api): drop the dead password-era `ResetMailer`, reconcile passkey-unbind docs
- **Tasks:** #27 (B1 thin thread), #79/#80/#81 (residue sweep + primitive adjudication)

## What it did

Shipped the staff local-password door (`af14f02`) as the primary web login when
Zero Trust is not in front of the API, then migrated the console to passwordless
(`0c1cc59`) once email-OTP + passkey were the intended factors. The two refactors
(`3b43f05`, `c20b12c`) then swept the password-era residue — the now-dead login
concurrency limiter and the `ResetMailer` — so no unused password machinery lingered in
the compile path, and reconciled the stale comments that referenced it.

## Why

`op.console` needs a real login even in deployments without a Cloudflare-Access edge; the
password backend was that. Once the passwordless factors landed, keeping the old password
scaffolding around was a bug farm — the sweep is the closeout evidence that the migration
was complete, not half-done.

> **Backfill note.** Reconstructed 2026-07-07 from the commit history. `af14f02` was
> covered by Go unit tests (content-type guard, anti-enumeration, forced-change lockdown)
> at its commit. Not independently re-verified for this doc; current tree green at
> `9911b8c` (WSL oracle, go1.26.4).
