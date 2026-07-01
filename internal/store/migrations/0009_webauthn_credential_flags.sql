-- Phase 6 passkey hardening: record the WebAuthn ceremony flags on each bound credential.
--
-- 0007 stored the credential material but discarded the authenticator-data flags. The
-- enrollment ceremony now requires user verification (verifier.go sets UV=required), and
-- we persist the flags the ceremony reported so the guarantee is auditable and a future
-- login/step-up path can enforce or reason about them per credential:
--   user_verified   — a PIN/biometric (not mere presence) was performed at bind. With the
--                     required-UV policy this is always true for new rows, but persisting
--                     it survives a future policy that permits UV=preferred credentials.
--   backup_eligible — the credential is exportable/syncable across devices (a passkey that
--                     lives in a cloud keychain), as opposed to a single-device key.
--   backup_state    — the credential is currently backed up / synced.
--
-- DEFAULT false backfills any pre-existing row (none in practice: enrollment shipped in
-- 0007 with no production data yet) to the conservative "not verified, single-device"
-- reading; NOT NULL keeps the Go scan a plain bool with no nullable handling.

ALTER TABLE webauthn_credentials
  ADD COLUMN user_verified   boolean NOT NULL DEFAULT false,
  ADD COLUMN backup_eligible boolean NOT NULL DEFAULT false,
  ADD COLUMN backup_state    boolean NOT NULL DEFAULT false;
