//go:build pgint

package pgint

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
)

func newSchedule(server, owner, step string, next *time.Time) *api.Schedule {
	return &api.Schedule{Server: server, OwnerID: owner, Label: "每晚", Steps: []api.ScheduleStep{{Type: step}}, MinuteOfDay: 240,
		Weekdays: 0x7f, Timezone: "Asia/Shanghai", WarnMinutes: 5, Enabled: next != nil, NextRunAt: next,
		CreatedBy: "pgint"}
}

func mustCreateSchedule(t *testing.T, s *api.Schedule) *api.Schedule {
	t.Helper()
	if err := repo.CreateSchedule(context.Background(), s, 20); err != nil {
		t.Fatalf("CreateSchedule(%v on %s): %v", s.Steps, s.Server, err)
	}
	return s
}

func mustGetSchedule(t *testing.T, server string, id int64) *api.Schedule {
	t.Helper()
	s, err := repo.GetSchedule(context.Background(), server, id)
	if err != nil {
		t.Fatalf("GetSchedule(%d): %v", id, err)
	}
	return s
}

// runView is the run columns of a schedule, for exact comparisons.
func runView(s *api.Schedule) string {
	ts := func(p *time.Time) string {
		if p == nil {
			return "-"
		}
		return p.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("state=%d:%q resume=%v step=%s last=%s result=%q detail=%q next=%s warned=%s enabled=%v runs=%d",
		s.RunStep, s.RunState, s.RunResume, ts(s.RunStepAt), ts(s.LastRunAt), s.LastResult, s.LastDetail,
		ts(s.NextRunAt), ts(s.WarnedFor), s.Enabled, s.RunCount)
}

func applied(t *testing.T, what string, ok bool, err error, want bool) {
	t.Helper()
	if err != nil || ok != want {
		t.Fatalf("%s = %v, %v; want applied=%v", what, ok, err, want)
	}
}

func TestScheduleStoreCRUD(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "sch-crud")
	sfx := suffix(t)
	name, other, gone := "sc-"+sfx, "sco-"+sfx, "scg-"+sfx
	seedOwnedServer(t, name, u.ID, false)
	seedOwnedServer(t, other, u.ID, false)
	seedOwnedServer(t, gone, u.ID, true)
	next := mustNow().Add(time.Hour).Truncate(time.Second)

	s := mustCreateSchedule(t, newSchedule(name, u.ID, api.StepRestart, &next))
	if s.ID == 0 || time.Since(s.CreatedAt) > time.Minute {
		t.Fatalf("created %+v", s)
	}
	settings := func(s *api.Schedule) string {
		return fmt.Sprintf("%d %s owner=%q label=%q steps=%v every=%d at=%d days=%d tz=%s once=%v max=%d warn=%d by=%s created=%s",
			s.ID, s.Server, s.OwnerID, s.Label, s.Steps, s.EveryMinutes, s.MinuteOfDay, s.Weekdays,
			s.Timezone, s.RunOnceAt, s.MaxRuns, s.WarnMinutes, s.CreatedBy, s.CreatedAt.UTC().Format(time.RFC3339Nano))
	}
	got := mustGetSchedule(t, name, s.ID)
	if settings(got) != settings(s) {
		t.Fatalf("read back %s\nwant %s", settings(got), settings(s))
	}
	if v, want := runView(got), `state=0:"" resume=false step=- last=- result="" detail="" next=`+next.UTC().Format(time.RFC3339)+` warned=- enabled=true runs=0`; v != want {
		t.Fatalf("read back %s\nwant %s", v, want)
	}
	if _, err := repo.GetSchedule(ctx, other, s.ID); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("GetSchedule on another server = %v, want ErrNotFound", err)
	}

	unowned := mustCreateSchedule(t, newSchedule(other, "", api.StepStop, nil))
	if g := mustGetSchedule(t, other, unowned.ID); g.OwnerID != "" || g.Enabled || g.NextRunAt != nil {
		t.Fatalf("unowned, disabled schedule read back %+v", g)
	}
	if err := repo.CreateSchedule(ctx, newSchedule(gone, u.ID, api.StepStop, nil), 20); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("CreateSchedule on a deleted server = %v, want ErrNotFound", err)
	}
	if err := repo.CreateSchedule(ctx, newSchedule("nope-"+sfx, u.ID, api.StepStop, nil), 20); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("CreateSchedule on an unknown server = %v, want ErrNotFound", err)
	}

	// The limit counts the server's own rows only.
	mustCreateSchedule(t, newSchedule(name, u.ID, api.StepStop, nil))
	if err := repo.CreateSchedule(ctx, newSchedule(name, u.ID, api.StepStart, nil), 2); !errors.Is(err, api.ErrScheduleLimit) {
		t.Fatalf("third schedule under a limit of 2 = %v, want ErrScheduleLimit", err)
	}
	if err := repo.CreateSchedule(ctx, newSchedule(other, u.ID, api.StepStart, nil), 2); err != nil {
		t.Fatalf("second schedule of the other server under a limit of 2 = %v", err)
	}

	// Update writes the settings and owner and clears the warning marker.
	if ok, err := repo.WarnScheduleRun(ctx, s.ID, next); err != nil || !ok {
		t.Fatalf("WarnScheduleRun = %v, %v", ok, err)
	}
	upd := mustGetSchedule(t, name, s.ID)
	sayHi := []api.ScheduleStep{{Type: api.StepCommand, Command: "say hi"}, {Type: api.StepDelay, Seconds: 30}, {Type: api.StepKill}}
	upd.OwnerID, upd.Label, upd.Steps = "", "", sayHi
	upd.EveryMinutes, upd.MinuteOfDay, upd.Weekdays, upd.Timezone, upd.WarnMinutes, upd.MaxRuns = 30, 0, 0x41, "UTC", 0, 7
	upd.Enabled, upd.NextRunAt = false, nil
	if err := repo.UpdateSchedule(ctx, upd); err != nil {
		t.Fatalf("UpdateSchedule: %v", err)
	}
	g := mustGetSchedule(t, name, s.ID)
	if g.OwnerID != "" || g.Label != "" || fmt.Sprint(g.Steps) != fmt.Sprint(sayHi) || g.MaxRuns != 7 || g.EveryMinutes != 30 ||
		g.MinuteOfDay != 0 || g.Weekdays != 0x41 || g.Timezone != "UTC" || g.WarnMinutes != 0 || g.Enabled ||
		g.NextRunAt != nil || g.WarnedFor != nil {
		t.Fatalf("after update %+v", g)
	}
	wrong := *g
	wrong.Server = other
	if err := repo.UpdateSchedule(ctx, &wrong); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("UpdateSchedule under another server = %v, want ErrNotFound", err)
	}

	// A running schedule refuses changes and deletion; an idle one is deleted.
	now := mustNow().Truncate(time.Second)
	ok, err := mustClaim(t, s.ID, nil, nil, now)
	applied(t, "claim", ok, err, true)
	if err := repo.UpdateSchedule(ctx, g); !errors.Is(err, api.ErrScheduleRunning) {
		t.Fatalf("UpdateSchedule while running = %v, want ErrScheduleRunning", err)
	}
	if err := repo.DeleteSchedule(ctx, name, s.ID); !errors.Is(err, api.ErrScheduleRunning) {
		t.Fatalf("DeleteSchedule while running = %v, want ErrScheduleRunning", err)
	}
	ok, err = repo.FinishScheduleRun(ctx, s.ID, claimed, api.ScheduleOK, "", nil, now)
	applied(t, "finish", ok, err, true)
	if err := repo.DeleteSchedule(ctx, other, s.ID); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("DeleteSchedule under another server = %v, want ErrNotFound", err)
	}
	if err := repo.DeleteSchedule(ctx, name, s.ID); err != nil {
		t.Fatalf("DeleteSchedule: %v", err)
	}
	if err := repo.DeleteSchedule(ctx, name, s.ID); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("second DeleteSchedule = %v, want ErrNotFound", err)
	}
	list, err := repo.ListSchedules(ctx, other)
	if err != nil || len(list) != 2 || list[0].ID != unowned.ID || list[0].ID >= list[1].ID {
		t.Fatalf("ListSchedules(other) = %+v, %v; want 2, oldest first", list, err)
	}
}

var claimed = api.RunPos{Step: 0, Phase: "claimed"}

func mustClaim(t *testing.T, id int64, due, next *time.Time, now time.Time) (bool, error) {
	t.Helper()
	return repo.ClaimScheduleRun(context.Background(), id, due, next, now)
}

func TestScheduleStoreRunCAS(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "sch-cas")
	name := "scc-" + suffix(t)
	seedOwnedServer(t, name, u.ID, false)
	t0 := mustNow().Truncate(time.Second)
	due, next, later := t0.Add(-time.Minute), t0.Add(23*time.Hour), t0.Add(47*time.Hour)
	s := mustCreateSchedule(t, newSchedule(name, u.ID, api.StepBackup, &due))
	ts := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }

	// Warned once for the run due then.
	ok, err := repo.WarnScheduleRun(ctx, s.ID, next)
	applied(t, "warn for another due time", ok, err, false)
	ok, err = repo.WarnScheduleRun(ctx, s.ID, due)
	applied(t, "warn", ok, err, true)
	ok, err = repo.WarnScheduleRun(ctx, s.ID, due)
	applied(t, "second warn", ok, err, false)

	// A claim for another due time loses; the right one wins once.
	ok, err = mustClaim(t, s.ID, &next, &later, t0)
	applied(t, "claim a stale due time", ok, err, false)
	ok, err = mustClaim(t, s.ID, &due, &next, t0)
	applied(t, "claim", ok, err, true)
	ok, err = mustClaim(t, s.ID, &due, &next, t0)
	applied(t, "second claim", ok, err, false)
	ok, err = mustClaim(t, s.ID, nil, nil, t0)
	applied(t, "claim on request during a run", ok, err, false)
	if got, want := runView(mustGetSchedule(t, name, s.ID)),
		fmt.Sprintf(`state=0:"claimed" resume=false step=%s last=%s result="" detail="" next=%s warned=- enabled=true runs=1`, ts(t0), ts(t0), ts(next)); got != want {
		t.Fatalf("after claim %s\nwant %s", got, want)
	}

	// Advance from the wrong step loses; the right one keeps the outcome so far
	// unless it brings one.
	t1 := t0.Add(time.Minute)
	stopping, starting := api.RunPos{Step: 0, Phase: "backup_stopping"}, api.RunPos{Step: 0, Phase: "starting"}
	ok, err = repo.AdvanceScheduleRun(ctx, s.ID, stopping, api.RunPos{Step: 0, Phase: "backup_restart"}, true, "", "", t1)
	applied(t, "advance from the wrong phase", ok, err, false)
	ok, err = repo.AdvanceScheduleRun(ctx, s.ID, api.RunPos{Step: 1, Phase: "claimed"}, stopping, true, "", "", t1)
	applied(t, "advance from the wrong step", ok, err, false)
	ok, err = repo.AdvanceScheduleRun(ctx, s.ID, claimed, stopping, true, "", "", t1)
	applied(t, "advance", ok, err, true)
	t2 := t1.Add(time.Minute)
	ok, err = repo.AdvanceScheduleRun(ctx, s.ID, stopping, starting, true, api.ScheduleFailed, "the backup failed: x", t2)
	applied(t, "advance with an outcome", ok, err, true)
	t3 := t2.Add(time.Minute)
	ok, err = repo.AdvanceScheduleRun(ctx, s.ID, starting, starting, true, "", "", t3)
	applied(t, "advance without one", ok, err, true)
	if got, want := runView(mustGetSchedule(t, name, s.ID)),
		fmt.Sprintf(`state=0:"starting" resume=true step=%s last=%s result="failed" detail="the backup failed: x" next=%s warned=- enabled=true runs=1`, ts(t3), ts(t0), ts(next)); got != want {
		t.Fatalf("after advancing %s\nwant %s", got, want)
	}
	if _, err := db.ExecContext(ctx, `UPDATE server_schedules SET run_step_at = NULL WHERE id = $1`, s.ID); err == nil ||
		!strings.Contains(err.Error(), "check") {
		t.Fatalf("a run step without its time = %v, want a check violation", err)
	}

	// Finish from the wrong step, or from none, loses.
	ok, err = repo.FinishScheduleRun(ctx, s.ID, stopping, api.ScheduleOK, "", nil, t3)
	applied(t, "finish from the wrong phase", ok, err, false)
	ok, err = repo.FinishScheduleRun(ctx, s.ID, starting, api.ScheduleFailed, "done", &later, t3)
	applied(t, "finish", ok, err, true)
	ok, err = repo.FinishScheduleRun(ctx, s.ID, api.RunPos{}, api.ScheduleOK, "", nil, t3)
	applied(t, "finish an idle schedule", ok, err, false)
	if got, want := runView(mustGetSchedule(t, name, s.ID)),
		fmt.Sprintf(`state=0:"" resume=false step=- last=%s result="failed" detail="done" next=%s warned=- enabled=true runs=1`, ts(t0), ts(next)); got != want {
		t.Fatalf("after finishing %s\nwant %s", got, want)
	}

	// A run on request leaves the next run alone and works while disabled.
	ok, err = mustClaim(t, s.ID, nil, nil, t3)
	applied(t, "claim on request", ok, err, true)
	if g := mustGetSchedule(t, name, s.ID); g.NextRunAt == nil || !g.NextRunAt.Equal(next) || g.RunState != "claimed" || g.LastResult != "" || g.RunCount != 2 {
		t.Fatalf("after a claim on request %s", runView(g))
	}
	ok, err = repo.DisableSchedule(ctx, s.ID, "x")
	applied(t, "disable during a run", ok, err, false)
	ok, err = repo.WarnScheduleRun(ctx, s.ID, next)
	applied(t, "warn during a run", ok, err, false)
	ok, err = mustClaim(t, s.ID, &next, &later, t3)
	applied(t, "claim the due run during a run on request", ok, err, false)
	ok, err = repo.MissScheduleRun(ctx, s.ID, next, &later, "x")
	applied(t, "miss during a run", ok, err, false)
	ok, err = repo.FinishScheduleRun(ctx, s.ID, claimed, api.ScheduleOK, "", nil, t3)
	applied(t, "finish the run on request", ok, err, true)

	// Missed: only the run due then, recorded at its due time.
	ok, err = repo.MissScheduleRun(ctx, s.ID, due, &later, "x")
	applied(t, "miss a stale due time", ok, err, false)
	ok, err = repo.WarnScheduleRun(ctx, s.ID, next)
	applied(t, "warn the next run", ok, err, true)
	ok, err = repo.MissScheduleRun(ctx, s.ID, next, &later, "felis-api was down")
	applied(t, "miss", ok, err, true)
	if got, want := runView(mustGetSchedule(t, name, s.ID)),
		fmt.Sprintf(`state=0:"" resume=false step=- last=%s result="missed" detail="felis-api was down" next=%s warned=- enabled=true runs=2`, ts(next), ts(later)); got != want {
		t.Fatalf("after a miss %s\nwant %s", got, want)
	}

	// Disabled: once, and a disabled schedule is neither claimed nor missed.
	ok, err = repo.DisableSchedule(ctx, s.ID, "new owner")
	applied(t, "disable", ok, err, true)
	ok, err = repo.DisableSchedule(ctx, s.ID, "again")
	applied(t, "second disable", ok, err, false)
	if got, want := runView(mustGetSchedule(t, name, s.ID)),
		fmt.Sprintf(`state=0:"" resume=false step=- last=%s result="skipped" detail="new owner" next=- warned=- enabled=false runs=2`, ts(next)); got != want {
		t.Fatalf("after disabling %s\nwant %s", got, want)
	}
	if _, err := db.ExecContext(ctx, `UPDATE server_schedules SET next_run_at = $2 WHERE id = $1`, s.ID, later); err != nil {
		t.Fatal(err)
	}
	ok, err = mustClaim(t, s.ID, &later, &later, t3)
	applied(t, "claim a disabled schedule's due run", ok, err, false)
	ok, err = repo.MissScheduleRun(ctx, s.ID, later, &later, "x")
	applied(t, "miss a disabled schedule's run", ok, err, false)
	ok, err = repo.WarnScheduleRun(ctx, s.ID, later)
	applied(t, "warn a disabled schedule's run", ok, err, false)
}

func TestDueSchedules(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "sch-due")
	v := newUser(t, "user", "sch-due2")
	sfx := suffix(t)
	live, gone, unowned := "sdl-"+sfx, "sdg-"+sfx, "sdu-"+sfx
	seedOwnedServer(t, live, v.ID, false)
	seedOwnedServer(t, gone, u.ID, true)
	mustExec(t, `INSERT INTO servers (name, cached_cpu_milli, cached_memory_mb, cached_storage_mb) VALUES ($1, 100, 128, 1)`, unowned)
	t0 := mustNow().Truncate(time.Second)
	at := func(m int) *time.Time { p := t0.Add(time.Duration(m) * time.Minute); return &p }
	horizon := *at(30)

	late := mustCreateSchedule(t, newSchedule(live, u.ID, api.StepStop, at(20)))
	early := mustCreateSchedule(t, newSchedule(live, u.ID, api.StepStart, at(-5)))
	edge := mustCreateSchedule(t, newSchedule(unowned, "", api.StepStop, at(30)))
	mustCreateSchedule(t, newSchedule(live, u.ID, api.StepStop, at(31)))     // beyond the horizon
	off := mustCreateSchedule(t, newSchedule(live, u.ID, api.StepStop, nil)) // disabled, yet due
	mustExec(t, `UPDATE server_schedules SET next_run_at = $2 WHERE id = $1`, off.ID, *at(-2))
	running := mustCreateSchedule(t, newSchedule(live, u.ID, api.StepRestart, nil)) // disabled, but running
	ok, err := mustClaim(t, running.ID, nil, nil, t0)
	applied(t, "claim", ok, err, true)
	// Deleted server: its schedules wait for SeedServer or the row's end.
	mustExec(t, `INSERT INTO server_schedules (server_name, owner_id, steps, timezone, next_run_at, created_by)
		VALUES ($1, $2, '[{"type":"stop"}]', 'UTC', $3, 'pgint')`, gone, u.ID, *at(-1))

	due, err := repo.DueSchedules(ctx, horizon)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range due {
		if d.Server != live && d.Server != unowned && d.Server != gone {
			continue // other tests' rows
		}
		got = append(got, fmt.Sprintf("%d/%s/%v", d.ID, d.RunState, d.ServerOwner == v.ID))
	}
	want := []string{
		fmt.Sprintf("%d/claimed/true", running.ID),
		fmt.Sprintf("%d//true", early.ID),
		fmt.Sprintf("%d//true", late.ID),
		fmt.Sprintf("%d//false", edge.ID),
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("due %v, want %v", got, want)
	}
	for _, d := range due {
		if d.ID == late.ID && d.OwnerID != u.ID {
			t.Fatalf("schedule owner %q, want %q", d.OwnerID, u.ID)
		}
		if d.ID == edge.ID && d.ServerOwner != "" {
			t.Fatalf("unowned server's owner %q", d.ServerOwner)
		}
	}
}

func TestSchedulesFollowTheServer(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "sch-seed")
	name, sub := "ss-"+suffix(t), "sss-"+suffix(t)
	if err := repo.SeedServer(ctx, name, sub, 100, 128, 1024); err != nil {
		t.Fatal(err)
	}
	s := mustCreateSchedule(t, newSchedule(name, u.ID, api.StepStop, nil))
	if err := repo.SeedServer(ctx, name, sub, 100, 128, 1024); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetSchedule(ctx, name, s.ID); !errors.Is(err, api.ErrNotFound) {
		t.Fatalf("schedule of the earlier server = %v, want ErrNotFound", err)
	}

	src := newUser(t, "user", "sch-src")
	dst := newUser(t, "user", "sch-dst")
	other := newUser(t, "user", "sch-other")
	sfx := suffix(t)
	mine, theirs := "sm-"+sfx, "st-"+sfx
	seedOwnedServer(t, mine, src.ID, false)
	seedOwnedServer(t, theirs, other.ID, false)
	moved := mustCreateSchedule(t, newSchedule(mine, src.ID, api.StepStop, nil))
	stale := mustCreateSchedule(t, newSchedule(mine, other.ID, api.StepStop, nil)) // saved by a previous owner
	kept := mustCreateSchedule(t, newSchedule(theirs, src.ID, api.StepStop, nil))  // src's, on a server src lost
	t0 := mustNow().Truncate(time.Second)
	if err := repo.StartMigration(ctx, "schmig-"+sfx, src.ID, t0); err != nil {
		t.Fatal(err)
	}
	if err := repo.ConfirmMigration(ctx, src.ID, "passkey", "sess", t0); err != nil {
		t.Fatal(err)
	}
	if err := repo.IssueMigrationCode(ctx, src.ID, dst.ID, "sess", "h-sch-"+sfx, t0, t0.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.RedeemMigration(ctx, dst.ID, "h-sch-"+sfx, t0); err != nil {
		t.Fatalf("RedeemMigration: %v", err)
	}
	for _, c := range []struct {
		server string
		id     int64
		owner  string
	}{{mine, moved.ID, dst.ID}, {mine, stale.ID, other.ID}, {theirs, kept.ID, src.ID}} {
		if g := mustGetSchedule(t, c.server, c.id); g.OwnerID != c.owner {
			t.Fatalf("schedule %d owner %q, want %q", c.id, g.OwnerID, c.owner)
		}
	}
}

func TestScheduleRunBudget(t *testing.T) {
	ctx := context.Background()
	u := newUser(t, "user", "sch-budget")
	name := "scb-" + suffix(t)
	seedOwnedServer(t, name, u.ID, false)
	t0 := mustNow().Truncate(time.Second)
	due, next, later := t0.Add(-time.Minute), t0.Add(time.Hour), t0.Add(2*time.Hour)
	claimAndFinish := func(t *testing.T, s *api.Schedule, due, next *time.Time) *api.Schedule {
		t.Helper()
		ok, err := mustClaim(t, s.ID, due, next, t0)
		applied(t, "claim", ok, err, true)
		ok, err = repo.FinishScheduleRun(ctx, s.ID, claimed, api.ScheduleOK, "", next, t0)
		applied(t, "finish", ok, err, true)
		return mustGetSchedule(t, name, s.ID)
	}

	t.Run("max_runs disables the schedule with the record of its last run", func(t *testing.T) {
		s := newSchedule(name, u.ID, api.StepStop, &due)
		s.MaxRuns = 2
		mustCreateSchedule(t, s)
		g := claimAndFinish(t, s, &due, &next)
		if !g.Enabled || g.NextRunAt == nil || !g.NextRunAt.Equal(next) || g.RunCount != 1 {
			t.Fatalf("after the first run %s", runView(g))
		}
		ok, err := mustClaim(t, s.ID, &next, &later, t0)
		applied(t, "claim the last run", ok, err, true)
		if g := mustGetSchedule(t, name, s.ID); g.NextRunAt != nil || !g.Enabled {
			t.Fatalf("the last run left %s", runView(g))
		}
		ok, err = repo.FinishScheduleRun(ctx, s.ID, claimed, api.ScheduleOK, "", &later, t0)
		applied(t, "finish the last run", ok, err, true)
		if g := mustGetSchedule(t, name, s.ID); g.Enabled || g.NextRunAt != nil || g.RunCount != 2 || g.LastResult != api.ScheduleOK {
			t.Fatalf("after the last run %s", runView(g))
		}
	})

	t.Run("a run on request counts", func(t *testing.T) {
		s := newSchedule(name, u.ID, api.StepStop, &next)
		s.MaxRuns = 1
		mustCreateSchedule(t, s)
		g := claimAndFinish(t, s, nil, nil)
		if g.Enabled || g.NextRunAt != nil || g.RunCount != 1 {
			t.Fatalf("after a run on request %s", runView(g))
		}
	})

	t.Run("a one-time schedule runs once", func(t *testing.T) {
		s := newSchedule(name, u.ID, api.StepRestart, &due)
		s.RunOnceAt = &due
		mustCreateSchedule(t, s)
		g := claimAndFinish(t, s, &due, nil)
		if g.Enabled || g.NextRunAt != nil || g.RunOnceAt == nil || !g.RunOnceAt.Equal(due) {
			t.Fatalf("after the one-time run %s", runView(g))
		}
	})

	t.Run("a missed one-time schedule is disabled; a missed repeating one is not counted", func(t *testing.T) {
		once := newSchedule(name, u.ID, api.StepStop, &due)
		once.RunOnceAt = &due
		mustCreateSchedule(t, once)
		ok, err := repo.MissScheduleRun(ctx, once.ID, due, nil, "felis-api was down")
		applied(t, "miss the one-time run", ok, err, true)
		if g := mustGetSchedule(t, name, once.ID); g.Enabled || g.NextRunAt != nil || g.LastResult != api.ScheduleMissed {
			t.Fatalf("after missing the one-time run %s", runView(g))
		}
		rep := newSchedule(name, u.ID, api.StepStop, &due)
		rep.MaxRuns = 1
		mustCreateSchedule(t, rep)
		ok, err = repo.MissScheduleRun(ctx, rep.ID, due, &next, "felis-api was down")
		applied(t, "miss", ok, err, true)
		if g := mustGetSchedule(t, name, rep.ID); !g.Enabled || g.RunCount != 0 {
			t.Fatalf("a missed run counted %s", runView(g))
		}
	})

	t.Run("a run that outlasts its next slot moves to the given one", func(t *testing.T) {
		s := mustCreateSchedule(t, newSchedule(name, u.ID, api.StepStop, &due))
		ok, err := mustClaim(t, s.ID, &due, ptrTo(t0.Add(-30*time.Second)), t0)
		applied(t, "claim", ok, err, true)
		ok, err = repo.FinishScheduleRun(ctx, s.ID, claimed, api.ScheduleOK, "", &next, t0)
		applied(t, "finish", ok, err, true)
		if g := mustGetSchedule(t, name, s.ID); g.NextRunAt == nil || !g.NextRunAt.Equal(next) {
			t.Fatalf("after a long run %s", runView(g))
		}
	})

	t.Run("re-enabling starts a fresh budget; an enabled save keeps the count", func(t *testing.T) {
		s := newSchedule(name, u.ID, api.StepStop, &next)
		s.MaxRuns = 1
		mustCreateSchedule(t, s)
		g := claimAndFinish(t, s, nil, nil)
		g.Enabled, g.NextRunAt = true, &later
		if err := repo.UpdateSchedule(ctx, g); err != nil {
			t.Fatal(err)
		}
		if g := mustGetSchedule(t, name, s.ID); !g.Enabled || g.RunCount != 0 {
			t.Fatalf("after re-enabling %s", runView(g))
		}
		if _, err := db.ExecContext(ctx, `UPDATE server_schedules SET run_count = 4, max_runs = 9 WHERE id = $1`, s.ID); err != nil {
			t.Fatal(err)
		}
		g = mustGetSchedule(t, name, s.ID)
		g.Label = "renamed"
		if err := repo.UpdateSchedule(ctx, g); err != nil {
			t.Fatal(err)
		}
		if g := mustGetSchedule(t, name, s.ID); g.RunCount != 4 {
			t.Fatalf("an enabled save reset the count %s", runView(g))
		}
	})

	t.Run("steps must be a nonempty array of at most ten", func(t *testing.T) {
		s := mustCreateSchedule(t, newSchedule(name, u.ID, api.StepStop, nil))
		for _, steps := range []string{`[]`, `{}`, `[` + strings.Repeat(`{"type":"stop"},`, 10) + `{"type":"stop"}]`} {
			if _, err := db.ExecContext(ctx, `UPDATE server_schedules SET steps = $2 WHERE id = $1`, s.ID, steps); err == nil ||
				!strings.Contains(err.Error(), "check") {
				t.Fatalf("steps %s = %v, want a check violation", steps, err)
			}
		}
	})
}

func ptrTo(t time.Time) *time.Time { return &t }
