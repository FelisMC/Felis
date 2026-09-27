package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The server_schedules store (ServerSchedules, migration 0036).

// scheduleColumns is the SELECT list scanSchedule reads, over server_schedules
// aliased s.
const scheduleColumns = `s.id, s.server_name, COALESCE(s.owner_id, ''), s.label, s.action, s.command,
	s.every_minutes, s.minute_of_day, s.weekdays, s.timezone, s.warn_minutes, s.enabled,
	s.next_run_at, s.warned_for, s.run_state, s.run_resume, s.run_step_at,
	s.last_run_at, s.last_result, s.last_detail, s.created_by, s.created_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanSchedule(row rowScanner, extra ...any) (*Schedule, error) {
	var s Schedule
	var next, warned, step, last sql.NullTime
	dest := []any{&s.ID, &s.Server, &s.OwnerID, &s.Label, &s.Action, &s.Command,
		&s.EveryMinutes, &s.MinuteOfDay, &s.Weekdays, &s.Timezone, &s.WarnMinutes, &s.Enabled,
		&next, &warned, &s.RunState, &s.RunResume, &step,
		&last, &s.LastResult, &s.LastDetail, &s.CreatedBy, &s.CreatedAt}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	s.NextRunAt, s.WarnedFor, s.RunStepAt, s.LastRunAt = nullTimePtr(next), nullTimePtr(warned), nullTimePtr(step), nullTimePtr(last)
	return &s, nil
}

func nullTimePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}

// ListSchedules lists a server's schedules, oldest first.
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

// GetSchedule reads one schedule of server, or ErrNotFound.
func (p *PGRepo) GetSchedule(ctx context.Context, server string, id int64) (*Schedule, error) {
	s, err := scanSchedule(p.db.QueryRowContext(ctx,
		`SELECT `+scheduleColumns+` FROM server_schedules s WHERE s.id = $1 AND s.server_name = $2`, id, server))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return s, err
}

// CreateSchedule inserts s under the server row's lock, so two saves racing
// for the last free place cannot both take it.
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
	if err := tx.QueryRowContext(ctx,
		`INSERT INTO server_schedules (server_name, owner_id, label, action, command, every_minutes,
		   minute_of_day, weekdays, timezone, warn_minutes, enabled, next_run_at, created_by)
		 VALUES ($1, NULLIF($2, ''), $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		 RETURNING id, created_at`,
		s.Server, s.OwnerID, s.Label, s.Action, s.Command, s.EveryMinutes,
		s.MinuteOfDay, s.Weekdays, s.Timezone, s.WarnMinutes, s.Enabled, s.NextRunAt, s.CreatedBy,
	).Scan(&s.ID, &s.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

// scheduleMissing tells why a write guarded on an idle run_state touched no
// row: the schedule is gone, or it is running.
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

// UpdateSchedule writes s's settings, owner and next run, unless it is running.
func (p *PGRepo) UpdateSchedule(ctx context.Context, s *Schedule) error {
	res, err := p.db.ExecContext(ctx,
		`UPDATE server_schedules SET owner_id = NULLIF($3, ''), label = $4, action = $5, command = $6,
		   every_minutes = $7, minute_of_day = $8, weekdays = $9, timezone = $10, warn_minutes = $11,
		   enabled = $12, next_run_at = $13, warned_for = NULL, updated_at = now()
		 WHERE id = $1 AND server_name = $2 AND run_state = ''`,
		s.ID, s.Server, s.OwnerID, s.Label, s.Action, s.Command,
		s.EveryMinutes, s.MinuteOfDay, s.Weekdays, s.Timezone, s.WarnMinutes,
		s.Enabled, s.NextRunAt)
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

// DeleteSchedule removes a schedule, unless it is running.
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

// DueSchedules lists the schedules the runner has to look at, runs in progress
// first, then by due time.
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

// execApplied runs a compare-and-set write and reports whether it matched.
func (p *PGRepo) execApplied(ctx context.Context, query string, args ...any) (bool, error) {
	res, err := p.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ClaimScheduleRun starts a run of a schedule without one.
func (p *PGRepo) ClaimScheduleRun(ctx context.Context, id int64, due, next *time.Time, now time.Time) (bool, error) {
	if due == nil {
		return p.execApplied(ctx,
			`UPDATE server_schedules SET run_state = 'claimed', run_resume = false, run_step_at = $2,
			   last_run_at = $2, last_result = '', last_detail = ''
			 WHERE id = $1 AND run_state = ''`, id, now)
	}
	return p.execApplied(ctx,
		`UPDATE server_schedules SET run_state = 'claimed', run_resume = false, run_step_at = $4,
		   last_run_at = $4, last_result = '', last_detail = '', next_run_at = $3, warned_for = NULL
		 WHERE id = $1 AND run_state = '' AND enabled AND next_run_at = $2`, id, *due, next, now)
}

// AdvanceScheduleRun moves a run on to its next step.
func (p *PGRepo) AdvanceScheduleRun(ctx context.Context, id int64, from, to string, resume bool, result, detail string, now time.Time) (bool, error) {
	return p.execApplied(ctx,
		`UPDATE server_schedules SET run_state = $3, run_resume = $4, run_step_at = $7,
		   last_result = CASE WHEN $5::text = '' THEN last_result ELSE $5::text END,
		   last_detail = CASE WHEN $5::text = '' THEN last_detail ELSE $6::text END
		 WHERE id = $1 AND run_state = $2`, id, from, to, resume, result, detail, now)
}

// FinishScheduleRun ends a run with its outcome.
func (p *PGRepo) FinishScheduleRun(ctx context.Context, id int64, from, result, detail string) (bool, error) {
	return p.execApplied(ctx,
		`UPDATE server_schedules SET run_state = '', run_resume = false, run_step_at = NULL,
		   last_result = $3, last_detail = $4
		 WHERE id = $1 AND run_state = $2::text AND $2::text <> ''`, id, from, result, detail)
}

// MissScheduleRun records the run due then as missed and moves on to next.
func (p *PGRepo) MissScheduleRun(ctx context.Context, id int64, due, next time.Time, detail string) (bool, error) {
	return p.execApplied(ctx,
		`UPDATE server_schedules SET next_run_at = $3, warned_for = NULL,
		   last_run_at = $2, last_result = 'missed', last_detail = $4
		 WHERE id = $1 AND run_state = '' AND enabled AND next_run_at = $2`, id, due, next, detail)
}

// WarnScheduleRun marks the run due then as warned about.
func (p *PGRepo) WarnScheduleRun(ctx context.Context, id int64, due time.Time) (bool, error) {
	return p.execApplied(ctx,
		`UPDATE server_schedules SET warned_for = $2
		 WHERE id = $1 AND run_state = '' AND enabled AND next_run_at = $2
		   AND warned_for IS DISTINCT FROM $2`, id, due)
}

// DisableSchedule turns off an enabled, idle schedule and records why.
func (p *PGRepo) DisableSchedule(ctx context.Context, id int64, detail string) (bool, error) {
	return p.execApplied(ctx,
		`UPDATE server_schedules SET enabled = false, next_run_at = NULL, warned_for = NULL,
		   last_result = 'skipped', last_detail = $2, updated_at = now()
		 WHERE id = $1 AND run_state = '' AND enabled`, id, detail)
}
