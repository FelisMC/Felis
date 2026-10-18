-- Drops the 0003 password columns: the product has no passwords. Irreversible by design, so no legacy password can authenticate again.
ALTER TABLE users
  DROP COLUMN password_hash,
  DROP COLUMN must_change_password;
