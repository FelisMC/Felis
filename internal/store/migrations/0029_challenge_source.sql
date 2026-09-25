-- Passkey login challenges stop being one-per-account and are bounded per source.
--
-- An email-first passkey login used to keep one challenge per account: every begin
-- deleted the previous one, so anyone who knew an address could cancel its owner's
-- ceremony by starting another. A login challenge now lives beside the others and
-- finish picks the one whose challenge the browser signed (clientDataJSON carries
-- it), so a stranger's begin never touches the owner's.
--
-- Both login stores are then bounded by where the begins come from: source is the
-- caller's IPv4 address or IPv6 /48, and a source holds only so many live login
-- challenges at once (maxLiveChallengesPerSource). One network flooding begins
-- fills its own allowance and leaves every other network able to sign in.
ALTER TABLE webauthn_challenges ADD COLUMN challenge text;
ALTER TABLE webauthn_challenges ADD COLUMN source text;
ALTER TABLE webauthn_discoverable_challenges ADD COLUMN source text;

CREATE INDEX webauthn_challenges_source_idx
    ON webauthn_challenges (source) WHERE source IS NOT NULL;
CREATE INDEX webauthn_discoverable_challenges_source_idx
    ON webauthn_discoverable_challenges (source);
