-- Global passwordless: retire the 0003 password columns.
-- The product no longer has a password anywhere — web sessions are minted only
-- by the passwordless doors (passkey, email-OTP, bind code, op-login vouch) and
-- `felis breakGlass` hands the Owner a one-time setup URL instead of a
-- credential. No code path reads or writes these columns any more, so keeping
-- them would preserve stale bcrypt material for an auth model that cannot use
-- it. Dropping the hashes is deliberate and irreversible: it guarantees no
-- legacy password can ever authenticate again.
ALTER TABLE users
  DROP COLUMN password_hash,
  DROP COLUMN must_change_password;
