-- Where an op.console sign-in started; the in-game admin sees it before vouching. Recorded once at start; older rows are blank.
ALTER TABLE op_login_requests
  ADD COLUMN client_ip text NOT NULL DEFAULT '',
  ADD COLUMN user_agent text NOT NULL DEFAULT '';
