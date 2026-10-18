-- revoked_at takes away a player's wake right without deleting the row, since a deleted row returns on the player's next join.
ALTER TABLE server_allowlist ADD COLUMN revoked_at timestamptz;
