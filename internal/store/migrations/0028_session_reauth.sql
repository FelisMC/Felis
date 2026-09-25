-- When the holder of a session last proved a factor the account already had: a
-- passkey assertion, a code mailed to the verified address, or a sign-in through
-- one of those (op-login and the setup token count too). Adding or removing a
-- passkey and changing the email need that proof within the last few minutes,
-- so a stolen cookie alone cannot plant a lasting way in. NULL means the session
-- never proved one (a bind-code sign-in), which is what every existing row gets.
ALTER TABLE sessions ADD COLUMN reauth_at timestamptz;
