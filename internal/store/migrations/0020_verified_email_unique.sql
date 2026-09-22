-- The verified-email uniqueness that the email-first login design has claimed
-- since 0010 but no migration ever actually created: at most one account may
-- hold a PROVEN address (email_verified), compared case-insensitively, so the
-- pre-session login door (UserByEmail on lower(email)) resolves at most one
-- identity. VerifyEmailOTP's ErrEmailTaken guard is the application-level
-- counterpart; this index is the database-level backstop behind it, so a
-- cross-user race that passes the guard still cannot write a second verified
-- row (it loses to a unique violation instead).
--
-- If an upgrade meets a database where the missing guard already produced two
-- verified rows for one address, CREATE UNIQUE INDEX refuses to build and names
-- the duplicated key in DETAIL. That is deliberate — no silent "unverify the
-- loser" surgery: only a human can say which holder keeps the address.
CREATE UNIQUE INDEX users_verified_email_unique ON users (lower(email))
  WHERE email_verified;
