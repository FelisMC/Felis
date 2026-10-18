-- WebAuthn foreign keys cascade on user deletion: authenticators and pending challenges must not outlive their user.

ALTER TABLE webauthn_credentials
  DROP CONSTRAINT webauthn_credentials_user_id_fkey,
  ADD CONSTRAINT webauthn_credentials_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;

ALTER TABLE webauthn_challenges
  DROP CONSTRAINT webauthn_challenges_user_id_fkey,
  ADD CONSTRAINT webauthn_challenges_user_id_fkey
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE;
