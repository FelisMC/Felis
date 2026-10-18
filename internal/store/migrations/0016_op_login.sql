-- Staff sign-in requires an email OTP and an in-game administrator approval.
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
