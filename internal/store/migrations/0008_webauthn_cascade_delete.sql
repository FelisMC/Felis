-- Phase 6 passkey hardening: make the WebAuthn foreign keys cascade on user deletion.
--
-- 0007 declared webauthn_credentials.user_id and webauthn_challenges.user_id as inline
-- REFERENCES users(id), which Postgres created with ON DELETE NO ACTION (RESTRICT).
-- There is no live user-delete path today, so this is latent — but a bound passkey and
-- a pending challenge are ephemeral auth material with no retention value: when a user
-- row is eventually removed, its authenticators MUST go with it, never linger as an
-- orphaned login foothold. Recreate both foreign keys with ON DELETE CASCADE.
--
-- Scope note (deliberate): cascade is applied ONLY to these ephemeral auth tables, NOT
-- blanket across every users(id) reference. Some child data — notably world_backups —
-- must SURVIVE account removal for retention/audit, so a global cascade would be a
-- data-loss footgun. This migration does not by itself enable user deletion (other child
-- tables still RESTRICT); it only makes the passkey tables cascade-correct for the day a
-- user-delete path lands, so they are never the forgotten dangling reference.
--
-- The constraint names are Postgres's deterministic defaults for an inline single-column
-- REFERENCES: <table>_<column>_fkey. DROP without IF EXISTS is intentional — if a name
-- ever differed the migration must fail loudly, never silently leave the old RESTRICT
-- constraint in place beside a new one.

ALTER TABLE webauthn_credentials
  DROP CONSTRAINT webauthn_credentials_user_id_fkey,
  ADD CONSTRAINT webauthn_credentials_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;

ALTER TABLE webauthn_challenges
  DROP CONSTRAINT webauthn_challenges_user_id_fkey,
  ADD CONSTRAINT webauthn_challenges_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;
