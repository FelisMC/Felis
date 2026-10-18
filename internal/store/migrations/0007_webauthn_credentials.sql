-- Passkey (WebAuthn) enrollment only; email-OTP stays the fallback factor, so no player is locked out.

-- One bound passkey per row. The public key and signature counter are attestation outputs; credential_id is the authenticator's globally unique handle.
CREATE TABLE webauthn_credentials (
  id            text PRIMARY KEY,                      -- opaque row id (crypto-random hex)
  user_id       text NOT NULL REFERENCES users(id),
  credential_id text NOT NULL UNIQUE,                  -- base64url(raw credential id)
  public_key    text NOT NULL,                         -- base64(COSE public key bytes)
  sign_count    bigint NOT NULL DEFAULT 0,             -- uint32 widened (overflows int4)
  aaguid        text,                                  -- authenticator model id, for display only
  name          text,                                  -- caller-supplied nickname ("My phone")
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_used_at  timestamptz                            -- NULL until an assertion is verified (deferred login path)
);

-- The credential-management views list a user's bound passkeys, so index that lookup.
CREATE INDEX webauthn_credentials_user_id_idx ON webauthn_credentials (user_id);

-- Persist ceremony state server-side; consume once and supersede previous begins for the same user and purpose.
CREATE TABLE webauthn_challenges (
  id           text PRIMARY KEY,                       -- opaque row id (crypto-random hex)
  user_id      text NOT NULL REFERENCES users(id),
  purpose      text NOT NULL,                          -- 'passkey_register'
  session_data bytea NOT NULL,                         -- opaque go-webauthn SessionData
  expires_at   timestamptz NOT NULL,                   -- short TTL set by the API clock
  consumed_at  timestamptz,                            -- non-NULL once redeemed (single-use)
  created_at   timestamptz NOT NULL DEFAULT now()
);

-- The finish path consumes the newest live row for a (user, purpose), so index it.
CREATE INDEX webauthn_challenges_user_purpose_idx ON webauthn_challenges (user_id, purpose);
