-- Existing case-insensitive verified-email duplicates must be resolved before this unique index can be created.
CREATE UNIQUE INDEX users_verified_email_unique ON users (lower(email))
  WHERE email_verified;
