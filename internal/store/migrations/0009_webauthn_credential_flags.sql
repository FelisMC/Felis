-- Persists each credential's WebAuthn flags; enrollment requires user verification, so new rows always have user_verified true.

ALTER TABLE webauthn_credentials
  ADD COLUMN user_verified   boolean NOT NULL DEFAULT false,
  ADD COLUMN backup_eligible boolean NOT NULL DEFAULT false,
  ADD COLUMN backup_state    boolean NOT NULL DEFAULT false;
