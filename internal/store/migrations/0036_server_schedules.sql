-- Scheduled tasks (GET/POST /servers/{name}/schedules): an owner or admin asks
-- felis-api to run a console command, restart, stop, start or back up a server
-- at set times. felis-api's schedule runner reads this table every few seconds.
--
-- A schedule fires at minute_of_day (local to timezone) on the weekdays in the
-- bitmask (bit 0 Sunday), or, with every_minutes set, at every multiple of it
-- since local midnight on those days. owner_id is the server's owner when the
-- schedule was saved (NULL for an unowned server): the runner skips and disables
-- a schedule once the server has another owner, so a new owner never inherits
-- somebody else's commands. A recreated server of the same name starts without
-- schedules (SeedServer deletes them).
--
-- run_state is the step of a run still in progress (a restart waits for the
-- server to stop before starting it, a backup of a running server stops it,
-- backs it up and starts it again); run_step_at is when that step began, and
-- run_resume says the run starts the server again once its backup is done.
CREATE TABLE server_schedules (
  id             bigserial PRIMARY KEY,
  server_name    text NOT NULL REFERENCES servers(name) ON DELETE CASCADE,
  owner_id       text REFERENCES users(id),
  label          text NOT NULL DEFAULT '',
  action         text NOT NULL CHECK (action IN ('command', 'restart', 'stop', 'start', 'backup')),
  command        text NOT NULL DEFAULT '',
  every_minutes  integer NOT NULL DEFAULT 0,
  minute_of_day  integer NOT NULL DEFAULT 0 CHECK (minute_of_day BETWEEN 0 AND 1439),
  weekdays       smallint NOT NULL DEFAULT 127 CHECK (weekdays BETWEEN 1 AND 127),
  timezone       text NOT NULL,
  warn_minutes   integer NOT NULL DEFAULT 0,
  enabled        boolean NOT NULL DEFAULT true,
  next_run_at    timestamptz,
  warned_for     timestamptz,
  run_state      text NOT NULL DEFAULT '',
  run_resume     boolean NOT NULL DEFAULT false,
  run_step_at    timestamptz,
  last_run_at    timestamptz,
  last_result    text NOT NULL DEFAULT '',
  last_detail    text NOT NULL DEFAULT '',
  created_by     text NOT NULL,
  created_at     timestamptz NOT NULL DEFAULT now(),
  updated_at     timestamptz NOT NULL DEFAULT now(),
  -- Every write that starts a step stamps it; the runner times each step from it.
  CHECK (run_state = '' OR run_step_at IS NOT NULL)
);
CREATE INDEX server_schedules_server ON server_schedules (server_name, id);
CREATE INDEX server_schedules_due ON server_schedules (next_run_at) WHERE enabled;
CREATE INDEX server_schedules_running ON server_schedules (id) WHERE run_state <> '';
