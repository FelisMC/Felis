package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
)

// The schedule runner. cmd/felis calls RunSchedules every few seconds; each
// call warns the players about the runs coming up, fires the runs that are
// due and moves every run in progress one step on. All of its state is in
// server_schedules, so a felis-api restart picks a restart or backup up where
// it was.
//
// A command, stop or start is one step. A restart stops the server (runStopping)
// and starts it once it is down (runStarting). A backup of a running server
// stops it, takes a backup once the world volume is free (runBackingUp), waits
// for the backup Job and starts the server again; a backup of a stopped server
// leaves it stopped. Each step gives up after its own wait, and a run that took
// the server down tries to bring it back when it gives up.

// Run steps (Schedule.RunState).
const (
	runClaimed   = "claimed"
	runStopping  = "stopping"
	runBackingUp = "backing_up"
	runStarting  = "starting"
)

const (
	// scheduleMissGrace is how late a run may still start: felis-api back from
	// a short restart catches up, and a run hours late is dropped as missed.
	scheduleMissGrace = 10 * time.Minute
	// claimedStale is how long a run may sit in its first step before it is
	// taken for one whose felis-api stopped mid-step. The first step is a few
	// API calls and at most one RCON round trip.
	claimedStale = 2 * time.Minute
	// stopWait is how long a run waits for the server to stop and its world
	// volume to come free. A pod saving a big world takes a while.
	stopWait = 15 * time.Minute
	// backupWait is how long a run waits for its backup Job: past the Job's own
	// deadline ([archive] backup Job deadline, 30m by default).
	backupWait = 45 * time.Minute
	// startWait is how long a run keeps trying to start the server again while
	// the cluster is at its running cap or the world volume is still busy.
	startWait = 15 * time.Minute
	// backupJobSkew is how much earlier than the step the backup Job's creation
	// stamp may read: felis-api's clock and the API server's differ a little.
	backupJobSkew = 2 * time.Minute
	// scheduleWarnHorizon is the longest warning lead time, in minutes.
	scheduleWarnHorizon = 30
)

// Audit identity of a run. Each run leaves one schedule.run row when it ends.
const (
	scheduleActor     = "scheduler"
	scheduleRunAction = "schedule.run"
)

// RunSchedules warns about, fires and advances the schedules once. A schedule
// that fails is logged and left for the next call; it never holds up the rest.
func (a *API) RunSchedules(ctx context.Context) error {
	if a.Schedules == nil {
		return nil
	}
	now := a.now()
	due, err := a.Schedules.DueSchedules(ctx, now.Add(scheduleWarnHorizon*time.Minute))
	if err != nil {
		return fmt.Errorf("list the due schedules: %w", err)
	}
	for i := range due {
		d := &due[i]
		if err := a.tickSchedule(ctx, d, now); err != nil {
			log.Printf("api: schedule %d of %s: %v", d.ID, d.Server, err)
		}
	}
	return nil
}

// tickSchedule does what one due schedule needs now.
func (a *API) tickSchedule(ctx context.Context, d *DueSchedule, now time.Time) error {
	s := &d.Schedule
	if s.RunState != "" {
		return a.advanceRun(ctx, s, now)
	}
	// Somebody else's server now: their commands must not run on it. Saving
	// the schedule again (PUT) hands it to the new owner.
	if d.ServerOwner != s.OwnerID {
		_, err := a.Schedules.DisableSchedule(ctx, s.ID,
			"the server has a new owner since this schedule was saved; save it again to use it")
		return err
	}
	due := *s.NextRunAt // set on every enabled schedule DueSchedules returns idle
	if now.Before(due) {
		return a.warnRun(ctx, s, due, now)
	}
	next := s.nextRun(now)
	if now.Sub(due) > scheduleMissGrace {
		_, err := a.Schedules.MissScheduleRun(ctx, s.ID, due, next,
			"felis-api was not running at the scheduled time")
		return err
	}
	ok, err := a.Schedules.ClaimScheduleRun(ctx, s.ID, &due, &next, now)
	if err != nil || !ok {
		return err
	}
	return a.beginRun(ctx, s)
}

// warnRun tells the players on the server that a restart, stop or backup is
// coming, once per run, when its warning time has come.
func (a *API) warnRun(ctx context.Context, s *Schedule, due, now time.Time) error {
	lead := time.Duration(s.WarnMinutes) * time.Minute
	if now.Before(due.Add(-lead)) || a.Console == nil {
		return nil // no warning, or not yet
	}
	info, err := a.Cluster.GetServer(ctx, s.Server)
	if err != nil {
		return err
	}
	if !info.Ready {
		return nil
	}
	ok, err := a.Schedules.WarnScheduleRun(ctx, s.ID, due)
	if err != nil || !ok {
		return err
	}
	// Warned late (felis-api was restarting at the warning time): say how long
	// is really left.
	minutes := int(math.Ceil(due.Sub(now).Minutes()))
	if _, err := a.Console.RunCommand(ctx, s.Server, "say "+scheduleWarning(s.Action, minutes)); err != nil {
		return fmt.Errorf("warn the players: %w", err)
	}
	return nil
}

// scheduleWarning is the in-game line announcing a run minutes ahead, in both
// panel languages.
func scheduleWarning(action string, minutes int) string {
	switch action {
	case ScheduleRestart:
		return fmt.Sprintf("[Felis] 服务器将在 %d 分钟后重启 / Server restarts in %d min", minutes, minutes)
	case ScheduleStop:
		return fmt.Sprintf("[Felis] 服务器将在 %d 分钟后关闭 / Server stops in %d min", minutes, minutes)
	default:
		return fmt.Sprintf("[Felis] 服务器将在 %d 分钟后暂停做备份，完成后自动恢复 / Server pauses for a backup in %d min and comes back after", minutes, minutes)
	}
}

// beginRun takes the first step of a run just claimed (runClaimed).
func (a *API) beginRun(ctx context.Context, s *Schedule) error {
	finish := func(result, detail string) error { return a.finishRun(ctx, s, runClaimed, result, detail) }
	info, err := a.Cluster.GetServer(ctx, s.Server)
	if errors.Is(err, ErrNotFound) {
		return finish(ScheduleSkipped, "the server no longer exists")
	}
	if err != nil {
		return finish(ScheduleFailed, "could not read the server: "+err.Error())
	}
	running := info.DesiredState == string(v1alpha1.DesiredRunning)

	switch s.Action {
	case ScheduleCommand:
		if !info.Ready {
			return finish(ScheduleSkipped, "the server was not running")
		}
		if a.Console == nil {
			return finish(ScheduleFailed, "the console is not configured")
		}
		out, err := a.Console.RunCommand(ctx, s.Server, s.Command)
		if errors.Is(err, ErrConsoleUnavailable) {
			return finish(ScheduleFailed, "the server console could not be reached")
		}
		if err != nil {
			return finish(ScheduleFailed, "the command failed: "+err.Error())
		}
		return finish(ScheduleOK, stripFormatting(out))

	case ScheduleStop:
		if !running {
			return finish(ScheduleSkipped, "the server was already stopped")
		}
		if err := a.Cluster.SetDesiredState(ctx, s.Server, v1alpha1.DesiredStopped); err != nil {
			return finish(ScheduleFailed, "could not stop the server: "+err.Error())
		}
		return finish(ScheduleOK, "")

	case ScheduleStart:
		if running && info.Phase != string(v1alpha1.PhaseFailed) {
			return finish(ScheduleSkipped, "the server was already running")
		}
		why, _, err := a.startScheduled(ctx, s.Server)
		if err != nil {
			return finish(ScheduleFailed, "could not start the server: "+err.Error())
		}
		if why != "" {
			return finish(ScheduleSkipped, why)
		}
		return finish(ScheduleOK, "")

	case ScheduleRestart:
		if !running {
			return finish(ScheduleSkipped, "the server was not running")
		}
		return a.stopForRun(ctx, s, true)

	case ScheduleBackup:
		if _, ok := a.Backuper.(ScheduledBackuper); !ok {
			return finish(ScheduleFailed, "backups are not configured")
		}
		if limit := a.BackupStoreCap; limit > 0 {
			used, err := a.Repo.BackupStoreBytes(ctx)
			if err != nil {
				return finish(ScheduleFailed, "could not read the backup store size: "+err.Error())
			}
			if used >= limit {
				return finish(ScheduleFailed, "the backup store is full; ask an administrator to free space")
			}
		}
		exists, err := a.Cluster.WorldVolumeExists(ctx, s.Server)
		if err != nil {
			return finish(ScheduleFailed, "could not look up the world volume: "+err.Error())
		}
		if !exists {
			return finish(ScheduleSkipped, "the server has no world yet")
		}
		if running {
			return a.stopForRun(ctx, s, true)
		}
		// Already stopped: back it up as soon as the world volume is free, and
		// leave it stopped afterwards.
		if ok, err := a.Schedules.AdvanceScheduleRun(ctx, s.ID, runClaimed, runStopping, false, "", "", a.now()); err != nil || !ok {
			return err
		}
		s.RunState, s.RunResume, s.RunStepAt = runStopping, false, ptrTime(a.now())
		return a.advanceRun(ctx, s, a.now())
	}
	return finish(ScheduleFailed, "unknown action "+s.Action)
}

// stopForRun records the stop step and then stops the server, in that order: a
// felis-api that dies between the two leaves a running server in runStopping,
// which the next call reads as someone having started it, and nothing is lost.
func (a *API) stopForRun(ctx context.Context, s *Schedule, resume bool) error {
	now := a.now()
	if ok, err := a.Schedules.AdvanceScheduleRun(ctx, s.ID, runClaimed, runStopping, resume, "", "", now); err != nil || !ok {
		return err
	}
	if err := a.Cluster.SetDesiredState(ctx, s.Server, v1alpha1.DesiredStopped); err != nil {
		return a.finishRun(ctx, s, runStopping, ScheduleFailed, "could not stop the server: "+err.Error())
	}
	return nil
}

// advanceRun moves a run in progress on by one step, or leaves it waiting.
func (a *API) advanceRun(ctx context.Context, s *Schedule, now time.Time) error {
	waited := now.Sub(*s.RunStepAt)
	switch s.RunState {
	case runClaimed:
		if waited < claimedStale {
			return nil // its first step is running right now
		}
		return a.finishRun(ctx, s, runClaimed, ScheduleFailed, "felis-api stopped in the middle of this run")
	case runStopping:
		return a.advanceStopping(ctx, s, waited)
	case runBackingUp:
		return a.advanceBackingUp(ctx, s, waited)
	case runStarting:
		return a.advanceStarting(ctx, s, waited)
	}
	return a.finishRun(ctx, s, s.RunState, ScheduleFailed, "unknown run step "+s.RunState)
}

// advanceStopping waits for the server to go down. A restart then starts it;
// a backup takes the world volume and starts the backup Job.
func (a *API) advanceStopping(ctx context.Context, s *Schedule, waited time.Duration) error {
	info, err := a.Cluster.GetServer(ctx, s.Server)
	if errors.Is(err, ErrNotFound) {
		return a.finishRun(ctx, s, runStopping, ScheduleSkipped, "the server no longer exists")
	}
	if err != nil {
		return err
	}
	// Only this run stops the server; wanted running again means a person (or
	// a player's join) started it meanwhile, and their start stands.
	if info.DesiredState == string(v1alpha1.DesiredRunning) {
		return a.finishRun(ctx, s, runStopping, ScheduleSkipped, "someone started the server before the run finished")
	}
	giveUp := func(detail string) error {
		if waited < stopWait {
			return nil
		}
		if s.RunResume {
			if err := a.Cluster.SetDesiredState(ctx, s.Server, v1alpha1.DesiredRunning); err == nil {
				detail += "; it was started again"
			}
		}
		return a.finishRun(ctx, s, runStopping, ScheduleFailed, detail)
	}

	if s.Action == ScheduleRestart {
		if info.Phase != string(v1alpha1.PhaseStopped) {
			return giveUp("the server did not stop within 15 minutes")
		}
		if ok, err := a.Schedules.AdvanceScheduleRun(ctx, s.ID, runStopping, runStarting, s.RunResume, "", "", a.now()); err != nil || !ok {
			return err
		}
		s.RunState, s.RunStepAt = runStarting, ptrTime(a.now())
		return a.advanceStarting(ctx, s, 0)
	}

	b, ok := a.Backuper.(ScheduledBackuper)
	if !ok {
		return a.finishRun(ctx, s, runStopping, ScheduleFailed, "backups are not configured")
	}
	switch err := a.Cluster.AcquireMaintenance(ctx, s.Server, maintenance.KindBackup); {
	case errors.Is(err, ErrNotStopped):
		return giveUp("the server did not stop within 15 minutes")
	case errors.Is(err, ErrMaintenanceInProgress):
		return giveUp("another operation kept the world busy for 15 minutes")
	case errors.Is(err, ErrNotFound):
		return a.finishRun(ctx, s, runStopping, ScheduleSkipped, "the server no longer exists")
	case err != nil:
		return err
	}
	err = b.BackupScheduled(ctx, s.Server, s.OwnerID)
	// Once the Job exists it holds the world; the lock only covered the gap.
	if rerr := a.Cluster.ReleaseMaintenance(context.WithoutCancel(ctx), s.Server); rerr != nil {
		log.Printf("api: release the maintenance lock on %s: %v (it lapses after %s)", s.Server, rerr, maintenance.Grace)
	}
	if err != nil {
		detail := "could not start the backup: " + err.Error()
		if s.RunResume {
			if serr := a.Cluster.SetDesiredState(ctx, s.Server, v1alpha1.DesiredRunning); serr == nil {
				detail += "; the server was started again"
			}
		}
		return a.finishRun(ctx, s, runStopping, ScheduleFailed, detail)
	}
	log.Printf("api: schedule %d started a backup of %s", s.ID, s.Server)
	_, err = a.Schedules.AdvanceScheduleRun(ctx, s.ID, runStopping, runBackingUp, s.RunResume, "", "", a.now())
	return err
}

// advanceBackingUp waits for the backup Job and records how it ended; a run
// that stopped a running server then starts it again.
func (a *API) advanceBackingUp(ctx context.Context, s *Schedule, waited time.Duration) error {
	result, detail := ScheduleOK, ""
	if a.JobStatus != nil {
		jobs, err := a.JobStatus.LatestJobs(ctx, s.Server)
		if err != nil {
			return err
		}
		var job *AsyncJob
		for i := range jobs {
			j := &jobs[i]
			if j.Scheduled && !j.StartedAt.Before(s.RunStepAt.Add(-backupJobSkew)) {
				job = j
				break // newest first
			}
		}
		switch {
		case job == nil && waited < backupJobSkew:
			return nil // not listed yet
		case job == nil:
			result, detail = ScheduleFailed, "the backup Job is gone before it could be checked; see the Backups page"
		case job.State == "running" && waited < backupWait:
			return nil
		case job.State == "running":
			result, detail = ScheduleFailed, "the backup did not finish within 45 minutes"
		case job.State == "failed":
			result, detail = ScheduleFailed, strings.TrimSpace("the backup failed: "+job.Message)
		}
	}
	if !s.RunResume {
		return a.finishRun(ctx, s, runBackingUp, result, detail)
	}
	if ok, err := a.Schedules.AdvanceScheduleRun(ctx, s.ID, runBackingUp, runStarting, true, result, detail, a.now()); err != nil || !ok {
		return err
	}
	s.RunState, s.RunStepAt, s.LastResult, s.LastDetail = runStarting, ptrTime(a.now()), result, detail
	return a.advanceStarting(ctx, s, 0)
}

// advanceStarting brings the server back up after a restart's stop or a
// backup. The outcome recorded so far (the backup's) is kept when it starts.
func (a *API) advanceStarting(ctx context.Context, s *Schedule, waited time.Duration) error {
	why, retry, err := a.startScheduled(ctx, s.Server)
	if errors.Is(err, ErrNotFound) {
		return a.finishRun(ctx, s, runStarting, ScheduleSkipped, "the server no longer exists")
	}
	if err != nil {
		return err
	}
	if why == "" {
		result, detail := s.LastResult, s.LastDetail
		if result == "" {
			result = ScheduleOK
		}
		return a.finishRun(ctx, s, runStarting, result, detail)
	}
	if retry && waited < startWait {
		return nil
	}
	detail := "could not start the server again: " + why
	if s.LastResult == ScheduleFailed {
		detail = s.LastDetail + "; " + detail
	}
	return a.finishRun(ctx, s, runStarting, ScheduleFailed, detail)
}

// startScheduled starts a server for a run: the wake path without the
// per-player cooldown. why names what kept it stopped ("" once it is wanted
// running), and retry says whether that may clear by itself.
func (a *API) startScheduled(ctx context.Context, name string) (why string, retry bool, err error) {
	rec, err := a.Repo.ServerByName(ctx, name)
	if err != nil {
		return "", false, err
	}
	if rec.Retire != nil {
		return "the server is being given up or deleted", false, nil
	}
	info, err := a.Cluster.GetServer(ctx, name)
	if err != nil {
		return "", false, err
	}
	ok, err := a.withinRunningCap(ctx, info)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "the cluster is at its running-server cap", true, nil
	}
	// A start that Failed is started over, as a person's start does (handleStart).
	if info.Phase == string(v1alpha1.PhaseFailed) && info.DesiredState == string(v1alpha1.DesiredRunning) {
		err = a.Cluster.RetryStart(ctx, name)
	} else {
		err = a.Cluster.SetDesiredState(ctx, name, v1alpha1.DesiredRunning)
	}
	var busy *MaintenanceBusyError
	if errors.As(err, &busy) {
		return "the world is busy with " + maintenanceLabel(busy.Kind), true, nil
	}
	return "", false, err
}

// finishRun ends a run at step from and audits it.
func (a *API) finishRun(ctx context.Context, s *Schedule, from, result, detail string) error {
	detail = truncateUTF8(detail, maxScheduleDetail)
	ok, err := a.Schedules.FinishScheduleRun(ctx, s.ID, from, result, detail)
	if err != nil || !ok {
		return err
	}
	a.writeAudit(ctx, AuditEntry{
		Actor: scheduleActor, Source: scheduleActor, Action: scheduleRunAction, ServerName: s.Server,
		Payload: auditPayload(map[string]any{"schedule": s.ID, "action": s.Action, "result": result, "detail": detail}),
	})
	return nil
}

// stripFormatting drops Minecraft's section-sign formatting codes from a
// console reply and trims it.
func stripFormatting(s string) string {
	var b strings.Builder
	skip := false
	for _, c := range s {
		switch {
		case skip:
			skip = false
		case c == '§':
			skip = true
		default:
			b.WriteRune(c)
		}
	}
	return strings.TrimSpace(b.String())
}

func ptrTime(t time.Time) *time.Time { return &t }
