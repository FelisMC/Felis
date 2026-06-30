-- Phase 6 passkey/WebAuthn bind (spec §14 WebAuthn, §B2 onboarding "link + email-OTP
-- + passkey"). This is the ENROLLMENT data layer only: an already-authenticated
-- principal binds a passkey to their account (the credential-creation ceremony),
-- and manages the credentials they have bound. Email-OTP remains the fallback
-- factor (migration 0004), so a player with no passkey is never locked out.
--
-- Scope boundary (deliberate): this slice covers ENROLLMENT only. Assertion
-- verification for LOGIN — proving a passkey to mint/elevate a session from an
-- unauthenticated state — is out of scope here and deferred. The spec keeps the two
-- passkey surfaces distinct (§14: admin.* rides "Tunnel+Access，WebAuthn/posture",
-- panel.* is "app 登录"; §B2 lists passkey among the player onboarding factors), so
-- where the panel.* passkey relying-party boundary ultimately lands (felis-api vs.
-- the Access edge) is a later decision, not settled by this migration. Accordingly
-- this migration models only the authenticated enrollment ceremony: every challenge
-- is bound to a known user_id, and there is no usernameless (pre-session) login
-- lookup column. Adding a login path later would also need the felis_session
-- honoring model in session.go extended.

-- webauthn_credentials stores one bound passkey per row. The public key and the
-- signature counter are attestation outputs captured at registration; only public,
-- non-secret material is held (a WebAuthn public key is meant to be public, unlike
-- the RCON password or a session token). credential_id is the authenticator's
-- globally-unique handle, base64url-encoded; UNIQUE guards the (astronomically
-- unlikely) cross-account collision and is the key a future login path would match.
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

-- webauthn_challenges holds the server-side ceremony state between begin and finish.
-- The full go-webauthn SessionData blob (challenge, allowed credentials, expiry) is
-- stashed here and reloaded at finish, so the client never echoes — and so cannot
-- forge — the challenge it must answer (the same principle as email_otps.code_hash).
-- Every row is bound to a known user_id (NOT NULL): enrollment always rides on an
-- authenticated principal, so there is no usernameless consume-by-hash path. consumed_at
-- enforces single-use; a fresh begin supersedes the prior live row for (user, purpose).
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
