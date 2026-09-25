-- Where an op.console sign-in was started. The in-game admin sees it before
-- vouching (/felis web op approve <code>), next to the account name and
-- address, so a request started from an unfamiliar network or browser can be
-- turned down. Recorded once, at start; rows from before this migration show
-- blanks.
ALTER TABLE op_login_requests
  ADD COLUMN client_ip text NOT NULL DEFAULT '',
  ADD COLUMN user_agent text NOT NULL DEFAULT '';
