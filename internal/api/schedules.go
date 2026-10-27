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

// Step types, in the order the panel lists them.
const (
	StepCommand = "command"
	StepDelay   = "delay"
	StepStart   = "start"
	StepStop    = "stop"
	StepRestart = "restart"
	StepKill    = "kill"
	StepBackup  = "backup"
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
	maxScheduleSteps     = 10
	maxDelaySeconds      = 3600
	maxScheduleRuns      = 10000
)

var (
	// scheduleEveryMinutes divides a day so runs keep the same local clock times each day.
	scheduleEveryMinutes = []int{1, 5, 10, 15, 30, 60, 120, 180, 240, 360, 480, 720}
	scheduleWarnMinutes  = []int{0, 1, 5, 10, 15, 30}
)

var (
	ErrScheduleLimit   = errors.New("schedule limit reached")
	ErrScheduleRunning = errors.New("schedule is running")
)

type ScheduleStep struct {
	Type    string `json:"type"`
	Command string `json:"command,omitempty"`
	Seconds int    `json:"seconds,omitempty"`
}

type Schedule struct {
	ID     int64          `json:"id"`
	Server string         `json:"server"`
	Label  string         `json:"label"`
	Steps  []ScheduleStep `json:"steps"`
	// EveryMinutes counts from local midnight; zero uses MinuteOfDay, and Weekdays is a bitmask with bit 0 for Sunday.
	EveryMinutes int    `json:"every_minutes"`
	MinuteOfDay  int    `json:"minute_of_day"`
	Weekdays     int    `json:"weekdays"`
	Timezone     string `json:"timezone"`
	// RunOnceAt makes a one-time schedule that ignores the repeat fields.
	RunOnceAt *time.Time `json:"run_once_at"`
	// MaxRuns=0 repeats without limit; RunCount counts claimed runs, manual ones included.
	MaxRuns  int `json:"max_runs"`
	RunCount int `json:"run_count"`
	// WarnMinutes=0 disables the in-game warning.
	WarnMinutes int  `json:"warn_minutes"`
	Enabled     bool `json:"enabled"`
	// NextRunAt is nil while disabled.
	NextRunAt *time.Time `json:"next_run_at"`
	// RunState is the phase of step RunStep, empty when idle.
	RunState   string     `json:"run_state"`
	RunStep    int        `json:"run_step"`
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

// RunPos locates a run: the step index and the phase within that step.
type RunPos struct {
	Step  int
	Phase string
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
	// ClaimScheduleRun counts the run and advances next only for a due run; nil due starts a manual run.
	// A one-time schedule or one reaching MaxRuns gets no next run.
	ClaimScheduleRun(ctx context.Context, id int64, due, next *time.Time, now time.Time) (bool, error)
	// AdvanceScheduleRun requires the run to be at from and records resume and any nonempty result.
	AdvanceScheduleRun(ctx context.Context, id int64, from, to RunPos, resume bool, result, detail string, now time.Time) (bool, error)
	// FinishScheduleRun applies only at from; it disables a used-up schedule and moves a passed next run to next.
	FinishScheduleRun(ctx context.Context, id int64, from RunPos, result, detail string, next *time.Time, now time.Time) (bool, error)
	// MissScheduleRun records the expected due run and advances to next, disabling the schedule when next is nil.
	MissScheduleRun(ctx context.Context, id int64, due time.Time, next *time.Time, detail string) (bool, error)
	// WarnScheduleRun marks each due run at most once.
	WarnScheduleRun(ctx context.Context, id int64, due time.Time) (bool, error)
	// DisableSchedule applies only when enabled and idle, recording the reason.
	DisableSchedule(ctx context.Context, id int64, detail string) (bool, error)
}

// scheduleInput defaults Enabled to true.
type scheduleInput struct {
	Label        string         `json:"label"`
	Steps        []ScheduleStep `json:"steps"`
	EveryMinutes int            `json:"every_minutes"`
	MinuteOfDay  int            `json:"minute_of_day"`
	Weekdays     int            `json:"weekdays"`
	Timezone     string         `json:"timezone"`
	RunOnceAt    *time.Time     `json:"run_once_at"`
	MaxRuns      int            `json:"max_runs"`
	WarnMinutes  int            `json:"warn_minutes"`
	Enabled      *bool          `json:"enabled"`
}

func badSchedule(format string, a ...any) error {
	return newError(http.StatusBadRequest, "bad_schedule", format, a...)
}

func parseSteps(in []ScheduleStep) ([]ScheduleStep, error) {
	if len(in) == 0 || len(in) > maxScheduleSteps {
		return nil, badSchedule("a schedule has 1 to %d steps", maxScheduleSteps)
	}
	out := make([]ScheduleStep, 0, len(in))
	acts, backups := 0, 0
	for i, st := range in {
		n := i + 1
		switch st.Type {
		case StepCommand:
			c, err := normalizeConsoleCommand(st.Command)
			if err != nil {
				return nil, err
			}
			st.Command = c
		case StepDelay:
			if st.Seconds < 1 || st.Seconds > maxDelaySeconds {
				return nil, badSchedule("step %d waits 1 to %d seconds", n, maxDelaySeconds)
			}
		case StepStart, StepStop, StepRestart, StepKill, StepBackup:
		default:
			return nil, badSchedule("step %d must be one of command, delay, start, stop, restart, kill, backup", n)
		}
		if st.Type != StepCommand && strings.TrimSpace(st.Command) != "" {
			return nil, badSchedule("step %d: only a command step takes a command", n)
		}
		if st.Type != StepDelay && st.Seconds != 0 {
			return nil, badSchedule("step %d: only a delay step takes seconds", n)
		}
		if st.Type != StepDelay {
			acts++
		}
		if st.Type == StepBackup {
			backups++
		}
		out = append(out, ScheduleStep{Type: st.Type, Command: st.Command, Seconds: st.Seconds})
	}
	if acts == 0 {
		return nil, badSchedule("a schedule needs a step that is not a delay")
	}
	if backups > 1 {
		return nil, badSchedule("a schedule has at most one backup step")
	}
	return out, nil
}

func (in *scheduleInput) apply(s *Schedule, now time.Time) error {
	label := strings.TrimSpace(in.Label)
	if utf8.RuneCountInString(label) > maxScheduleLabel {
		return badSchedule("label too long (max %d characters)", maxScheduleLabel)
	}
	if strings.IndexFunc(label, func(c rune) bool { return c < 0x20 || c == 0x7f }) >= 0 {
		return badSchedule("label must be a single line")
	}
	steps, err := parseSteps(in.Steps)
	if err != nil {
		return err
	}
	probe := Schedule{Steps: steps}
	enabled := in.Enabled == nil || *in.Enabled

	minuteOfDay, weekdays := in.MinuteOfDay, in.Weekdays
	if in.RunOnceAt != nil {
		switch {
		case in.EveryMinutes != 0:
			return badSchedule("a one-time schedule does not repeat")
		case in.MaxRuns != 0:
			return badSchedule("a one-time schedule runs once; leave max_runs at 0")
		case enabled && !in.RunOnceAt.After(now):
			return badSchedule("run_once_at must be in the future")
		}
		minuteOfDay, weekdays = 0, 0x7f
	}
	switch {
	case in.EveryMinutes == 0:
		if minuteOfDay < 0 || minuteOfDay > 24*60-1 {
			return badSchedule("minute_of_day must be between 0 and 1439")
		}
	case !slices.Contains(scheduleEveryMinutes, in.EveryMinutes):
		return badSchedule("every_minutes must be 0 or one of 1, 5, 10, 15, 30, 60, 120, 180, 240, 360, 480, 720")
	case in.EveryMinutes < minPowerEveryMinutes && probe.powers():
		return badSchedule("a schedule that starts, stops or backs up repeats at most every %d minutes", minPowerEveryMinutes)
	case minuteOfDay != 0:
		return badSchedule("a repeating schedule has no minute_of_day")
	}
	if weekdays < 1 || weekdays > 0x7f {
		return badSchedule("weekdays must name at least one day (bits 0 to 6, Sunday first)")
	}
	if in.MaxRuns < 0 || in.MaxRuns > maxScheduleRuns {
		return badSchedule("max_runs must be between 0 and %d", maxScheduleRuns)
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
	if in.WarnMinutes > 0 && probe.warnStep() == "" {
		return badSchedule("only a schedule that stops, restarts or backs up the server warns the players")
	}

	s.Label, s.Steps = label, steps
	s.EveryMinutes, s.MinuteOfDay, s.Weekdays = in.EveryMinutes, minuteOfDay, weekdays
	s.Timezone, s.WarnMinutes, s.MaxRuns = tz, in.WarnMinutes, in.MaxRuns
	s.RunOnceAt = nil
	if in.RunOnceAt != nil {
		s.RunOnceAt = ptrTime(in.RunOnceAt.UTC())
	}
	s.Enabled = enabled
	return nil
}

// powers reports whether a step starts, stops or backs up the server.
func (s *Schedule) powers() bool {
	return slices.ContainsFunc(s.Steps, func(st ScheduleStep) bool {
		return st.Type != StepCommand && st.Type != StepDelay
	})
}

// warnStep is the first step that takes the server down, which words the in-game warning.
func (s *Schedule) warnStep() string {
	for _, st := range s.Steps {
		switch st.Type {
		case StepStop, StepRestart, StepKill, StepBackup:
			return st.Type
		}
	}
	return ""
}

// upcoming is the next run after a moment, or nil when the schedule has none left.
func (s *Schedule) upcoming(after time.Time) *time.Time {
	if t := s.nextRun(after); !t.IsZero() {
		return &t
	}
	return nil
}

// nextRun uses local wall time: skipped DST times run an hour early, repeated times run once at the first occurrence.
func (s *Schedule) nextRun(after time.Time) time.Time {
	if s.RunOnceAt != nil {
		if s.RunOnceAt.After(after) {
			return *s.RunOnceAt
		}
		return time.Time{}
	}
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
