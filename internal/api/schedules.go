package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // embed timezone data for hosts without zoneinfo
	"unicode/utf8"
)

const (
	ScheduleCommand = "command"
	ScheduleRestart = "restart"
	ScheduleStop    = "stop"
	ScheduleStart   = "start"
	ScheduleBackup  = "backup"
)

// LastResult is empty while a run is in progress.
const (
	ScheduleOK      = "ok"
	ScheduleSkipped = "skipped"
	ScheduleFailed  = "failed"
	// ScheduleMissed drops runs older than scheduleMissGrace instead of executing them hours late.
	ScheduleMissed = "missed"
)

const (
	maxSchedulesPerServer = 20
	maxScheduleLabel      = 64
	// maxScheduleDetail is measured in bytes.
	maxScheduleDetail    = 500
	minPowerEveryMinutes = 60
)

var (
	// scheduleEveryMinutes divides a day so runs keep the same local clock times each day.
	scheduleEveryMinutes = []int{15, 30, 60, 120, 180, 240, 360, 480, 720}
	scheduleWarnMinutes  = []int{0, 1, 5, 10, 15, 30}
)

var (
	ErrScheduleLimit   = errors.New("schedule limit reached")
	ErrScheduleRunning = errors.New("schedule is running")
)

type Schedule struct {
	ID      int64  `json:"id"`
	Server  string `json:"server"`
	Label   string `json:"label"`
	Action  string `json:"action"`
	Command string `json:"command"`
	// EveryMinutes counts from local midnight; zero uses MinuteOfDay, and Weekdays is a bitmask with bit 0 for Sunday.
	EveryMinutes int    `json:"every_minutes"`
	MinuteOfDay  int    `json:"minute_of_day"`
	Weekdays     int    `json:"weekdays"`
	Timezone     string `json:"timezone"`
	// WarnMinutes=0 disables the in-game warning.
	WarnMinutes int  `json:"warn_minutes"`
	Enabled     bool `json:"enabled"`
	// NextRunAt is nil while disabled.
	NextRunAt *time.Time `json:"next_run_at"`
	// RunState is empty when idle; runClaimed denotes the first step.
	RunState   string     `json:"run_state"`
	LastRunAt  *time.Time `json:"last_run_at"`
	LastResult string     `json:"last_result"`
	LastDetail string     `json:"last_detail"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`

	// OwnerID captures ownership at save time; a changed owner invalidates the schedule until saved again.
	OwnerID   string     `json:"-"`
	WarnedFor *time.Time `json:"-"`
	RunResume bool       `json:"-"`
	RunStepAt *time.Time `json:"-"`
}

type DueSchedule struct {
	Schedule
	ServerOwner string
}

// ServerSchedules uses compare-and-set transitions to prevent duplicate runs across API replicas.
type ServerSchedules interface {
	ListSchedules(ctx context.Context, server string) ([]Schedule, error)
	GetSchedule(ctx context.Context, server string, id int64) (*Schedule, error)
	// CreateSchedule fills ID and CreatedAt, or returns ErrScheduleLimit at the per-server limit.
	CreateSchedule(ctx context.Context, s *Schedule, limit int) error
	// UpdateSchedule clears the warning marker and refuses changes during a run.
	UpdateSchedule(ctx context.Context, s *Schedule) error
	// DeleteSchedule refuses deletion during a run.
	DeleteSchedule(ctx context.Context, server string, id int64) error

	// DueSchedules includes enabled live-server schedules due by horizon and all runs in progress.
	DueSchedules(ctx context.Context, horizon time.Time) ([]DueSchedule, error)
	// ClaimScheduleRun advances next only for a due run; nil due starts a manual run without changing the next scheduled time.
	ClaimScheduleRun(ctx context.Context, id int64, due, next *time.Time, now time.Time) (bool, error)
	// AdvanceScheduleRun requires the current step to match from and records resume and any nonempty result.
	AdvanceScheduleRun(ctx context.Context, id int64, from, to string, resume bool, result, detail string, now time.Time) (bool, error)
	// FinishScheduleRun applies only at step from.
	FinishScheduleRun(ctx context.Context, id int64, from, result, detail string) (bool, error)
	// MissScheduleRun records the expected due run and advances to next.
	MissScheduleRun(ctx context.Context, id int64, due, next time.Time, detail string) (bool, error)
	// WarnScheduleRun marks each due run at most once.
	WarnScheduleRun(ctx context.Context, id int64, due time.Time) (bool, error)
	// DisableSchedule applies only when enabled and idle, recording the reason.
	DisableSchedule(ctx context.Context, id int64, detail string) (bool, error)
}

// scheduleInput defaults Enabled to true.
type scheduleInput struct {
	Label        string `json:"label"`
	Action       string `json:"action"`
	Command      string `json:"command"`
	EveryMinutes int    `json:"every_minutes"`
	MinuteOfDay  int    `json:"minute_of_day"`
	Weekdays     int    `json:"weekdays"`
	Timezone     string `json:"timezone"`
	WarnMinutes  int    `json:"warn_minutes"`
	Enabled      *bool  `json:"enabled"`
}

func badSchedule(format string, a ...any) error {
	return newError(http.StatusBadRequest, "bad_schedule", format, a...)
}

func (in *scheduleInput) apply(s *Schedule) error {
	label := strings.TrimSpace(in.Label)
	if utf8.RuneCountInString(label) > maxScheduleLabel {
		return badSchedule("label too long (max %d characters)", maxScheduleLabel)
	}
	if strings.IndexFunc(label, func(c rune) bool { return c < 0x20 || c == 0x7f }) >= 0 {
		return badSchedule("label must be a single line")
	}

	var command string
	switch in.Action {
	case ScheduleCommand:
		c, err := normalizeConsoleCommand(in.Command)
		if err != nil {
			return err
		}
		command = c
	case ScheduleRestart, ScheduleStop, ScheduleStart, ScheduleBackup:
		if strings.TrimSpace(in.Command) != "" {
			return badSchedule("only a command schedule takes a command")
		}
	default:
		return badSchedule("action must be one of command, restart, stop, start, backup")
	}

	switch {
	case in.EveryMinutes == 0:
		if in.MinuteOfDay < 0 || in.MinuteOfDay > 24*60-1 {
			return badSchedule("minute_of_day must be between 0 and 1439")
		}
	case !slices.Contains(scheduleEveryMinutes, in.EveryMinutes):
		return badSchedule("every_minutes must be 0 or one of 15, 30, 60, 120, 180, 240, 360, 480, 720")
	case in.EveryMinutes < minPowerEveryMinutes && in.Action != ScheduleCommand:
		return badSchedule("a %s can repeat at most every %d minutes", in.Action, minPowerEveryMinutes)
	case in.MinuteOfDay != 0:
		return badSchedule("a repeating schedule has no minute_of_day")
	}
	if in.Weekdays < 1 || in.Weekdays > 0x7f {
		return badSchedule("weekdays must name at least one day (bits 0 to 6, Sunday first)")
	}

	tz := strings.TrimSpace(in.Timezone)
	if tz == "" || tz == "Local" {
		return badSchedule("timezone must be an IANA zone name such as Asia/Shanghai")
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return badSchedule("unknown timezone %q", tz)
	}

	if !slices.Contains(scheduleWarnMinutes, in.WarnMinutes) {
		return badSchedule("warn_minutes must be one of 0, 1, 5, 10, 15, 30")
	}
	// Power actions repeat at most hourly so a warning cannot precede the previous run.
	if in.WarnMinutes > 0 && (in.Action == ScheduleCommand || in.Action == ScheduleStart) {
		return badSchedule("only a restart, stop or backup warns the players")
	}

	s.Label, s.Action, s.Command = label, in.Action, command
	s.EveryMinutes, s.MinuteOfDay, s.Weekdays = in.EveryMinutes, in.MinuteOfDay, in.Weekdays
	s.Timezone, s.WarnMinutes = tz, in.WarnMinutes
	s.Enabled = in.Enabled == nil || *in.Enabled
	return nil
}

// nextRun uses local wall time: skipped DST times run an hour early, repeated times run once at the first occurrence.
func (s *Schedule) nextRun(after time.Time) time.Time {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		loc = time.UTC // fall back to UTC if a saved zone disappears
	}
	y, m, d := after.In(loc).Date()
	// Search eight days to include next week's run when today's time has passed.
	for day := d; day <= d+7; day++ {
		if s.Weekdays&(1<<time.Date(y, m, day, 12, 0, 0, 0, loc).Weekday()) == 0 {
			continue
		}
		var minutes []int
		if s.EveryMinutes > 0 {
			for at := 0; at < 24*60; at += s.EveryMinutes {
				minutes = append(minutes, at)
			}
		} else {
			minutes = []int{s.MinuteOfDay}
		}
		for _, at := range minutes {
			if t := time.Date(y, m, day, at/60, at%60, 0, 0, loc); t.After(after) {
				return t
			}
		}
	}
	return time.Time{}
}

func normalizeConsoleCommand(raw string) (string, error) {
	command := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "/"))
	if command == "" {
		return "", newError(http.StatusBadRequest, "bad_request", "command is required")
	}
	if len(command) > maxConsoleCommandLen {
		return "", newError(http.StatusBadRequest, "bad_request",
			"command too long (max %d bytes)", maxConsoleCommandLen)
	}
	if strings.IndexFunc(command, func(c rune) bool { return c < 0x20 }) >= 0 {
		return "", newError(http.StatusBadRequest, "bad_request",
			"command must be a single line (no control characters)")
	}
	return command, nil
}
