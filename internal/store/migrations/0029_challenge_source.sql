-- Concurrent login begins coexist; finish selects the signed challenge and admission remains bounded per source.
ALTER TABLE webauthn_challenges ADD COLUMN challenge text;
ALTER TABLE webauthn_challenges ADD COLUMN source text;
ALTER TABLE webauthn_discoverable_challenges ADD COLUMN source text;

CREATE INDEX webauthn_challenges_source_idx
    ON webauthn_challenges (source) WHERE source IS NOT NULL;
CREATE INDEX webauthn_discoverable_challenges_source_idx
    ON webauthn_discoverable_challenges (source);
