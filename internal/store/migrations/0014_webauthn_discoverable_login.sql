-- Challenge store for discoverable (usernameless) passkey login, keyed by an opaque handle: the account is revealed only inside the signed assertion.
CREATE TABLE webauthn_discoverable_challenges (
    id           text        PRIMARY KEY,          -- opaque login handle (login_id): 128-bit hex
    session_data bytea       NOT NULL,             -- marshaled webauthn.SessionData (the challenge lives here)
    expires_at   timestamptz NOT NULL,
    consumed_at  timestamptz,                       -- single-use: NULL until finish stamps it
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX webauthn_discoverable_challenges_expires_idx
    ON webauthn_discoverable_challenges (expires_at);
