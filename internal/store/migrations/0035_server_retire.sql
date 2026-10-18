-- Give-up and retirement requests, carried out by the reaper, the only component that deletes a world.
-- Until then the server stays stopped and cannot be woken or claimed.
ALTER TABLE servers ADD COLUMN retire_requested_at timestamptz;
ALTER TABLE servers ADD COLUMN retire_delete boolean NOT NULL DEFAULT false;
