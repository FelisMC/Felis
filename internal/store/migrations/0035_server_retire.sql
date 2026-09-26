-- An owner can give a server up, and an admin can retire one for good
-- (PUT /servers/{name}/retirement). The request is recorded here and carried out
-- by the reaper, the one component that deletes a world: on its next run it
-- archives the world, deletes the volume and releases the server, and with
-- retire_delete it also removes the MinecraftServer and marks this row deleted.
-- Until then the server stays stopped and cannot be woken or claimed.
ALTER TABLE servers ADD COLUMN retire_requested_at timestamptz;
ALTER TABLE servers ADD COLUMN retire_delete boolean NOT NULL DEFAULT false;
