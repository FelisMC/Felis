-- Account migration (spec §B3 inherit): a LIVE old account hands its owned servers
-- to a new account and is then retired. This is scenario A (the source runs
-- /felis migrate in-game), distinct from the eviction-inherit hook on
-- player_data_holds (scenario B, a barred UUID's stashed data).
--
-- State machine (one live migration per source at a time):
--   initiated   -- /felis migrate in-game put the source account into migrate mode
--   confirmed   -- source proved control via a FRESH web step-up (passkey if any is
--                  enrolled, else email-OTP) — never mere session possession
--   code_issued -- source named the target account by id and minted a one-time code
--   redeemed    -- target logged in, entered the code; owned servers re-pointed to the
--                  target and the source account disabled (terminal)
--
-- The transfer moves server ownership only. It does NOT move the mc_uuid link (the
-- target keeps the in-game identity it logged in with) nor web credentials (email /
-- passkeys stay with the source) — moving credentials would make migrate a
-- credential-theft primitive.
CREATE TABLE account_migrations (
  id              text PRIMARY KEY,
  source_user_id  text NOT NULL REFERENCES users(id),
  target_user_id  text REFERENCES users(id),   -- NULL until code_issued (named at issue time)
  state           text NOT NULL,               -- initiated|confirmed|code_issued|redeemed
  confirm_factor  text,                         -- 'passkey'|'email_otp'; NULL until confirmed
  confirmed_at    timestamptz,                  -- when the step-up proof landed
  code_hash       text,                         -- sha-256 of the one-time code; NULL until code_issued
  code_expires_at timestamptz,                  -- one-time code TTL; NULL until code_issued
  redeemed_at     timestamptz,                  -- when the target spent the code (terminal)
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);

-- At most one live (non-terminal) migration per source. Re-running /felis migrate
-- supersedes any earlier unfinished attempt (StartMigration deletes it first), so
-- this guards against two concurrent live migrations racing the same source.
CREATE UNIQUE INDEX idx_account_migrations_live_source
  ON account_migrations (source_user_id)
  WHERE state <> 'redeemed';

-- Redeem resolves a submitted code to its pending migration. Scoped to code_issued
-- so spent/superseded rows never match.
CREATE INDEX idx_account_migrations_code
  ON account_migrations (code_hash)
  WHERE code_hash IS NOT NULL AND state = 'code_issued';

-- Reuse the shared updated_at trigger installed in 0010_user_management.sql.
CREATE TRIGGER trg_account_migrations_updated_at
  BEFORE UPDATE ON account_migrations
  FOR EACH ROW EXECUTE FUNCTION felis_set_updated_at();
