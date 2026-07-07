# Player onboarding data layer §B2: email-OTP, account-link, QR, Bind-Code (ledger backfill)

- **Type:** feature + fix — retroactive ledger entry
- **Date:** 2026-06-27 – 2026-07-03
- **Area:** `internal/api` (onboarding/auth-bind handlers), `internal/store` (migrations 0004–0006)
- **Commits:**
  - `dbe34a1` feat(api): player email-OTP verification (§B2) — `POST /account/email/{start,verify}`; 6-digit code, SHA-256-at-rest, 10-min TTL, 5-attempt cap enforced in the repo
  - `1f8b9bb` feat(api): record account-link auth source (`mojang|thirdparty`) for the dual-Yggdrasil split (§10)
  - `116595f` feat(api): QR scan-login completion poll on the internal face (`GET /internal/account/link/status/{mc_uuid}`) — read-only, reuses `UserByMCUUID`, no migration
  - `fe2ece0` feat(api): public Bind-Code onboarding (`POST /auth/bind`) — the one pre-account entrypoint of `console.<root_domain>`; refuses a staff-UUID code with 403 without consuming it, so the public door provably never yields an admin principal
  - `55592ed` feat(auth): public auth-bind endpoint wiring
  - `6c3999a` fix(api): rate-limit email-OTP sends to close the email-bomb vector
  - `879b177` fix(api): make OTP-start throttle atomic to close the concurrent-burst bypass
- **Tasks:** #29 (B2 data layer), #32 (OTP rate-limit), #35 (atomic throttle), #39 (console access model)

## What it did

Built the Go-verifiable data layer of forced web onboarding: prove control of an email
(OTP), record which Yggdrasil authenticated an in-game UUID, let a phone already signed in
to the panel complete a QR device-code link, and let an account-less player redeem a
one-time Bind Code minted in the Login Lobby to create+link+session in one public step.
The two fixes bound the OTP abuse surface — a per-target send rate limit and an atomic
reserve that closes the check-then-act race on the attempt counter.

## Why

The spec forces onboarding through the web so every account is provably email-controlled
and UUID-linked before it can operate anything. The `op.console` redline in `fe2ece0` — a
staff-UUID code is refused without being consumed — is what keeps the public console door
from ever minting an admin principal.

> **Backfill note.** Reconstructed 2026-07-07 from the commit history. The account/session
> logic, single-use codes, and the op.console redline were covered by handler tests + the
> OpenAPI parity gate at each commit; the identity guarantee behind a Bind Code lives in
> velocity/Java (CODE-ONLY) and is not verifiable from this repo. Not independently
> re-verified for this doc; current tree green at `9911b8c`.
