-- op.console staff sign-in (spec §B op-login): a staff account signs in at
-- op.console with an email-OTP (minted under purpose 'op_login', stored in
-- player_email_otp) PLUS an in-game admin vouching for the attempt via
-- /felis web op approve <request_id>. A row here is the vouch half of that
-- pair: it exists from the moment the OTP checks out until the approved
-- request is exchanged for a session (consumed_at) or expires.
--
-- Lifecycle (derived, no state column): pending while approved_at IS NULL,
-- approved once ApproveOpLogin stamps approved_at/approved_by, dead once
-- consumed_at is set or expires_at passes. Both the approve and the consume
-- UPDATE re-check the full liveness predicate, so a double approval or a
-- replayed finish is a no-op.
CREATE TABLE op_login_requests (
  id          text PRIMARY KEY,                       -- opaque handle shown to the staff member and typed in-game
  user_id     text NOT NULL REFERENCES users(id),     -- the staff account signing in
  email       text NOT NULL,                          -- snapshot for the audit trail (users.email may change later)
  expires_at  timestamptz NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  consumed_at timestamptz,                            -- set exactly once by the finish path
  approved_at timestamptz,                            -- set by the in-game admin's approval
  approved_by text REFERENCES users(id)               -- the approving admin's web account
);

-- ListPendingOpLogins serves the in-game admin's approval prompt: live rows
-- only (pending, unconsumed, unexpired), oldest first.
CREATE INDEX idx_op_login_requests_pending
  ON op_login_requests (created_at)
  WHERE consumed_at IS NULL AND approved_at IS NULL;
