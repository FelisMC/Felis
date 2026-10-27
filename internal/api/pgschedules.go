package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const scheduleColumns = `s.id, s.server_name, COALESCE(s.owner_id, ''), s.label, s.steps,
	s.every_minutes, s.minute_of_day, s.weekdays, s.timezone, s.run_once_at, s.max_runs, s.run_count,
	s.warn_minutes, s.enabled, s.next_run_at, s.warned_for, s.run_state, s.run_step, s.run_resume, s.run_step_at,
	s.last_run_at, s.last_result, s.last_detail, s.created_by, s.created_at`

// scheduleFinal is true once a run claimed or finished the schedule's last allowed run.
const scheduleFinal = `(run_once_at IS NOT NULL OR (max_runs > 0 AND run_count >= max_runs))`

type rowScanner interface{ Scan(dest ...any) error }

func scanSchedule(row rowScanner, extra ...any) (*Schedule, error) {
	var s Schedule
	var steps []byte
	var once, next, warned, step, last sql.NullTime
	dest := []any{&s.ID, &s.Server, &s.OwnerID, &s.Label, &steps,
		&s.EveryMinutes, &s.MinuteOfDay, &s.Weekdays, &s.Timezone, &once, &s.MaxRuns, &s.RunCount,
		&s.WarnMinutes, &s.Enabled, &next, &warned, &s.RunState, &s.RunStep, &s.RunResume, &step,
		&last, &s.LastResult, &s.LastDetail, &s.CreatedBy, &s.CreatedAt}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(steps, &s.Steps); err != nil {
		return nil, fmt.Errorf("schedule %d steps: %w", s.ID, err)
	}
	s.RunOnceAt, s.NextRunAt, s.WarnedFor = nullTimePtr(once), nullTimePtr(next), nullTimePtr(warned)
	s.RunStepAt, s.LastRunAt = nullTimePtr(step), nullTimePtr(last)
	return &s, nil
}

func nullTimePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

func (p *PGRepo) ListSchedules(ctx context.Context, server string) ([]Schedule, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT `+scheduleColumns+` FROM server_schedules s WHERE s.server_name = $1 ORDER BY s.id`, server)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

func (p *PGRepo) GetSchedule(ctx context.Context, server string, id int64) (*Schedule, error) {
	s, err := scanSchedule(p.db.QueryRowContext(ctx,
		`SELECT `+scheduleColumns+` FROM server_schedules s WHERE s.id = $1 AND s.server_name = $2`, id, server))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

// CreateSchedule locks the server row so concurrent creates cannot exceed the per-server limit.
func (p *PGRepo) CreateSchedule(ctx context.Context, s *Schedule, limit int) error {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var one int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM servers WHERE name = $1 AND deleted_at IS NULL FOR UPDATE`, s.Server).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM server_schedules WHERE server_name = $1`, s.Server).Scan(&n); err != nil {
		return err
	}
	if n >= limit {
		return ErrScheduleLimit
	}
	steps, err := json.Marshal(s.Steps)
	if err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO server_schedules (server_name, owner_id, label, steps, every_minutes,
		   minute_of_day, weekdays, timezone, run_once_at, max_runs, warn_minutes, enabled, next_run_at, created_by)
		 VALUES ($1, NULLIF($2, ''), $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
		 RETURNING id, created_at`,
		s.Server, s.OwnerID, s.Label, steps, s.EveryMinutes,
		s.MinuteOfDay, s.Weekdays, s.Timezone, s.RunOnceAt, s.MaxRuns, s.WarnMinutes, s.Enabled, s.NextRunAt, s.CreatedBy,
	).Scan(&s.ID, &s.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *PGRepo) scheduleMissing(ctx context.Context, server string, id int64) error {
	var state string
	err := p.db.QueryRowContext(ctx,
		`SELECT run_state FROM server_schedules WHERE id = $1 AND server_name = $2`, id, server).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return err
	case state != "":
		return ErrScheduleRunning
	}
	return fmt.Errorf("schedule %d of %s did not change", id, server)
}

// UpdateSchedule starts a fresh run budget when it re-enables a disabled schedule.
func (p *PGRepo) UpdateSchedule(ctx context.Context, s *Schedule) error {
	steps, err := json.Marshal(s.Steps)
	if err != nil {
		return err
	}
	res, err := p.db.ExecContext(ctx,
		`UPDATE server_schedules SET owner_id = NULLIF($3, ''), label = $4, steps = $5,
		   every_minutes = $6, minute_of_day = $7, weekdays = $8, timezone = $9, run_once_at = $10,
		   max_runs = $11, warn_minutes = $12, enabled = $13, next_run_at = $14, warned_for = NULL,
		   run_count = CASE WHEN $13 AND NOT enabled THEN 0 ELSE run_count END, updated_at = now()
		 WHERE id = $1 AND server_name = $2 AND run_state = ''`,
		s.ID, s.Server, s.OwnerID, s.Label, steps,
		s.EveryMinutes, s.MinuteOfDay, s.Weekdays, s.Timezone, s.RunOnceAt,
		s.MaxRuns, s.WarnMinutes, s.Enabled, s.NextRunAt)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return err
		}
		return p.scheduleMissing(ctx, s.Server, s.ID)
	}
	return nil
}

func (p *PGRepo) DeleteSchedule(ctx context.Context, server string, id int64) error {
	res, err := p.db.ExecContext(ctx,
		`DELETE FROM server_schedules WHERE id = $1 AND server_name = $2 AND run_state = ''`, id, server)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return err
		}
		return p.scheduleMissing(ctx, server, id)
	}
	return nil
}

func (p *PGRepo) DueSchedules(ctx context.Context, horizon time.Time) ([]DueSchedule, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT `+scheduleColumns+`, COALESCE(v.owner_id, '')
		 FROM server_schedules s JOIN servers v ON v.name = s.server_name
		 WHERE v.deleted_at IS NULL AND (s.run_state <> '' OR (s.enabled AND s.next_run_at <= $1))
		 ORDER BY s.run_state = '', s.next_run_at, s.id`, horizon)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueSchedule
	for rows.Next() {
		var owner string
		s, err := scanSchedule(rows, &owner)
		if err != nil {
			return nil, err
		}
		out = append(out, DueSchedule{Schedule: *s, ServerOwner: owner})
	}
	return out, rows.Err()
}

func (p *PGRepo) execApplied(ctx context.Context, query string, args ...any) (bool, error) {
	res, err := p.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (p *PGRepo) ClaimScheduleRun(ctx context.Context, id int64, due, next *time.Time, now time.Time) (bool, error) {
	const claim = `UPDATE server_schedules SET run_state = 'claimed', run_step = 0, run_resume = false,
		   run_step_at = $2, last_run_at = $2, last_result = '', last_detail = '', run_count = run_count + 1,
		   next_run_at = CASE WHEN run_once_at IS NOT NULL OR (max_runs > 0 AND run_count + 1 >= max_runs)
		     THEN NULL ELSE `
	if due == nil {
		return p.execApplied(ctx, claim+`next_run_at END
		 WHERE id = $1 AND run_state = ''`, id, now)
	}
	return p.execApplied(ctx, claim+`$4::timestamptz END, warned_for = NULL
		 WHERE id = $1 AND run_state = '' AND enabled AND next_run_at = $3`, id, now, *due, next)
}

func (p *PGRepo) AdvanceScheduleRun(ctx context.Context, id int64, from, to RunPos, resume bool, result, detail string, now time.Time) (bool, error) {
	return p.execApplied(ctx,
		`UPDATE server_schedules SET run_step = $4, run_state = $5, run_resume = $6, run_step_at = $9,
		   last_result = CASE WHEN $7::text = '' THEN last_result ELSE $7::text END,
		   last_detail = CASE WHEN $7::text = '' THEN last_detail ELSE $8::text END
		 WHERE id = $1 AND run_step = $2 AND run_state = $3`,
		id, from.Step, from.Phase, to.Step, to.Phase, resume, result, detail, now)
}

func (p *PGRepo) FinishScheduleRun(ctx context.Context, id int64, from RunPos, result, detail string, next *time.Time, now time.Time) (bool, error) {
	return p.execApplied(ctx,
		`UPDATE server_schedules SET run_state = '', run_step = 0, run_resume = false, run_step_at = NULL,
		   last_result = $4, last_detail = $5,
		   enabled = enabled AND NOT `+scheduleFinal+`,
		   next_run_at = CASE WHEN `+scheduleFinal+` THEN NULL
		     WHEN enabled AND next_run_at < $7 THEN $6::timestamptz ELSE next_run_at END
		 WHERE id = $1 AND run_step = $2 AND run_state = $3::text AND $3::text <> ''`,
		id, from.Step, from.Phase, result, detail, next, now)
}

func (p *PGRepo) MissScheduleRun(ctx context.Context, id int64, due time.Time, next *time.Time, detail string) (bool, error) {
	return p.execApplied(ctx,
		`UPDATE server_schedules SET next_run_at = $3, enabled = $3::timestamptz IS NOT NULL, warned_for = NULL,
		   last_run_at = $2, last_result = 'missed', last_detail = $4
		 WHERE id = $1 AND run_state = '' AND enabled AND next_run_at = $2`, id, due, next, detail)
}

func (p *PGRepo) WarnScheduleRun(ctx context.Context, id int64, due time.Time) (bool, error) {
	return p.execApplied(ctx,
		`UPDATE server_schedules SET warned_for = $2
		 WHERE id = $1 AND run_state = '' AND enabled AND next_run_at = $2
		   AND warned_for IS DISTINCT FROM $2`, id, due)
}

func (p *PGRepo) DisableSchedule(ctx context.Context, id int64, detail string) (bool, error) {
	return p.execApplied(ctx,
		`UPDATE server_schedules SET enabled = false, next_run_at = NULL, warned_for = NULL,
		   last_result = 'skipped', last_detail = $2, updated_at = now()
		 WHERE id = $1 AND run_state = '' AND enabled`, id, detail)
}
