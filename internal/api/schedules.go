package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
	_ "time/tzdata" // a schedule's timezone resolves on a host without zoneinfo
	"unicode/utf8"
)

// Scheduled tasks. An owner or admin asks felis-api to act on a server at set
// times: run a console command, restart it, stop it, start it, or back it up.
// A backup of a running server stops it, takes the backup and starts it again,
// so a world kept up around the clock still gets restore points; the
// BackupScheduler only ever backs up a stopped world. The schedules live in
// server_schedules (migration 0036), and RunSchedules (schedulerunner.go) is
// the loop that fires them.

// Schedule actions.
const (
	ScheduleCommand = "command"
	ScheduleRestart = "restart"
	ScheduleStop    = "stop"
	ScheduleStart   = "start"
	ScheduleBackup  = "backup"
)

// Run results (Schedule.LastResult). A run in progress has none yet.
const (
	ScheduleOK      = "ok"
	ScheduleSkipped = "skipped"
	ScheduleFailed  = "failed"
	// ScheduleMissed is a run felis-api was not up for: it is dropped once it
	// is scheduleMissGrace late, so a restart never lands hours after its time.
	ScheduleMissed = "missed"
)

const (
	// maxSchedulesPerServer bounds the rows one server can hold.
	maxSchedulesPerServer = 20
	// maxScheduleLabel bounds a label, in characters.
	maxScheduleLabel = 64
	// maxScheduleDetail bounds the stored outcome of a run (a command's reply
	// can be long), in bytes.
	maxScheduleDetail = 500
	// minPowerEveryMinutes is the shortest interval of an action that takes the
	// server down; a command may run every scheduleEveryMinutes[0].
	minPowerEveryMinutes = 60
)

var (
	// scheduleEveryMinutes are the intervals a repeating schedule may use: each
	// divides a day, so the runs sit at the same clock times every day.
	scheduleEveryMinutes = []int{15, 30, 60, 120, 180, 240, 360, 480, 720}
	// scheduleWarnMinutes are the lead times of the in-game warning.
	scheduleWarnMinutes = []int{0, 1, 5, 10, 15, 30}
)

var (
	// ErrScheduleLimit means the server already holds maxSchedulesPerServer.
	ErrScheduleLimit = errors.New("schedule limit reached")
	// ErrScheduleRunning means the schedule has a run in progress, which must
	// finish before the schedule is changed, deleted or run again.
	ErrScheduleRunning = errors.New("schedule is running")
)

// Schedule is one server_schedules row as the owner sees it. OwnerID, the
// warning marker and the run step stay off the wire; RunState tells the panel
// what a run in progress is waiting for.
type Schedule struct {
	ID     int64  `json:"id"`
	Server string `json:"server"`
	Label  string `json:"label"`
	Action string `json:"action"`
	// Command is the console command of a command schedule, without a slash.
	Command string `json:"command"`
	// EveryMinutes repeats the schedule at every multiple of it since local
	// midnight; 0 runs it once a day at MinuteOfDay. Either way only on the
	// Weekdays (a bitmask, bit 0 Sunday), in Timezone.
	EveryMinutes int    `json:"every_minutes"`
	MinuteOfDay  int    `json:"minute_of_day"`
	Weekdays     int    `json:"weekdays"`
	Timezone     string `json:"timezone"`
	// WarnMinutes is how long before a restart, stop or backup the players on
	// the server are told it is coming; 0 says nothing.
	WarnMinutes int  `json:"warn_minutes"`
	Enabled     bool `json:"enabled"`
	// NextRunAt is nil while the schedule is disabled.
	NextRunAt *time.Time `json:"next_run_at"`
	// RunState is the step of a run in progress (runStopping, runBackingUp,
	// runStarting, or runClaimed while its first step runs); empty otherwise.
	RunState   string     `json:"run_state"`
	LastRunAt  *time.Time `json:"last_run_at"`
	LastResult string     `json:"last_result"`
	LastDetail string     `json:"last_detail"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`

	// OwnerID is the server's owner when the schedule was saved (empty for an
	// unowned server). The runner fires the schedule only while it still is.
	OwnerID   string     `json:"-"`
	WarnedFor *time.Time `json:"-"`
	RunResume bool       `json:"-"`
	RunStepAt *time.Time `json:"-"`
}

// DueSchedule is a schedule the runner has to look at, with the server's
// current owner.
type DueSchedule struct {
	Schedule
	ServerOwner string
}

// ServerSchedules stores the schedules (PGRepo). The run methods are
// compare-and-set writes that report whether they applied, so two felis-api
// processes side by side during a rollout never fire one run twice.
type ServerSchedules interface {
	// ListSchedules lists a server's schedules, oldest first.
	ListSchedules(ctx context.Context, server string) ([]Schedule, error)
	// GetSchedule reads one schedule of server, or ErrNotFound.
	GetSchedule(ctx context.Context, server string, id int64) (*Schedule, error)
	// CreateSchedule inserts s and fills in its ID and CreatedAt, or returns
	// ErrScheduleLimit when the server already holds limit schedules.
	CreateSchedule(ctx context.Context, s *Schedule, limit int) error
	// UpdateSchedule writes s's settings, owner and next run and clears its
	// warning marker: ErrNotFound, or ErrScheduleRunning during a run.
	UpdateSchedule(ctx context.Context, s *Schedule) error
	// DeleteSchedule removes a schedule: ErrNotFound, or ErrScheduleRunning
	// during a run.
	DeleteSchedule(ctx context.Context, server string, id int64) error

	// DueSchedules lists the enabled schedules of live servers due by horizon
	// and every schedule with a run in progress.
	DueSchedules(ctx context.Context, horizon time.Time) ([]DueSchedule, error)
	// ClaimScheduleRun starts a run (run state runClaimed, a fresh result) of
	// a schedule without one. With due set it claims only the enabled run due
	// then and moves the schedule on to next; nil due is a run on request that
	// leaves the next run where it is.
	ClaimScheduleRun(ctx context.Context, id int64, due, next *time.Time, now time.Time) (bool, error)
	// AdvanceScheduleRun moves a run from step from to step to, recording
	// resume and, when result is set, the outcome so far.
	AdvanceScheduleRun(ctx context.Context, id int64, from, to string, resume bool, result, detail string, now time.Time) (bool, error)
	// FinishScheduleRun ends a run at step from with its outcome.
	FinishScheduleRun(ctx context.Context, id int64, from, result, detail string) (bool, error)
	// MissScheduleRun records the run due then as missed and moves on to next.
	MissScheduleRun(ctx context.Context, id int64, due, next time.Time, detail string) (bool, error)
	// WarnScheduleRun marks the run due then as warned about, once.
	WarnScheduleRun(ctx context.Context, id int64, due time.Time) (bool, error)
	// DisableSchedule turns off an enabled schedule without a run in progress
	// and records why.
	DisableSchedule(ctx context.Context, id int64, detail string) (bool, error)
}

// scheduleInput is the body of POST /servers/{name}/schedules and of PUT
// /servers/{name}/schedules/{id}. Enabled defaults to true.
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

// apply validates in and writes it onto s.
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
	// A restart, stop or backup repeats at most hourly, so its warning (30
	// minutes ahead at most) always comes after the run before it.
	if in.WarnMinutes > 0 && (in.Action == ScheduleCommand || in.Action == ScheduleStart) {
		return badSchedule("only a restart, stop or backup warns the players")
	}

	s.Label, s.Action, s.Command = label, in.Action, command
	s.EveryMinutes, s.MinuteOfDay, s.Weekdays = in.EveryMinutes, in.MinuteOfDay, in.Weekdays
	s.Timezone, s.WarnMinutes = tz, in.WarnMinutes
	s.Enabled = in.Enabled == nil || *in.Enabled
	return nil
}

// nextRun is the first run of s strictly after after, or the zero time for a
// schedule that names no weekday. The runs are wall-clock times in s's zone:
// a time the zone skips at the start of daylight saving runs an hour early
// (time.Date reads it with the offset before the jump), and one it repeats
// runs once, the first time the clock shows it.
func (s *Schedule) nextRun(after time.Time) time.Time {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		loc = time.UTC // validated when saved; a zone later dropped runs in UTC
	}
	y, m, d := after.In(loc).Date()
	// Eight days: today's runs may all be past, and a schedule of one weekday
	// next runs a week from today.
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

// normalizeConsoleCommand makes raw one console command: surrounding space
// and a single leading slash removed (players type "/say hi"; RCON wants
// "say hi"), within maxConsoleCommandLen, and without control characters, so
// one request can never smuggle a second command past a newline.
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
