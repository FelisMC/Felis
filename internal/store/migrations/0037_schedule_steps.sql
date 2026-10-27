-- A schedule runs an ordered list of steps; run_step and run_state locate a run inside it.
-- run_once_at marks a one-time schedule; max_runs > 0 disables a schedule after that many claimed runs.
ALTER TABLE server_schedules
  ADD COLUMN steps       jsonb,
  ADD COLUMN run_once_at timestamptz,
  ADD COLUMN max_runs    integer NOT NULL DEFAULT 0 CHECK (max_runs BETWEEN 0 AND 10000),
  ADD COLUMN run_count   integer NOT NULL DEFAULT 0 CHECK (run_count >= 0),
  ADD COLUMN run_step    integer NOT NULL DEFAULT 0 CHECK (run_step BETWEEN 0 AND 9);

UPDATE server_schedules SET
  steps = CASE action
    WHEN 'command' THEN jsonb_build_array(jsonb_build_object('type', 'command', 'command', command))
    ELSE jsonb_build_array(jsonb_build_object('type', action))
  END,
  run_state = CASE
    WHEN action = 'backup' AND run_state = 'stopping' AND run_resume THEN 'backup_stopping'
    WHEN action = 'backup' AND run_state = 'stopping' THEN 'backup_waiting'
    WHEN action = 'backup' AND run_state = 'backing_up' AND run_resume THEN 'backup_restart'
    ELSE run_state
  END;

ALTER TABLE server_schedules
  ALTER COLUMN steps SET NOT NULL,
  ADD CHECK (jsonb_typeof(steps) = 'array' AND jsonb_array_length(steps) BETWEEN 1 AND 10),
  DROP COLUMN action,
  DROP COLUMN command;
