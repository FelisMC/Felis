package api

import (
	"errors"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
)

// outcome asserts how a schedule's last run ended and that no run is left.
func (r *schedRig) outcome(t *testing.T, id int64, result, detail string) *Schedule {
	t.Helper()
	s := r.st.row(t, id)
	if s.RunState != "" || s.LastResult != result || s.LastDetail != detail {
		t.Fatalf("run state %q, result %q %q; want done with %q %q", s.RunState, s.LastResult, s.LastDetail, result, detail)
	}
	return s
}

// step asserts a run waits at step.
func (r *schedRig) step(t *testing.T, id int64, step string) {
	t.Helper()
	if s := r.st.row(t, id); s.RunState != step {
		t.Fatalf("run state %q (result %q %q), want %q", s.RunState, s.LastResult, s.LastDetail, step)
	}
}

// runAudits lists the schedule.run rows as result:detail.
func (r *schedRig) runAudits() []string {
	var out []string
	for _, e := range r.repo.audits {
		if e.Action == scheduleRunAction && e.Actor == scheduleActor && e.Source == scheduleActor {
			out = append(out, string(e.Payload))
		}
	}
	return out
}

func TestScheduleRunnerFiring(t *testing.T) {
	t.Run("a due command runs, moves on a day and is audited", func(t *testing.T) {
		r := newSchedRig(t)
		r.con.reply = "  §6There are §c2§6 players online  "
		id := r.schedule(ScheduleCommand, schedT0, func(s *Schedule) { s.Command = "list" })
		r.clock = schedT0.Add(20 * time.Second)
		r.tick(t)
		s := r.outcome(t, id, ScheduleOK, "There are 2 players online")
		if !sameTime(s.NextRunAt, schedT0.AddDate(0, 0, 1)) || !sameTime(s.LastRunAt, r.clock) || r.con.gotCommand != "list" {
			t.Fatalf("next %v, last %v, console %q", s.NextRunAt, s.LastRunAt, r.con.gotCommand)
		}
		if got := r.runAudits(); len(got) != 1 ||
			got[0] != `{"action":"command","detail":"There are 2 players online","result":"ok","schedule":1}` {
			t.Fatalf("run audits %v", got)
		}
		r.tick(t) // not due again
		if r.con.calls != 1 || len(r.runAudits()) != 1 {
			t.Fatalf("ran again: %d console calls", r.con.calls)
		}
	})

	t.Run("not due yet: nothing happens", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleCommand, schedT0.Add(time.Second))
		r.tick(t)
		if s := r.st.row(t, id); r.con.calls != 0 || s.LastRunAt != nil || !sameTime(s.NextRunAt, schedT0.Add(time.Second)) {
			t.Fatalf("ran early: %+v", s)
		}
	})

	t.Run("a long reply is cut at 500 bytes on a rune boundary", func(t *testing.T) {
		r := newSchedRig(t)
		r.con.reply = "x" + strings.Repeat("猫", 200)
		id := r.schedule(ScheduleCommand, schedT0)
		r.tick(t)
		if s := r.st.row(t, id); s.LastDetail != "x"+strings.Repeat("猫", 166) {
			t.Fatalf("detail is %d bytes: %q", len(s.LastDetail), s.LastDetail)
		}
	})

	t.Run("a command on a stopped server is skipped", func(t *testing.T) {
		r := newSchedRig(t)
		r.stopped()
		id := r.schedule(ScheduleCommand, schedT0)
		r.tick(t)
		r.outcome(t, id, ScheduleSkipped, "the server was not running")
		if r.con.calls != 0 {
			t.Fatal("dialed a stopped server")
		}
	})

	t.Run("an unreachable console fails the run", func(t *testing.T) {
		r := newSchedRig(t)
		r.con.err = ErrConsoleUnavailable
		id := r.schedule(ScheduleCommand, schedT0)
		r.tick(t)
		r.outcome(t, id, ScheduleFailed, "the server console could not be reached")
	})

	t.Run("ten minutes late still runs; later is missed", func(t *testing.T) {
		r := newSchedRig(t)
		late := r.schedule(ScheduleCommand, schedT0.Add(-scheduleMissGrace))
		missed := r.schedule(ScheduleCommand, schedT0.Add(-scheduleMissGrace-time.Second))
		r.tick(t)
		r.outcome(t, late, ScheduleOK, "")
		s := r.outcome(t, missed, ScheduleMissed, "felis-api was not running at the scheduled time")
		if r.con.calls != 1 || !sameTime(s.LastRunAt, schedT0.Add(-scheduleMissGrace-time.Second)) ||
			!sameTime(s.NextRunAt, time.Date(2026, 9, 29, 2, 49, 0, 0, time.UTC)) {
			t.Fatalf("console calls %d, missed row %+v", r.con.calls, s)
		}
	})

	t.Run("a new owner disables the schedule instead of running it", func(t *testing.T) {
		r := newSchedRig(t)
		r.st.owners["survival"] = "newowner"
		id := r.schedule(ScheduleCommand, schedT0)
		r.tick(t)
		s := r.outcome(t, id, ScheduleSkipped, "the server has a new owner since this schedule was saved; save it again to use it")
		if s.Enabled || s.NextRunAt != nil || r.con.calls != 0 {
			t.Fatalf("still enabled or ran: %+v, %d console calls", s, r.con.calls)
		}
	})

	t.Run("the schedule of a released server stops too", func(t *testing.T) {
		r := newSchedRig(t)
		r.st.owners["survival"] = ""
		id := r.schedule(ScheduleStop, schedT0)
		r.tick(t)
		if s := r.st.row(t, id); s.Enabled || r.cl.desired["survival"] != "" {
			t.Fatalf("ran on a released server: %+v", s)
		}
	})

	t.Run("stop", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleStop, schedT0)
		r.tick(t)
		r.outcome(t, id, ScheduleOK, "")
		if r.cl.desired["survival"] != v1alpha1.DesiredStopped {
			t.Fatalf("desired %q", r.cl.desired["survival"])
		}
		r2 := newSchedRig(t)
		r2.stopped()
		id = r2.schedule(ScheduleStop, schedT0)
		r2.tick(t)
		r2.outcome(t, id, ScheduleSkipped, "the server was already stopped")
		if r2.cl.desired["survival"] != "" {
			t.Fatal("stopped a stopped server again")
		}
	})

	t.Run("a server gone since the schedule was saved", func(t *testing.T) {
		r := newSchedRig(t)
		delete(r.cl.byName, "survival")
		id := r.schedule(ScheduleStop, schedT0)
		r.tick(t)
		r.outcome(t, id, ScheduleSkipped, "the server no longer exists")
	})
}

func TestScheduleRunnerStart(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(r *schedRig)
		result  string
		detail  string
		desired v1alpha1.DesiredState
		retried bool
	}{
		{"a stopped server starts", func(r *schedRig) { r.stopped() }, ScheduleOK, "", v1alpha1.DesiredRunning, false},
		{"a running server is left alone", func(*schedRig) {}, ScheduleSkipped, "the server was already running", "", false},
		{"a failed server is retried", func(r *schedRig) { r.cl.byName["survival"].Phase = string(v1alpha1.PhaseFailed) },
			ScheduleOK, "", v1alpha1.DesiredRunning, true},
		{"a failed start of a stopped server starts plainly", func(r *schedRig) {
			r.stopped()
			r.cl.byName["survival"].Phase = string(v1alpha1.PhaseFailed)
		}, ScheduleOK, "", v1alpha1.DesiredRunning, false},
		{"the running cap holds it back", func(r *schedRig) {
			r.stopped()
			r.a.MaxRunningServers = 1
			r.cl.list = []ServerInfo{{Name: "other", DesiredState: string(v1alpha1.DesiredRunning)}}
		}, ScheduleSkipped, "the cluster is at its running-server cap", "", false},
		{"a pending retirement holds it back", func(r *schedRig) {
			r.stopped()
			r.repo.byName["survival"].Retire = &RetireState{}
		}, ScheduleSkipped, "the server is being given up or deleted", "", false},
		{"a busy world holds it back", func(r *schedRig) {
			r.stopped()
			r.cl.wakeErr["survival"] = &MaintenanceBusyError{Kind: maintenance.KindBackup}
		}, ScheduleSkipped, "the world is busy with a backup", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newSchedRig(t)
			tc.setup(r)
			id := r.schedule(ScheduleStart, schedT0)
			r.tick(t)
			r.outcome(t, id, tc.result, tc.detail)
			if r.cl.desired["survival"] != tc.desired || (len(r.cl.retried) == 1) != tc.retried {
				t.Fatalf("desired %q, retried %v", r.cl.desired["survival"], r.cl.retried)
			}
		})
	}
}

func TestScheduleRunnerWarning(t *testing.T) {
	t.Run("told once, at the warning time", func(t *testing.T) {
		r := newSchedRig(t)
		due := schedT0.Add(5 * time.Minute)
		id := r.schedule(ScheduleRestart, due, func(s *Schedule) { s.WarnMinutes = 5 })
		r.clock = schedT0.Add(-time.Second)
		r.tick(t)
		if r.con.calls != 0 {
			t.Fatal("warned early")
		}
		r.clock = schedT0
		r.tick(t)
		r.clock = schedT0.Add(15 * time.Second)
		r.tick(t)
		if r.con.calls != 1 || r.con.gotCommand != "say [Felis] 服务器将在 5 分钟后重启 / Server restarts in 5 min" {
			t.Fatalf("%d warnings, last %q", r.con.calls, r.con.gotCommand)
		}
		if s := r.st.row(t, id); !sameTime(s.WarnedFor, due) || s.LastRunAt != nil {
			t.Fatalf("after the warning %+v", s)
		}
		r.clock = due
		r.tick(t) // the run itself claims and clears the marker
		if s := r.st.row(t, id); s.WarnedFor != nil || s.RunState != runStopping {
			t.Fatalf("after the run started %+v", s)
		}
	})

	t.Run("late warnings say how long is really left", func(t *testing.T) {
		for action, want := range map[string]string{
			ScheduleStop:   "say [Felis] 服务器将在 2 分钟后关闭 / Server stops in 2 min",
			ScheduleBackup: "say [Felis] 服务器将在 2 分钟后暂停做备份，完成后自动恢复 / Server pauses for a backup in 2 min and comes back after",
		} {
			r := newSchedRig(t)
			r.schedule(action, schedT0.Add(90*time.Second), func(s *Schedule) { s.WarnMinutes = 10 })
			r.tick(t)
			if r.con.gotCommand != want {
				t.Fatalf("%s warned %q, want %q", action, r.con.gotCommand, want)
			}
		}
	})

	t.Run("nobody to tell on a stopped server, none without a lead time", func(t *testing.T) {
		r := newSchedRig(t)
		r.stopped()
		a := r.schedule(ScheduleRestart, schedT0.Add(time.Minute), func(s *Schedule) { s.WarnMinutes = 5 })
		r.tick(t)
		r2 := newSchedRig(t)
		r2.schedule(ScheduleRestart, schedT0.Add(time.Minute))
		r2.tick(t)
		if r.con.calls != 0 || r2.con.calls != 0 || r.st.row(t, a).WarnedFor != nil {
			t.Fatalf("warned: %d / %d", r.con.calls, r2.con.calls)
		}
	})
}

func TestScheduleRunnerRestart(t *testing.T) {
	t.Run("stops, waits for the pod, starts", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleRestart, schedT0)
		r.tick(t)
		r.step(t, id, runStopping)
		if r.cl.desired["survival"] != v1alpha1.DesiredStopped {
			t.Fatalf("desired %q", r.cl.desired["survival"])
		}
		// Still shutting down.
		info := r.cl.byName["survival"]
		info.DesiredState = string(v1alpha1.DesiredStopped)
		r.clock = schedT0.Add(time.Minute)
		r.tick(t)
		r.step(t, id, runStopping)
		info.Ready = false
		r.tick(t)
		r.step(t, id, runStopping) // not Ready, but not Stopped either
		info.Phase = string(v1alpha1.PhaseStopped)
		r.tick(t)
		r.outcome(t, id, ScheduleOK, "")
		if r.cl.desired["survival"] != v1alpha1.DesiredRunning {
			t.Fatalf("desired %q", r.cl.desired["survival"])
		}
		if got := r.runAudits(); len(got) != 1 || got[0] != `{"action":"restart","detail":"","result":"ok","schedule":1}` {
			t.Fatalf("run audits %v", got)
		}
	})

	t.Run("a stopped server is not restarted", func(t *testing.T) {
		r := newSchedRig(t)
		r.stopped()
		id := r.schedule(ScheduleRestart, schedT0)
		r.tick(t)
		r.outcome(t, id, ScheduleSkipped, "the server was not running")
	})

	t.Run("someone starting it meanwhile ends the run", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleRestart, schedT0)
		r.tick(t)
		r.clock = schedT0.Add(30 * time.Second)
		r.tick(t) // info still says desired Running: a person started it again
		r.outcome(t, id, ScheduleSkipped, "someone started the server before the run finished")
	})

	t.Run("a pod that does not stop in 15 minutes: give up and start it again", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleRestart, schedT0)
		r.tick(t)
		r.cl.byName["survival"].DesiredState = string(v1alpha1.DesiredStopped)
		r.clock = schedT0.Add(stopWait - time.Second)
		r.tick(t)
		r.step(t, id, runStopping)
		r.clock = schedT0.Add(stopWait)
		r.tick(t)
		r.outcome(t, id, ScheduleFailed, "the server did not stop within 15 minutes; it was started again")
		if r.cl.desired["survival"] != v1alpha1.DesiredRunning {
			t.Fatalf("desired %q", r.cl.desired["survival"])
		}
	})

	t.Run("the running cap: keep trying for 15 minutes, then fail", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleRestart, schedT0)
		r.tick(t)
		r.stopped()
		r.a.MaxRunningServers = 1
		r.cl.list = []ServerInfo{{Name: "other", DesiredState: string(v1alpha1.DesiredRunning)}}
		r.clock = schedT0.Add(time.Minute)
		r.tick(t)
		r.step(t, id, runStarting)
		r.clock = schedT0.Add(time.Minute + startWait - time.Second)
		r.tick(t)
		r.step(t, id, runStarting)
		r.clock = schedT0.Add(time.Minute + startWait)
		r.tick(t)
		r.outcome(t, id, ScheduleFailed, "could not start the server again: the cluster is at its running-server cap")
	})

	t.Run("the cap clearing lets it start", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleRestart, schedT0)
		r.tick(t)
		r.stopped()
		r.a.MaxRunningServers = 1
		r.cl.list = []ServerInfo{{Name: "other", DesiredState: string(v1alpha1.DesiredRunning)}}
		r.tick(t)
		r.cl.list = nil
		r.clock = schedT0.Add(time.Minute)
		r.tick(t)
		r.outcome(t, id, ScheduleOK, "")
	})
}

func TestScheduleRunnerBackup(t *testing.T) {
	job := func(state, message string, at time.Time) AsyncJob {
		return AsyncJob{Kind: "backup", State: state, Message: message, StartedAt: at, Scheduled: true}
	}

	t.Run("a running server: stop, back up, start", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleBackup, schedT0)
		r.tick(t)
		r.step(t, id, runStopping)
		r.clock = schedT0.Add(time.Minute)
		r.cl.maintErr["survival"] = ErrNotStopped
		r.cl.byName["survival"].DesiredState = string(v1alpha1.DesiredStopped)
		r.tick(t)
		r.step(t, id, runStopping) // the pod is still going down
		delete(r.cl.maintErr, "survival")
		r.stopped()
		r.clock = schedT0.Add(2 * time.Minute)
		r.tick(t)
		r.step(t, id, runBackingUp)
		if strings.Join(r.cl.acquired, ",") != "survival:backup" || strings.Join(r.cl.released, ",") != "survival" ||
			len(r.b.scheduled) != 1 || r.b.scheduled[0] != (ScheduledCandidate{Name: "survival", OwnerID: "owner1"}) {
			t.Fatalf("acquired %v released %v backups %v", r.cl.acquired, r.cl.released, r.b.scheduled)
		}
		r.jobs.jobs = []AsyncJob{job("running", "", r.clock.Add(-time.Minute))} // clock skew
		r.clock = schedT0.Add(30 * time.Minute)
		r.tick(t)
		r.step(t, id, runBackingUp)
		r.jobs.jobs = []AsyncJob{job("succeeded", "", schedT0.Add(time.Minute))}
		r.tick(t)
		r.outcome(t, id, ScheduleOK, "")
		if r.cl.desired["survival"] != v1alpha1.DesiredRunning || r.jobs.got != "survival" {
			t.Fatalf("desired %q, jobs read for %q", r.cl.desired["survival"], r.jobs.got)
		}
	})

	t.Run("a stopped server is backed up at once and left stopped", func(t *testing.T) {
		r := newSchedRig(t)
		r.stopped()
		id := r.schedule(ScheduleBackup, schedT0)
		r.tick(t)
		r.step(t, id, runBackingUp)
		r.jobs.jobs = []AsyncJob{job("succeeded", "", schedT0)}
		r.tick(t)
		r.outcome(t, id, ScheduleOK, "")
		if r.cl.desired["survival"] != "" {
			t.Fatalf("desired %q, want untouched", r.cl.desired["survival"])
		}
	})

	t.Run("a failed backup is reported and the server still comes back", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleBackup, schedT0)
		r.tick(t)
		r.stopped()
		r.tick(t)
		r.jobs.jobs = []AsyncJob{job("failed", "disk full", schedT0), job("succeeded", "", schedT0.Add(-time.Hour))}
		r.tick(t)
		r.outcome(t, id, ScheduleFailed, "the backup failed: disk full")
		if r.cl.desired["survival"] != v1alpha1.DesiredRunning {
			t.Fatalf("desired %q", r.cl.desired["survival"])
		}
	})

	t.Run("an older or unscheduled Job is not this run's", func(t *testing.T) {
		r := newSchedRig(t)
		r.stopped()
		id := r.schedule(ScheduleBackup, schedT0)
		r.tick(t)
		manual := job("failed", "x", schedT0)
		manual.Scheduled = false
		r.jobs.jobs = []AsyncJob{manual, job("succeeded", "", schedT0.Add(-backupJobSkew-time.Second))}
		r.clock = schedT0.Add(backupJobSkew - time.Second)
		r.tick(t)
		r.step(t, id, runBackingUp)
		r.clock = schedT0.Add(backupJobSkew)
		r.tick(t)
		r.outcome(t, id, ScheduleFailed, "the backup Job is gone before it could be checked; see the Backups page")
	})

	t.Run("two scheduled Jobs since the step: the newest counts", func(t *testing.T) {
		r := newSchedRig(t)
		r.stopped()
		id := r.schedule(ScheduleBackup, schedT0)
		r.tick(t)
		r.jobs.jobs = []AsyncJob{job("succeeded", "", schedT0.Add(time.Minute)), job("failed", "disk full", schedT0)}
		r.tick(t)
		r.outcome(t, id, ScheduleOK, "")
	})

	t.Run("a Job running past 45 minutes", func(t *testing.T) {
		r := newSchedRig(t)
		r.stopped()
		id := r.schedule(ScheduleBackup, schedT0)
		r.tick(t)
		r.jobs.jobs = []AsyncJob{job("running", "", schedT0)}
		r.clock = schedT0.Add(backupWait - time.Second)
		r.tick(t)
		r.step(t, id, runBackingUp)
		r.clock = schedT0.Add(backupWait)
		r.tick(t)
		r.outcome(t, id, ScheduleFailed, "the backup did not finish within 45 minutes")
	})

	t.Run("a world kept busy for 15 minutes", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleBackup, schedT0)
		r.tick(t)
		r.stopped()
		r.cl.maintErr["survival"] = &MaintenanceBusyError{Kind: maintenance.KindRestore}
		r.clock = schedT0.Add(stopWait - time.Second)
		r.tick(t)
		r.step(t, id, runStopping)
		r.clock = schedT0.Add(stopWait)
		r.tick(t)
		r.outcome(t, id, ScheduleFailed, "another operation kept the world busy for 15 minutes; it was started again")
		if len(r.b.scheduled) != 0 || r.cl.desired["survival"] != v1alpha1.DesiredRunning {
			t.Fatalf("backups %v, desired %q", r.b.scheduled, r.cl.desired["survival"])
		}
	})

	t.Run("a pod that does not stop in 15 minutes: no backup, started again", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleBackup, schedT0)
		r.tick(t)
		r.cl.byName["survival"].DesiredState = string(v1alpha1.DesiredStopped)
		r.cl.maintErr["survival"] = ErrNotStopped
		r.clock = schedT0.Add(stopWait)
		r.tick(t)
		r.outcome(t, id, ScheduleFailed, "the server did not stop within 15 minutes; it was started again")
		if len(r.b.scheduled) != 0 {
			t.Fatalf("backups %v", r.b.scheduled)
		}
	})

	t.Run("a failed Job without a message", func(t *testing.T) {
		r := newSchedRig(t)
		r.stopped()
		id := r.schedule(ScheduleBackup, schedT0)
		r.tick(t)
		r.jobs.jobs = []AsyncJob{job("failed", "", schedT0)}
		r.tick(t)
		r.outcome(t, id, ScheduleFailed, "the backup failed:")
	})

	t.Run("the backup cannot start", func(t *testing.T) {
		r := newSchedRig(t)
		r.b.err = errors.New("quota")
		id := r.schedule(ScheduleBackup, schedT0)
		r.tick(t)
		r.stopped()
		r.tick(t)
		r.outcome(t, id, ScheduleFailed, "could not start the backup: quota; the server was started again")
		if strings.Join(r.cl.released, ",") != "survival" {
			t.Fatalf("released %v", r.cl.released)
		}
	})

	t.Run("refused before touching the server", func(t *testing.T) {
		cases := []struct {
			name, result, detail string
			setup                func(r *schedRig)
		}{
			{"no backup executor", ScheduleFailed, "backups are not configured", func(r *schedRig) { r.a.Backuper = &fakeBackuper{} }},
			{"store full", ScheduleFailed, "the backup store is full; ask an administrator to free space", func(r *schedRig) {
				r.a.BackupStoreCap = 1
				r.repo.backups = append(r.repo.backups, fakeBackup{view: BackupView{Status: "present", SizeBytes: 1}})
			}},
			{"no world yet", ScheduleSkipped, "the server has no world yet", func(r *schedRig) { r.cl.noWorld["survival"] = true }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				r := newSchedRig(t)
				tc.setup(r)
				id := r.schedule(ScheduleBackup, schedT0)
				r.tick(t)
				r.outcome(t, id, tc.result, tc.detail)
				if r.cl.desired["survival"] != "" || len(r.b.scheduled) != 0 {
					t.Fatalf("desired %q, backups %v", r.cl.desired["survival"], r.b.scheduled)
				}
			})
		}
	})
}

func TestScheduleRunnerStaleClaim(t *testing.T) {
	r := newSchedRig(t)
	id := r.schedule(ScheduleCommand, schedT0.Add(time.Hour), func(s *Schedule) {
		s.RunState, s.RunStepAt = runClaimed, ptrTime(schedT0.Add(-claimedStale+time.Second))
	})
	r.tick(t)
	r.step(t, id, runClaimed)
	r.clock = schedT0.Add(time.Second)
	r.tick(t)
	r.outcome(t, id, ScheduleFailed, "felis-api stopped in the middle of this run")
	if r.con.calls != 0 {
		t.Fatal("re-ran the command of a stale claim")
	}
}

func TestStripFormatting(t *testing.T) {
	if got := stripFormatting(" §l§aHi§r §x§§ok "); got != "Hi ok" {
		t.Fatalf("stripFormatting = %q", got)
	}
}

func TestScheduleRunnerEdges(t *testing.T) {
	t.Run("no store: nothing to run", func(t *testing.T) {
		r := newSchedRig(t)
		r.a.Schedules = nil
		r.tick(t)
	})

	t.Run("no console: no warning, and a command fails", func(t *testing.T) {
		r := newSchedRig(t)
		r.a.Console = nil
		warn := r.schedule(ScheduleRestart, schedT0.Add(time.Minute), func(s *Schedule) { s.WarnMinutes = 5 })
		cmd := r.schedule(ScheduleCommand, schedT0)
		r.tick(t)
		r.outcome(t, cmd, ScheduleFailed, "the console is not configured")
		if s := r.st.row(t, warn); s.WarnedFor != nil {
			t.Fatalf("marked warned without a console: %+v", s)
		}
	})

	t.Run("a stopped server's backup gives up without starting it", func(t *testing.T) {
		r := newSchedRig(t)
		r.stopped()
		r.cl.maintErr["survival"] = &MaintenanceBusyError{Kind: maintenance.KindFileWrite}
		id := r.schedule(ScheduleBackup, schedT0)
		r.tick(t)
		r.step(t, id, runStopping)
		r.clock = schedT0.Add(stopWait)
		r.tick(t)
		r.outcome(t, id, ScheduleFailed, "another operation kept the world busy for 15 minutes")
		if r.cl.desired["survival"] != "" {
			t.Fatalf("desired %q, want untouched", r.cl.desired["survival"])
		}
	})

	t.Run("a stopped server's backup that cannot start leaves it stopped", func(t *testing.T) {
		r := newSchedRig(t)
		r.stopped()
		r.b.err = errors.New("quota")
		id := r.schedule(ScheduleBackup, schedT0)
		r.tick(t)
		r.outcome(t, id, ScheduleFailed, "could not start the backup: quota")
		if r.cl.desired["survival"] != "" {
			t.Fatalf("desired %q, want untouched", r.cl.desired["survival"])
		}
	})

	t.Run("the backup executor gone mid-run (felis-api restarted without it)", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleBackup, schedT0)
		r.tick(t)
		r.stopped()
		r.a.Backuper = &fakeBackuper{}
		r.tick(t)
		r.outcome(t, id, ScheduleFailed, "backups are not configured")
	})

	t.Run("the server deleted while its world is being locked", func(t *testing.T) {
		r := newSchedRig(t)
		r.stopped()
		r.cl.maintErr["survival"] = ErrNotFound
		id := r.schedule(ScheduleBackup, schedT0)
		r.tick(t)
		r.outcome(t, id, ScheduleSkipped, "the server no longer exists")
	})

	t.Run("without Job status the backup counts as done", func(t *testing.T) {
		r := newSchedRig(t)
		r.stopped()
		r.a.JobStatus = nil
		id := r.schedule(ScheduleBackup, schedT0)
		r.tick(t)
		r.step(t, id, runBackingUp)
		r.tick(t)
		r.outcome(t, id, ScheduleOK, "")
	})

	t.Run("a failed backup whose server cannot start either names both", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleBackup, schedT0)
		r.tick(t)
		r.stopped()
		r.tick(t)
		r.repo.byName["survival"].Retire = &RetireState{}
		r.jobs.jobs = []AsyncJob{{Kind: "backup", State: "failed", Message: "disk full", StartedAt: schedT0, Scheduled: true}}
		r.tick(t)
		r.outcome(t, id, ScheduleFailed, "the backup failed: disk full; could not start the server again: the server is being given up or deleted")
	})

	t.Run("a retirement stops the restart at once", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleRestart, schedT0)
		r.tick(t)
		r.stopped()
		r.repo.byName["survival"].Retire = &RetireState{}
		r.tick(t)
		r.outcome(t, id, ScheduleFailed, "could not start the server again: the server is being given up or deleted")
	})

	t.Run("the server deleted before it could start again", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleRestart, schedT0)
		r.tick(t)
		r.stopped()
		delete(r.repo.byName, "survival")
		r.tick(t)
		r.outcome(t, id, ScheduleSkipped, "the server no longer exists")
	})
}

func TestScheduleNextRunMidnightJump(t *testing.T) {
	// Chile moves its clocks from 00:00 to 01:00 on 2026-09-06: that Sunday has
	// no midnight, so the weekday is read at midday.
	santiago := mustZone(t, "America/Santiago")
	s := Schedule{MinuteOfDay: 12 * 60, Weekdays: 1, Timezone: "America/Santiago"}
	after := time.Date(2026, 9, 5, 13, 0, 0, 0, santiago)
	if got, want := s.nextRun(after), time.Date(2026, 9, 6, 12, 0, 0, 0, santiago); !got.Equal(want) {
		t.Fatalf("nextRun = %s, want %s", got, want)
	}
}

// TestScheduleFinishLostRace: a finish that another felis-api beat to the row
// changes nothing and leaves no second schedule.run audit.
func TestScheduleFinishLostRace(t *testing.T) {
	r := newSchedRig(t)
	id := r.schedule(ScheduleRestart, schedT0, func(s *Schedule) { s.LastResult = ScheduleOK })
	s := r.st.row(t, id)
	if err := r.a.finishRun(t.Context(), s, runStarting, ScheduleFailed, "late"); err != nil {
		t.Fatal(err)
	}
	r.outcome(t, id, ScheduleOK, "")
	if len(r.repo.audits) != 0 {
		t.Fatalf("audits %+v", r.repo.audits)
	}
}
