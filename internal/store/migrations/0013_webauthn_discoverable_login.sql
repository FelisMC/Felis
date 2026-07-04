-- Phase 6 passkey — the challenge store for DISCOVERABLE ("usernameless") login (spec §14,
-- task #40). webauthn_challenges (0007) is keyed by (user_id, purpose) because both
-- enrollment and email-first login already know WHO is authenticating before the ceremony
-- starts. A from-zero passkey login does not: the browser calls navigator.credentials.get()
-- with an EMPTY allowCredentials list, the authenticator offers a resident credential it
-- holds for this RP, and the account is revealed only inside the signed assertion at finish.
-- So this challenge cannot be keyed by user — it is keyed by an opaque, server-minted handle
-- (login_id) the browser echoes back at finish, and the marshaled WebAuthn SessionData is the
-- only server-held ceremony state. This is exactly the "non-user-keyed challenge store, a
-- future migration" that 0007's own comment anticipated.
--
-- Bounding. webauthn_challenges self-bounds via a per-(user,purpose) supersede-DELETE on each
-- begin — one live row per user+purpose. That key does not exist here (there is no user at
-- begin), so this table is bounded two ways instead, both inside CreateDiscoverableChallenge's
-- one transaction: (1) every begin first reaps rows that already expired or were consumed by a
-- prior finish, and (2) a hard cap (maxLiveDiscoverableChallenges) refuses a new begin once the
-- live count is reached, so an abusive begin-flood is bounded to trivial storage rather than
-- growing without limit. A reap alone does NOT bound a burst — freshly inserted rows have a
-- future expiry, so N begins inside the TTL leave N live rows — which is why the cap, not the
-- reap, is the real ceiling. Volumetric per-source (client-IP) limiting is deliberately left to
-- the edge, the same stance handlers_auth_email.go documents (behind Cloudflare RemoteAddr is
-- the proxy, and CGNAT would false-positive) and unavoidable here since a usernameless door has
-- neither a principal NOR a typed recipient to key a fair per-caller limit on.
--
-- The expires_at index serves the reap's WHERE clause; consumed_at (nullable) makes the row
-- single-use, stamped at finish and swept by a later begin's reap.
CREATE TABLE webauthn_discoverable_challenges (
    id           text        PRIMARY KEY,          -- opaque login handle (login_id): 128-bit hex
    session_data bytea       NOT NULL,             -- marshaled webauthn.SessionData (the challenge lives here)
    expires_at   timestamptz NOT NULL,
    consumed_at  timestamptz,                       -- single-use: NULL until finish stamps it
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX webauthn_discoverable_challenges_expires_idx
    ON webauthn_discoverable_challenges (expires_at);
