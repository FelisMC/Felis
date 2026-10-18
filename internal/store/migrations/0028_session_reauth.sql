-- Passkey/email changes require recent factor proof; NULL means the session has never reauthenticated.
ALTER TABLE sessions ADD COLUMN reauth_at timestamptz;
