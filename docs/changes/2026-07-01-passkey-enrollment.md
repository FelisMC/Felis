# Passkey (WebAuthn) enrollment subsystem + hardening (ledger backfill)

- **Type:** feature + fix — retroactive ledger entry
- **Date:** 2026-07-01 – 2026-07-02
- **Area:** `internal/passkey` (go-webauthn adapter), `internal/api` (enrollment handlers/audit), `internal/store` (migrations 0007–0009)
- **Commits:**
  - `f2c916d` feat(api): passkey enrollment persistence layer
  - `742f15f` feat(api): passkey enrollment endpoints
  - `0261204` feat(passkey): go-webauthn enrollment verifier adapter (Oracle-verified against a virtual authenticator)
  - `fce0fce` feat(passkey): wire the enrollment verifier into felis-api
  - `7278cd7` feat(passkey): require + record user verification at enrollment (`UserVerification=required`; capture `user_verified`/`backup_eligible`/`backup_state` — migration 0009) — *fix (d)*
  - `cdbb5ab` fix(api): record credential id in the passkey-register audit event so bind/unbind are symmetric — *fix (a)*
  - `9953275` fix(api): bound `webauthn_challenges` growth by superseding *all* prior rows per (user, purpose) — *fix (b)*
  - `20e31fb` fix(store): cascade-delete passkeys + challenges on user removal (recreate both FKs `ON DELETE CASCADE`, scoped to the passkey tables only) — *fix (c)*
  - `54bc6ef` fix(api): clear bound passkeys on password change to close a takeover foothold — *fix (e)*
- **Tasks:** #36 (passkey bind with email-OTP fallback), #48–#52 (fixes a–e)

## What it did

Built the WebAuthn *enrollment* half — persistence, the go-webauthn crypto adapter, and
the register-begin/finish endpoints — then hardened it through the five-fix batch (a–e):
symmetric audit, a bounded challenge table, cascade cleanup, enforced+recorded user
verification, and unbinding every passkey on a password reset so a passkey planted through
a transiently-hijacked session cannot survive as a standing login foothold.

## Why

Passkeys are the phishing-resistant factor with email-OTP as the fallback. The hardening
batch closes the seams that make enrollment safe to *rely on*: without UV enforcement a
passkey proves possession but not user; without the password-reset clear, a planted
passkey outlives the very remediation meant to evict an attacker.

> **Backfill note.** Reconstructed 2026-07-07 from the commit history. The adapter crypto
> was verified against a virtual authenticator (virtualwebauthn), and each fix shipped
> with a targeted test (UV-negative rejection, challenge-growth bound, cascade, symmetric
> audit) at its commit. Not independently re-verified for this doc; current tree green at
> `9911b8c`. The assertion/login half is a separate doc ([passkey-login](2026-07-01-passkey-login.md)).
