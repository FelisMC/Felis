-- An owner can take a player's right to wake the server away (PUT
-- /servers/{name}/allowlist/{uuid}). The row stays with revoked_at set instead of
-- being deleted, because a deleted row comes straight back on the player's next
-- join (RecordJoin appends ON CONFLICT DO NOTHING) and the owner's choice would
-- last only until then. Both wake gates read revoked_at IS NULL.
ALTER TABLE server_allowlist ADD COLUMN revoked_at timestamptz;
