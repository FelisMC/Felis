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
	"felis.lolicon.best/internal/backupjob"
	"felis.lolicon.best/internal/maintenance"
)

// Run positions are persisted so API restarts resume; a failed run starts again a server it took down.

const (
	phaseClaimed        = "claimed"
	phaseDelaying       = "delaying"
	phaseStopping       = "stopping"
	phaseBackupStopping = "backup_stopping"
	phaseBackupWaiting  = "backup_waiting"
	phaseBackingUp      = "backing_up"
	phaseBackupRestart  = "backup_restart"
	phaseStarting       = "starting"
	phaseSettling       = "settling"
)

const (
	scheduleMissGrace = 10 * time.Minute
	claimedStale      = 2 * time.Minute
	stopWait          = 15 * time.Minute
	// backupWait must exceed the backup Job deadline.
	backupWait = 45 * time.Minute
	startWait  = 15 * time.Minute
	// backupJobSkew tolerates clock differences between the API and Kubernetes.
	backupJobSkew       = 2 * time.Minute
	scheduleWarnHorizon = 30
)

const (
	scheduleActor     = "scheduler"
	scheduleRunAction = "schedule.run"
)

// RunSchedules isolates failures so one schedule does not block others.
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

func (a *API) tickSchedule(ctx context.Context, d *DueSchedule, now time.Time) error {
	s := &d.Schedule
	if s.RunState != "" {
		return a.advanceRun(ctx, s, now)
	}
	// Ownership changed; disable old commands until the new owner saves the schedule.
	if d.ServerOwner != s.OwnerID {
		_, err := a.Schedules.DisableSchedule(ctx, s.ID,
			"the server has a new owner since this schedule was saved; save it again to use it")
		return err
	}
	due := *s.NextRunAt
	if now.Before(due) {
		return a.warnRun(ctx, s, due, now)
	}
	next := s.upcoming(now)
	if now.Sub(due) > scheduleMissGrace {
		_, err := a.Schedules.MissScheduleRun(ctx, s.ID, due, next,
			"felis-api was not running at the scheduled time")
		return err
	}
	ok, err := a.Schedules.ClaimScheduleRun(ctx, s.ID, &due, next, now)
	if err != nil || !ok {
		return err
	}
	markClaimed(s, now)
	return a.beginStep(ctx, s)
}

// markClaimed mirrors a successful ClaimScheduleRun in memory.
func markClaimed(s *Schedule, now time.Time) {
	s.RunStep, s.RunState, s.RunResume, s.RunStepAt = 0, phaseClaimed, false, ptrTime(now)
	s.RunCount++
	s.LastResult, s.LastDetail = "", ""
}

func (a *API) warnRun(ctx context.Context, s *Schedule, due, now time.Time) error {
	lead := time.Duration(s.WarnMinutes) * time.Minute
	if now.Before(due.Add(-lead)) || a.Console == nil {
		return nil
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
	// A late warning reports the remaining time instead of the configured lead time.
	minutes := int(math.Ceil(due.Sub(now).Minutes()))
	_, _, live := a.liveBackupParts()
	if _, err := a.Console.RunCommand(ctx, s.Server, "say "+scheduleWarning(s.warnStep(), minutes, live)); err != nil {
		return fmt.Errorf("warn the players: %w", err)
	}
	return nil
}

func scheduleWarning(step string, minutes int, live bool) string {
	switch step {
	case StepRestart:
		return fmt.Sprintf("[Felis] 服务器将在 %d 分钟后重启 / Server restarts in %d min", minutes, minutes)
	case StepStop, StepKill:
		return fmt.Sprintf("[Felis] 服务器将在 %d 分钟后关闭 / Server stops in %d min", minutes, minutes)
	case StepBackup:
		if live {
			return fmt.Sprintf("[Felis] %d 分钟后进行备份，存档会暂停几秒钟 / A backup runs in %d min; saving pauses for a few seconds", minutes, minutes)
		}
		fallthrough
	default:
		return fmt.Sprintf("[Felis] 服务器将在 %d 分钟后暂停做备份，完成后自动恢复 / Server pauses for a backup in %d min and comes back after", minutes, minutes)
	}
}

// beginStep runs step s.RunStep from its claimed phase.
func (a *API) beginStep(ctx context.Context, s *Schedule) error {
	pos := RunPos{s.RunStep, phaseClaimed}
	step := s.Steps[s.RunStep]
	if step.Type == StepDelay {
		return a.moveRun(ctx, s, pos, RunPos{s.RunStep, phaseDelaying}, "", "")
	}
	info, err := a.Cluster.GetServer(ctx, s.Server)
	if errors.Is(err, ErrNotFound) {
		return a.endRun(ctx, s, pos, ScheduleSkipped, "the server no longer exists")
	}
	if err != nil {
		return a.failRun(ctx, s, pos, "could not read the server: "+err.Error())
	}
	running := info.DesiredState == string(v1alpha1.DesiredRunning)

	switch step.Type {
	case StepCommand:
		if !info.Ready {
			return a.endRun(ctx, s, pos, ScheduleSkipped, "the server was not running")
		}
		if a.Console == nil {
			return a.failRun(ctx, s, pos, "the console is not configured")
		}
		out, err := a.Console.RunCommand(ctx, s.Server, step.Command)
		if errors.Is(err, ErrConsoleUnavailable) {
			return a.failRun(ctx, s, pos, "the server console could not be reached")
		}
		if err != nil {
			return a.failRun(ctx, s, pos, "the command failed: "+err.Error())
		}
		return a.stepDone(ctx, s, pos, stripFormatting(out))

	case StepStop, StepKill, StepRestart:
		if !running && step.Type == StepRestart {
			return a.alreadyThere(ctx, s, pos, "the server was not running")
		}
		if !running && !info.hibernated() {
			return a.alreadyThere(ctx, s, pos, "the server was already stopped")
		}
		if step.Type != StepRestart && a.lastStep(s) {
			if err := a.takeDown(ctx, s.Server, step.Type); err != nil {
				return a.failRun(ctx, s, pos, "could not stop the server: "+err.Error())
			}
			return a.endRun(ctx, s, pos, ScheduleOK, "")
		}
		return a.downThen(ctx, s, pos, phaseStopping, step.Type)

	case StepStart:
		if running && info.Phase != string(v1alpha1.PhaseFailed) {
			return a.alreadyThere(ctx, s, pos, "the server was already running")
		}
		if err := a.moveRun(ctx, s, pos, RunPos{s.RunStep, phaseStarting}, "", ""); err != nil || s.RunState != phaseStarting {
			return err
		}
		return a.advanceStarting(ctx, s, RunPos{s.RunStep, phaseStarting}, 0)

	case StepBackup:
		if _, ok := a.Backuper.(ScheduledBackuper); !ok {
			return a.failRun(ctx, s, pos, "backups are not configured")
		}
		if limit := a.backupStoreCap(ctx); limit > 0 {
			used, err := a.Repo.BackupStoreBytes(ctx)
			if err != nil {
				return a.failRun(ctx, s, pos, "could not read the backup store size: "+err.Error())
			}
			if used >= limit {
				return a.failRun(ctx, s, pos, "the backup store is full; ask an administrator to free space")
			}
		}
		exists, err := a.Cluster.WorldVolumeExists(ctx, s.Server)
		if err != nil {
			return a.failRun(ctx, s, pos, "could not look up the world volume: "+err.Error())
		}
		if !exists {
			return a.endRun(ctx, s, pos, ScheduleSkipped, "the server has no world yet")
		}
		if running {
			return a.backUpRunning(ctx, s, pos)
		}
		if err := a.moveRun(ctx, s, pos, RunPos{s.RunStep, phaseBackupWaiting}, "", ""); err != nil || s.RunState != phaseBackupWaiting {
			return err
		}
		return a.advanceStopping(ctx, s, RunPos{s.RunStep, phaseBackupWaiting}, 0)
	}
	return a.failRun(ctx, s, pos, "unknown step "+step.Type)
}

func (a *API) lastStep(s *Schedule) bool { return s.RunStep == len(s.Steps)-1 }

// alreadyThere ends a one-step run as skipped and moves a longer run past a step whose goal already holds.
func (a *API) alreadyThere(ctx context.Context, s *Schedule, pos RunPos, why string) error {
	if len(s.Steps) == 1 {
		return a.endRun(ctx, s, pos, ScheduleSkipped, why)
	}
	return a.stepDone(ctx, s, pos, "")
}

func (a *API) takeDown(ctx context.Context, server, step string) error {
	if step == StepKill {
		return a.Cluster.KillServer(ctx, server)
	}
	return a.Cluster.SetDesiredState(ctx, server, v1alpha1.DesiredStopped)
}

// downThen persists phase before stopping so a crash between the two does not lose the server's running intent.
func (a *API) downThen(ctx context.Context, s *Schedule, pos RunPos, phase, step string) error {
	s.RunResume = true
	to := RunPos{s.RunStep, phase}
	if err := a.moveRun(ctx, s, pos, to, "", ""); err != nil || s.RunState != phase {
		return err
	}
	if err := a.takeDown(ctx, s.Server, step); err != nil {
		return a.failRun(ctx, s, to, "could not stop the server: "+err.Error())
	}
	return nil
}

// moveRun advances the run and mirrors the move in s; a lost race leaves s unchanged.
func (a *API) moveRun(ctx context.Context, s *Schedule, from, to RunPos, result, detail string) error {
	now := a.now()
	ok, err := a.Schedules.AdvanceScheduleRun(ctx, s.ID, from, to, s.RunResume, result, detail, now)
	if err != nil || !ok {
		return err
	}
	s.RunStep, s.RunState, s.RunStepAt = to.Step, to.Phase, ptrTime(now)
	if result != "" {
		s.LastResult, s.LastDetail = result, detail
	}
	return nil
}

// stepDone ends the run after its last step or begins the next one.
func (a *API) stepDone(ctx context.Context, s *Schedule, pos RunPos, detail string) error {
	if a.lastStep(s) {
		return a.endRun(ctx, s, pos, ScheduleOK, detail)
	}
	next := RunPos{s.RunStep + 1, phaseClaimed}
	if err := a.moveRun(ctx, s, pos, next, "", ""); err != nil || s.RunState != phaseClaimed || s.RunStep != next.Step {
		return err
	}
	return a.beginStep(ctx, s)
}

func (a *API) advanceRun(ctx context.Context, s *Schedule, now time.Time) error {
	pos := RunPos{s.RunStep, s.RunState}
	if s.RunStep < 0 || s.RunStep >= len(s.Steps) {
		return a.endRun(ctx, s, pos, ScheduleFailed, "the run points past the last step")
	}
	waited := now.Sub(*s.RunStepAt)
	switch s.RunState {
	case phaseClaimed:
		if waited < claimedStale {
			return nil
		}
		return a.failRun(ctx, s, pos, "felis-api stopped in the middle of this run")
	case phaseDelaying:
		if waited < time.Duration(s.Steps[s.RunStep].Seconds)*time.Second {
			return nil
		}
		return a.stepDone(ctx, s, pos, "")
	case phaseStopping, phaseBackupStopping, phaseBackupWaiting:
		return a.advanceStopping(ctx, s, pos, waited)
	case phaseBackingUp, phaseBackupRestart:
		return a.advanceBackingUp(ctx, s, pos, waited)
	case phaseStarting:
		return a.advanceStarting(ctx, s, pos, waited)
	case phaseSettling:
		return a.advanceSettling(ctx, s, pos, waited)
	}
	return a.endRun(ctx, s, pos, ScheduleFailed, "unknown run phase "+s.RunState)
}

func (a *API) advanceStopping(ctx context.Context, s *Schedule, pos RunPos, waited time.Duration) error {
	info, err := a.Cluster.GetServer(ctx, s.Server)
	if errors.Is(err, ErrNotFound) {
		return a.endRun(ctx, s, pos, ScheduleSkipped, "the server no longer exists")
	}
	if err != nil {
		return err
	}
	// Preserve a start requested by a person or join while this run was waiting for shutdown.
	if info.DesiredState == string(v1alpha1.DesiredRunning) {
		return a.endRun(ctx, s, pos, ScheduleSkipped, "someone started the server before the run finished")
	}
	if info.Phase != string(v1alpha1.PhaseStopped) {
		if waited < stopWait {
			return nil
		}
		return a.failRun(ctx, s, pos, "the server did not stop within 15 minutes")
	}
	switch {
	case pos.Phase == phaseBackupStopping:
		return a.startBackupJob(ctx, s, pos, phaseBackupRestart, waited)
	case pos.Phase == phaseBackupWaiting:
		return a.startBackupJob(ctx, s, pos, phaseBackingUp, waited)
	case s.Steps[s.RunStep].Type == StepRestart:
		to := RunPos{s.RunStep, phaseStarting}
		if err := a.moveRun(ctx, s, pos, to, "", ""); err != nil || s.RunState != phaseStarting {
			return err
		}
		return a.advanceStarting(ctx, s, to, 0)
	}
	return a.stepDone(ctx, s, pos, "")
}

// startBackupJob hands the stopped world to a backup Job, then waits in phase then.
func (a *API) startBackupJob(ctx context.Context, s *Schedule, pos RunPos, then string, waited time.Duration) error {
	b, ok := a.Backuper.(ScheduledBackuper)
	if !ok {
		return a.failRun(ctx, s, pos, "backups are not configured")
	}
	switch err := a.Cluster.AcquireMaintenance(ctx, s.Server, maintenance.KindBackup); {
	case errors.Is(err, ErrNotStopped):
		if waited < stopWait {
			return nil
		}
		return a.failRun(ctx, s, pos, "the server did not stop within 15 minutes")
	case errors.Is(err, ErrMaintenanceInProgress):
		if waited < stopWait {
			return nil
		}
		return a.failRun(ctx, s, pos, "another operation kept the world busy for 15 minutes")
	case errors.Is(err, ErrNotFound):
		return a.endRun(ctx, s, pos, ScheduleSkipped, "the server no longer exists")
	case err != nil:
		return err
	}
	err := b.BackupScheduled(ctx, s.Server, s.OwnerID)
	// The Job holds the world volume after creation; the lock protects only the handoff gap.
	if rerr := a.Cluster.ReleaseMaintenance(context.WithoutCancel(ctx), s.Server); rerr != nil {
		log.Printf("api: release the maintenance lock on %s: %v (it lapses after %s)", s.Server, rerr, maintenance.Grace)
	}
	if err != nil {
		return a.failRun(ctx, s, pos, "could not start the backup: "+err.Error())
	}
	log.Printf("api: schedule %d started a backup of %s", s.ID, s.Server)
	return a.moveRun(ctx, s, pos, RunPos{s.RunStep, then}, "", "")
}

func (a *API) advanceBackingUp(ctx context.Context, s *Schedule, pos RunPos, waited time.Duration) error {
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
				break
			}
		}
		switch {
		case job == nil && waited < backupJobSkew:
			return nil
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
	if pos.Phase == phaseBackupRestart {
		to := RunPos{s.RunStep, phaseStarting}
		failed := ""
		if result == ScheduleFailed {
			failed = result
		}
		if err := a.moveRun(ctx, s, pos, to, failed, detail); err != nil || s.RunState != phaseStarting {
			return err
		}
		return a.advanceStarting(ctx, s, to, 0)
	}
	if result == ScheduleFailed {
		return a.failRun(ctx, s, pos, detail)
	}
	return a.stepDone(ctx, s, pos, "")
}

func (a *API) advanceStarting(ctx context.Context, s *Schedule, pos RunPos, waited time.Duration) error {
	why, retry, err := a.startScheduled(ctx, s.Server)
	if errors.Is(err, ErrNotFound) {
		return a.endRun(ctx, s, pos, ScheduleSkipped, "the server no longer exists")
	}
	if err != nil {
		return err
	}
	if why == "" {
		s.RunResume = false
		if s.LastResult == ScheduleFailed {
			return a.endRun(ctx, s, pos, ScheduleFailed, s.LastDetail+"; the server was started again")
		}
		if a.lastStep(s) {
			return a.endRun(ctx, s, pos, ScheduleOK, "")
		}
		to := RunPos{s.RunStep, phaseSettling}
		if err := a.moveRun(ctx, s, pos, to, "", ""); err != nil || s.RunState != phaseSettling {
			return err
		}
		return a.advanceSettling(ctx, s, to, 0)
	}
	if s.Steps[s.RunStep].Type == StepStart && !retry {
		return a.endRun(ctx, s, pos, ScheduleSkipped, why)
	}
	if retry && waited < startWait {
		return nil
	}
	detail := "could not start the server again: " + why
	if s.Steps[s.RunStep].Type == StepStart {
		detail = "could not start the server: " + why
	}
	if s.LastResult == ScheduleFailed {
		detail = s.LastDetail + "; " + detail
	}
	return a.endRun(ctx, s, pos, ScheduleFailed, detail)
}

// advanceSettling waits for a started server to be ready before the next step uses it.
func (a *API) advanceSettling(ctx context.Context, s *Schedule, pos RunPos, waited time.Duration) error {
	info, err := a.Cluster.GetServer(ctx, s.Server)
	if errors.Is(err, ErrNotFound) {
		return a.endRun(ctx, s, pos, ScheduleSkipped, "the server no longer exists")
	}
	if err != nil {
		return err
	}
	switch {
	case info.Ready:
		return a.stepDone(ctx, s, pos, "")
	case info.Phase == string(v1alpha1.PhaseFailed):
		return a.endRun(ctx, s, pos, ScheduleFailed, "the server failed to start")
	case waited >= startWait:
		return a.endRun(ctx, s, pos, ScheduleFailed, "the server did not become ready within 15 minutes")
	}
	return nil
}

// startScheduled bypasses the player cooldown; retry reports whether the blocking condition may clear.
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
	policy, _, err := a.readWakePolicy(ctx)
	if err != nil {
		return "", false, err
	}
	ok, err := a.withinRunningCap(ctx, info, policy.MaxRunningServers)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "the cluster is at its running-server cap", true, nil
	}
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

// failRun ends the run as failed and starts again a server this run took down.
func (a *API) failRun(ctx context.Context, s *Schedule, pos RunPos, detail string) error {
	if s.RunResume {
		if err := a.Cluster.SetDesiredState(ctx, s.Server, v1alpha1.DesiredRunning); err == nil {
			detail += "; the server was started again"
		}
	}
	return a.endRun(ctx, s, pos, ScheduleFailed, detail)
}

func (a *API) endRun(ctx context.Context, s *Schedule, pos RunPos, result, detail string) error {
	detail = truncateUTF8(detail, maxScheduleDetail)
	now := a.now()
	ok, err := a.Schedules.FinishScheduleRun(ctx, s.ID, pos, result, detail, s.upcoming(now), now)
	if err != nil || !ok {
		return err
	}
	a.writeAudit(ctx, AuditEntry{
		Actor: scheduleActor, Source: scheduleActor, Action: scheduleRunAction, ServerName: s.Server,
		Payload: auditPayload(map[string]any{"schedule": s.ID, "step": pos.Step, "result": result, "detail": detail}),
	})
	return nil
}

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

// backUpRunning falls back to stop-backup-start when live backup support, console access or volume access is unavailable.
func (a *API) backUpRunning(ctx context.Context, s *Schedule, pos RunPos) error {
	if _, _, ok := a.liveBackupParts(); !ok {
		return a.downThen(ctx, s, pos, phaseBackupStopping, StepStop)
	}
	if err := a.startLiveBackup(ctx, s.Server, s.OwnerID, backupjob.ReasonScheduled); err != nil {
		log.Printf("api: schedule %d could not back %s up while it runs (%v); stopping it for the backup", s.ID, s.Server, err)
		return a.downThen(ctx, s, pos, phaseBackupStopping, StepStop)
	}
	log.Printf("api: schedule %d started a backup of %s while it runs", s.ID, s.Server)
	return a.moveRun(ctx, s, pos, RunPos{s.RunStep, phaseBackingUp}, "", "")
}
