# Passkey (WebAuthn) login: assertion, discoverable, clone-detection (ledger backfill)

- **Type:** feature — retroactive ledger entry
- **Date:** 2026-07-01 – 2026-07-05
- **Area:** `internal/passkey` (assertion crypto), `internal/api` (login/assertion, unbind, UA-guard), `internal/store` (migrations 0013/0014)
- **Commits:**
  - `e035142` feat(passkey): WebAuthn login/assertion crypto adapter (BeginLogin/FinishLogin over go-webauthn, Oracle-verified against a virtual authenticator; surfaces the signature counter as a ceremony fact)
  - `ec468ba` feat(auth): discoverable (usernameless) passkey login — the from-zero door the username-first assertion couldn't key on
  - `0dbd557` fix(store): renumber the discoverable-login migration 0013 → 0014
  - `9e1df12` feat(passkey): advance `sign_count`, reject clone-warned assertions
  - `4f59d51` feat(auth): owner-tier passkey-unbind remediation endpoint
  - `a63f49d` feat(panel): steer WeChat/QQ in-app browsers to the system browser for passkey — a backend-only UA interstitial (the SPA is untouched); asset/API/health requests pass through, an `ua_ack` cookie lets a determined user continue
- **Tasks:** #40 (from-zero discoverable login), #67 (WeChat/QQ UA-guard in `internal/panel`)

## What it did

Built the assertion (login) half of the ceremony: the crypto adapter, then discoverable
credentials so a user with no typed identifier can still log in (the enrollment
identifier problem the earlier deferral doc named), clone detection via the advancing
signature counter, and the owner-tier unbind remediation. `a63f49d` guards the flow at the
transport edge — WebAuthn is unusable inside the WeChat/QQ WebViews, so those UAs get a
bilingual "open in your system browser" page instead of the passkey SPA.

## Why

Enrollment without a login path is half a feature. Discoverable credentials resolve the
blocker recorded in the earlier deferral (`users.email` is nullable/non-unique and a
player's username is their Minecraft UUID, so username-first assertion had nothing to key
on). The UA-guard stops the most common real-world dead end: a passkey prompt that can
never succeed inside an in-app browser.

> **Backfill note.** Reconstructed 2026-07-07 from the commit history. The assertion crypto
> was verified against a virtual authenticator (enrollment→assertion chain, origin-mismatch
> and unbound-credential rejection); `9e1df12`'s clone policy and the UA-guard pass-through
> were unit-tested at their commits. Not independently re-verified for this doc; current
> tree green at `9911b8c`.
