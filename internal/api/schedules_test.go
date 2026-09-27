package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

var _ ServerSchedules = (*PGRepo)(nil)

// fakeSchedules is an in-memory ServerSchedules with the compare-and-set
// conditions of the PGRepo queries (internal/pgint/schedules_test.go runs the
// same cases against Postgres). It hands out copies, so a test sees only what
// was written through the interface.
type fakeSchedules struct {
	rows   map[int64]*Schedule
	owners map[string]string // server -> its owner now, for DueSchedules
	nextID int64
	claims int
}

func newFakeSchedules() *fakeSchedules {
	return &fakeSchedules{rows: map[int64]*Schedule{}, owners: map[string]string{}}
}

func cloneSchedule(s *Schedule) *Schedule {
	c := *s
	for _, p := range []**time.Time{&c.NextRunAt, &c.WarnedFor, &c.RunStepAt, &c.LastRunAt} {
		if *p != nil {
			*p = ptrTime(**p)
		}
	}
	return &c
}

func sameTime(p *time.Time, t time.Time) bool { return p != nil && p.Equal(t) }

// put stores s as it is and returns its id.
func (f *fakeSchedules) put(s Schedule) int64 {
	f.nextID++
	s.ID = f.nextID
	f.rows[s.ID] = cloneSchedule(&s)
	return s.ID
}

// row reads a schedule back for a test's assertions.
func (f *fakeSchedules) row(t *testing.T, id int64) *Schedule {
	t.Helper()
	r, ok := f.rows[id]
	if !ok {
		t.Fatalf("schedule %d is gone", id)
	}
	return cloneSchedule(r)
}

func (f *fakeSchedules) ListSchedules(_ context.Context, server string) ([]Schedule, error) {
	var out []Schedule
	for _, r := range f.rows {
		if r.Server == server {
			out = append(out, *cloneSchedule(r))
		}
	}
	slices.SortFunc(out, func(a, b Schedule) int { return int(a.ID - b.ID) })
	return out, nil
}

func (f *fakeSchedules) GetSchedule(_ context.Context, server string, id int64) (*Schedule, error) {
	r, ok := f.rows[id]
	if !ok || r.Server != server {
		return nil, ErrNotFound
	}
	return cloneSchedule(r), nil
}

func (f *fakeSchedules) CreateSchedule(_ context.Context, s *Schedule, limit int) error {
	n := 0
	for _, r := range f.rows {
		if r.Server == s.Server {
			n++
		}
	}
	if n >= limit {
		return ErrScheduleLimit
	}
	s.CreatedAt = time.Unix(1_700_000_000, 0)
	s.ID = f.put(*s)
	return nil
}

func (f *fakeSchedules) idle(server string, id int64) (*Schedule, error) {
	r, ok := f.rows[id]
	switch {
	case !ok || r.Server != server:
		return nil, ErrNotFound
	case r.RunState != "":
		return nil, ErrScheduleRunning
	}
	return r, nil
}

func (f *fakeSchedules) UpdateSchedule(_ context.Context, s *Schedule) error {
	r, err := f.idle(s.Server, s.ID)
	if err != nil {
		return err
	}
	r.OwnerID, r.Label, r.Action, r.Command = s.OwnerID, s.Label, s.Action, s.Command
	r.EveryMinutes, r.MinuteOfDay, r.Weekdays, r.Timezone = s.EveryMinutes, s.MinuteOfDay, s.Weekdays, s.Timezone
	r.WarnMinutes, r.Enabled, r.NextRunAt, r.WarnedFor = s.WarnMinutes, s.Enabled, s.NextRunAt, nil
	if r.NextRunAt != nil {
		r.NextRunAt = ptrTime(*r.NextRunAt)
	}
	return nil
}

func (f *fakeSchedules) DeleteSchedule(_ context.Context, server string, id int64) error {
	if _, err := f.idle(server, id); err != nil {
		return err
	}
	delete(f.rows, id)
	return nil
}

func (f *fakeSchedules) DueSchedules(_ context.Context, horizon time.Time) ([]DueSchedule, error) {
	var out []DueSchedule
	for _, r := range f.rows {
		if r.RunState != "" || (r.Enabled && r.NextRunAt != nil && !r.NextRunAt.After(horizon)) {
			out = append(out, DueSchedule{Schedule: *cloneSchedule(r), ServerOwner: f.owners[r.Server]})
		}
	}
	slices.SortFunc(out, func(a, b DueSchedule) int {
		if (a.RunState == "") != (b.RunState == "") {
			if a.RunState != "" {
				return -1
			}
			return 1
		}
		if a.NextRunAt != nil && b.NextRunAt != nil && !a.NextRunAt.Equal(*b.NextRunAt) {
			return a.NextRunAt.Compare(*b.NextRunAt)
		}
		return int(a.ID - b.ID)
	})
	return out, nil
}

func (f *fakeSchedules) ClaimScheduleRun(_ context.Context, id int64, due, next *time.Time, now time.Time) (bool, error) {
	r, ok := f.rows[id]
	if !ok || r.RunState != "" {
		return false, nil
	}
	if due != nil {
		if !r.Enabled || !sameTime(r.NextRunAt, *due) {
			return false, nil
		}
		r.NextRunAt, r.WarnedFor = ptrTime(*next), nil
	}
	f.claims++
	r.RunState, r.RunResume, r.RunStepAt = runClaimed, false, ptrTime(now)
	r.LastRunAt, r.LastResult, r.LastDetail = ptrTime(now), "", ""
	return true, nil
}

func (f *fakeSchedules) AdvanceScheduleRun(_ context.Context, id int64, from, to string, resume bool, result, detail string, now time.Time) (bool, error) {
	r, ok := f.rows[id]
	if !ok || r.RunState != from {
		return false, nil
	}
	r.RunState, r.RunResume, r.RunStepAt = to, resume, ptrTime(now)
	if result != "" {
		r.LastResult, r.LastDetail = result, detail
	}
	return true, nil
}

func (f *fakeSchedules) FinishScheduleRun(_ context.Context, id int64, from, result, detail string) (bool, error) {
	r, ok := f.rows[id]
	if !ok || from == "" || r.RunState != from {
		return false, nil
	}
	r.RunState, r.RunResume, r.RunStepAt = "", false, nil
	r.LastResult, r.LastDetail = result, detail
	return true, nil
}

func (f *fakeSchedules) MissScheduleRun(_ context.Context, id int64, due, next time.Time, detail string) (bool, error) {
	r, ok := f.rows[id]
	if !ok || r.RunState != "" || !r.Enabled || !sameTime(r.NextRunAt, due) {
		return false, nil
	}
	r.NextRunAt, r.WarnedFor = ptrTime(next), nil
	r.LastRunAt, r.LastResult, r.LastDetail = ptrTime(due), ScheduleMissed, detail
	return true, nil
}

func (f *fakeSchedules) WarnScheduleRun(_ context.Context, id int64, due time.Time) (bool, error) {
	r, ok := f.rows[id]
	if !ok || r.RunState != "" || !r.Enabled || !sameTime(r.NextRunAt, due) || sameTime(r.WarnedFor, due) {
		return false, nil
	}
	r.WarnedFor = ptrTime(due)
	return true, nil
}

func (f *fakeSchedules) DisableSchedule(_ context.Context, id int64, detail string) (bool, error) {
	r, ok := f.rows[id]
	if !ok || r.RunState != "" || !r.Enabled {
		return false, nil
	}
	r.Enabled, r.NextRunAt, r.WarnedFor = false, nil, nil
	r.LastResult, r.LastDetail = ScheduleSkipped, detail
	return true, nil
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// TestScheduleNextRun pins the run times: daily and repeating rules, the
// weekday mask read in the schedule's zone, and the two daylight-saving edges.
func TestScheduleNextRun(t *testing.T) {
	sh := mustZone(t, "Asia/Shanghai")
	ny := mustZone(t, "America/New_York")
	const everyDay, weekdaysOnly, monday, saturday = 0x7f, 0x3e, 1 << 1, 1 << 6
	cases := []struct {
		name  string
		s     Schedule
		after time.Time
		want  time.Time
	}{
		{"daily, later today",
			Schedule{MinuteOfDay: 4*60 + 30, Weekdays: everyDay, Timezone: "Asia/Shanghai"},
			time.Date(2026, 9, 28, 1, 0, 0, 0, sh), time.Date(2026, 9, 28, 4, 30, 0, 0, sh)},
		{"daily, today's run is past",
			Schedule{MinuteOfDay: 4 * 60, Weekdays: everyDay, Timezone: "Asia/Shanghai"},
			time.Date(2026, 9, 28, 4, 0, 0, 0, sh), time.Date(2026, 9, 29, 4, 0, 0, 0, sh)},
		{"weekday mask in the schedule's zone (Sunday 23:00 UTC is Monday in Shanghai)",
			Schedule{MinuteOfDay: 9 * 60, Weekdays: monday, Timezone: "Asia/Shanghai"},
			time.Date(2026, 9, 27, 23, 0, 0, 0, time.UTC), time.Date(2026, 9, 28, 9, 0, 0, 0, sh)},
		{"one weekday, next week, across the month end",
			Schedule{MinuteOfDay: 9 * 60, Weekdays: monday, Timezone: "Asia/Shanghai"},
			time.Date(2026, 9, 28, 10, 0, 0, 0, sh), time.Date(2026, 10, 5, 9, 0, 0, 0, sh)},
		{"interval: the next multiple since midnight",
			Schedule{EveryMinutes: 180, Weekdays: everyDay, Timezone: "Asia/Shanghai"},
			time.Date(2026, 9, 28, 7, 10, 0, 0, sh), time.Date(2026, 9, 28, 9, 0, 0, 0, sh)},
		{"interval skips the days off (Friday night -> Monday 00:00)",
			Schedule{EveryMinutes: 720, Weekdays: weekdaysOnly, Timezone: "Asia/Shanghai"},
			time.Date(2026, 10, 2, 12, 0, 0, 0, sh), time.Date(2026, 10, 5, 0, 0, 0, 0, sh)},
		{"interval, Saturday only, from the week before",
			Schedule{EveryMinutes: 15, Weekdays: saturday, Timezone: "UTC"},
			time.Date(2026, 9, 26, 23, 50, 0, 0, time.UTC), time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)},
		{"a time daylight saving skips runs an hour early",
			Schedule{MinuteOfDay: 2*60 + 30, Weekdays: everyDay, Timezone: "America/New_York"},
			time.Date(2026, 3, 8, 0, 0, 0, 0, ny), time.Date(2026, 3, 8, 6, 30, 0, 0, time.UTC)},
		{"a repeated time runs once: after the first 01:30 comes tomorrow's",
			Schedule{MinuteOfDay: 90, Weekdays: everyDay, Timezone: "America/New_York"},
			time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), time.Date(2026, 11, 2, 6, 30, 0, 0, time.UTC)},
		{"a zone no longer known runs in UTC",
			Schedule{MinuteOfDay: 60, Weekdays: everyDay, Timezone: "Gone/Zone"},
			time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC)},
		{"the first 01:30 on the day the clock goes back",
			Schedule{MinuteOfDay: 90, Weekdays: everyDay, Timezone: "America/New_York"},
			time.Date(2026, 11, 1, 0, 0, 0, 0, ny), time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.nextRun(tc.after); !got.Equal(tc.want) {
				t.Fatalf("nextRun(%s) = %s, want %s", tc.after, got, tc.want)
			}
		})
	}
}

// TestScheduleInputValidation walks the settings apply refuses, each with the
// one field that makes it wrong, and what a valid body stores.
func TestScheduleInputValidation(t *testing.T) {
	valid := func() scheduleInput {
		return scheduleInput{Action: ScheduleRestart, MinuteOfDay: 240, Weekdays: 0x7f, Timezone: "Asia/Shanghai", WarnMinutes: 5}
	}
	bad := []struct {
		name string
		edit func(*scheduleInput)
		code string
		msg  string
	}{
		{"label too long", func(in *scheduleInput) { in.Label = strings.Repeat("猫", 65) }, "bad_schedule", "label too long"},
		{"label with a newline", func(in *scheduleInput) { in.Label = "a\nb" }, "bad_schedule", "single line"},
		{"label with DEL", func(in *scheduleInput) { in.Label = "a\x7fb" }, "bad_schedule", "single line"},
		{"unknown action", func(in *scheduleInput) { in.Action = "reboot" }, "bad_schedule", "action must be"},
		{"command on a restart", func(in *scheduleInput) { in.Command = "say hi" }, "bad_schedule", "only a command schedule"},
		{"command action without a command", func(in *scheduleInput) { in.Action, in.WarnMinutes = ScheduleCommand, 0 }, "bad_request", "command is required"},
		{"two commands in one", func(in *scheduleInput) { in.Action, in.WarnMinutes, in.Command = ScheduleCommand, 0, "say a\nop me" }, "bad_request", "single line"},
		{"minute_of_day negative", func(in *scheduleInput) { in.MinuteOfDay = -1 }, "bad_schedule", "minute_of_day must be"},
		{"minute_of_day 1440", func(in *scheduleInput) { in.MinuteOfDay = 1440 }, "bad_schedule", "minute_of_day must be"},
		{"interval off the list", func(in *scheduleInput) { in.EveryMinutes, in.MinuteOfDay = 45, 0 }, "bad_schedule", "every_minutes must be"},
		{"restart every 30 minutes", func(in *scheduleInput) { in.EveryMinutes, in.MinuteOfDay = 30, 0 }, "bad_schedule", "at most every 60 minutes"},
		{"interval with a minute_of_day", func(in *scheduleInput) { in.EveryMinutes = 60 }, "bad_schedule", "no minute_of_day"},
		{"no weekday", func(in *scheduleInput) { in.Weekdays = 0 }, "bad_schedule", "weekdays"},
		{"weekday bit 7", func(in *scheduleInput) { in.Weekdays = 0x80 }, "bad_schedule", "weekdays"},
		{"empty timezone", func(in *scheduleInput) { in.Timezone = " " }, "bad_schedule", "IANA zone"},
		{"Local", func(in *scheduleInput) { in.Timezone = "Local" }, "bad_schedule", "IANA zone"},
		{"unknown timezone", func(in *scheduleInput) { in.Timezone = "Mars/Olympus" }, "bad_schedule", "unknown timezone"},
		{"warning off the list", func(in *scheduleInput) { in.WarnMinutes = 2 }, "bad_schedule", "warn_minutes must be"},
		{"warning on a command", func(in *scheduleInput) { in.Action, in.Command = ScheduleCommand, "say hi" }, "bad_schedule", "only a restart, stop or backup warns"},
		{"warning on a start", func(in *scheduleInput) { in.Action = ScheduleStart }, "bad_schedule", "only a restart, stop or backup warns"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			in := valid()
			tc.edit(&in)
			err := in.apply(&Schedule{})
			var he *apiError
			if !errors.As(err, &he) || he.status != http.StatusBadRequest || he.code != tc.code || !strings.Contains(he.msg, tc.msg) {
				t.Fatalf("apply = %v, want 400 %s containing %q", err, tc.code, tc.msg)
			}
		})
	}

	t.Run("valid bodies store trimmed values", func(t *testing.T) {
		in := scheduleInput{Label: "  每晚  ", Action: ScheduleCommand, Command: " /say 晚安 ", EveryMinutes: 15,
			Weekdays: 0x41, Timezone: " Europe/Berlin "}
		var s Schedule
		if err := in.apply(&s); err != nil {
			t.Fatal(err)
		}
		want := Schedule{Label: "每晚", Action: ScheduleCommand, Command: "say 晚安", EveryMinutes: 15,
			Weekdays: 0x41, Timezone: "Europe/Berlin", Enabled: true}
		if s != want {
			t.Fatalf("stored %+v, want %+v", s, want)
		}
		off := false
		in = scheduleInput{Label: strings.Repeat("猫", 64), Action: ScheduleBackup, MinuteOfDay: 1439, Weekdays: 1,
			Timezone: "UTC", WarnMinutes: 30, Enabled: &off}
		if err := in.apply(&s); err != nil {
			t.Fatal(err)
		}
		if s.Enabled || s.Command != "" || s.WarnMinutes != 30 || s.MinuteOfDay != 1439 {
			t.Fatalf("stored %+v", s)
		}
	})
}

// schedRig is a server with a schedule store: survival, owned by owner1,
// running, at 2026-09-28 03:00 UTC.
type schedRig struct {
	a     *API
	repo  *fakeRepo
	cl    *fakeCluster
	st    *fakeSchedules
	con   *fakeConsole
	b     *fakeScheduledBackuper
	jobs  *fakeJobStatus
	clock time.Time
}

var schedT0 = time.Date(2026, 9, 28, 3, 0, 0, 0, time.UTC)

func newSchedRig(t *testing.T) *schedRig {
	t.Helper()
	r := &schedRig{repo: newFakeRepo(), cl: newFakeCluster(), st: newFakeSchedules(), con: &fakeConsole{},
		b: &fakeScheduledBackuper{}, jobs: &fakeJobStatus{}, clock: schedT0}
	r.repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
	r.cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: string(v1alpha1.PhaseRunning), Ready: true,
		DesiredState: string(v1alpha1.DesiredRunning)}
	r.st.owners["survival"] = "owner1"
	r.a = newTestAPI(r.repo, r.cl)
	r.a.Now = func() time.Time { return r.clock }
	r.a.Schedules, r.a.Console, r.a.Backuper, r.a.JobStatus = r.st, r.con, r.b, r.jobs
	return r
}

func (r *schedRig) stopped() {
	info := r.cl.byName["survival"]
	info.Phase, info.Ready, info.DesiredState = string(v1alpha1.PhaseStopped), false, string(v1alpha1.DesiredStopped)
}

// schedule stores a schedule of survival owned by owner1, due at due.
func (r *schedRig) schedule(action string, due time.Time, edit ...func(*Schedule)) int64 {
	s := Schedule{Server: "survival", OwnerID: "owner1", Action: action, MinuteOfDay: due.Hour()*60 + due.Minute(),
		Weekdays: 0x7f, Timezone: "UTC", Enabled: true, NextRunAt: ptrTime(due), CreatedBy: "owner1@example.net"}
	if action == ScheduleCommand {
		s.Command = "say hi"
	}
	for _, e := range edit {
		e(&s)
	}
	return r.st.put(s)
}

func (r *schedRig) tick(t *testing.T) {
	t.Helper()
	if err := r.a.RunSchedules(context.Background()); err != nil {
		t.Fatalf("RunSchedules: %v", err)
	}
}

var (
	schedOwner    = &Principal{UserID: "owner1", Email: "owner1@example.net", Role: "user"}
	schedStranger = &Principal{UserID: "other", Email: "other@example.net", Role: "user"}
	schedAdmin    = &Principal{UserID: "adm", Email: "adm@example.net", Role: "admin", ViaAdminAccess: true}
)

func (r *schedRig) do(p *Principal, method, target, body string) *http.Response {
	r.a.External = staticExternal{p: p}
	var h map[string]string
	if body != "" {
		h = jsonHeader
	}
	return do(r.a.ExternalHandler(), method, target, body, h).Result()
}

func decodeSchedule(t *testing.T, res *http.Response) Schedule {
	t.Helper()
	var s Schedule
	if err := json.NewDecoder(res.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func schedErrCode(t *testing.T, res *http.Response) string {
	t.Helper()
	var raw map[string]map[string]string
	if err := json.NewDecoder(res.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	return raw["error"]["code"]
}

const schedBase = "/api/v1/servers/survival/schedules"

// TestScheduleRoutesGate: every schedule route answers 400 for a bad name, 404
// for an unknown server, 403 for a stranger and 503 without a store, before it
// reads anything else.
func TestScheduleRoutesGate(t *testing.T) {
	body := `{"action":"restart","minute_of_day":240,"weekdays":127,"timezone":"UTC"}`
	routes := []struct{ method, path, body string }{
		{"GET", "", ""}, {"POST", "", body}, {"PUT", "/1", body}, {"DELETE", "/1", ""}, {"POST", "/1/run", ""},
	}
	for _, rt := range routes {
		t.Run(rt.method+rt.path, func(t *testing.T) {
			r := newSchedRig(t)
			r.schedule(ScheduleRestart, schedT0.Add(time.Hour))
			check := func(p *Principal, target string, status int, code string) {
				t.Helper()
				res := r.do(p, rt.method, target, rt.body)
				if res.StatusCode != status || schedErrCode(t, res) != code {
					t.Fatalf("%s %s = %d, want %d %s", rt.method, target, res.StatusCode, status, code)
				}
			}
			check(schedOwner, "/api/v1/servers/Bad_Name/schedules"+rt.path, 400, "bad_name")
			check(schedOwner, "/api/v1/servers/nope/schedules"+rt.path, 404, "not_found")
			check(schedStranger, schedBase+rt.path, 403, "forbidden")
			r.a.Schedules = nil
			check(schedOwner, schedBase+rt.path, 503, "schedules_unavailable")
			if len(r.repo.audits) != 0 || r.con.calls != 0 {
				t.Fatalf("a refused request left audits %+v / %d console calls", r.repo.audits, r.con.calls)
			}
		})
	}
	t.Run("bad id", func(t *testing.T) {
		r := newSchedRig(t)
		for _, id := range []string{"0", "-1", "x"} {
			res := r.do(schedOwner, "DELETE", schedBase+"/"+id, "")
			if res.StatusCode != 400 || schedErrCode(t, res) != "bad_id" {
				t.Fatalf("DELETE %s = %d, want 400 bad_id", id, res.StatusCode)
			}
		}
	})
}

func TestScheduleCRUD(t *testing.T) {
	t.Run("list: empty is [] with the limit", func(t *testing.T) {
		r := newSchedRig(t)
		res := r.do(schedOwner, "GET", schedBase, "")
		raw, _ := io.ReadAll(res.Body)
		if res.StatusCode != 200 || string(raw) != `{"limit":20,"schedules":[],"server":"survival"}`+"\n" {
			t.Fatalf("GET = %d %s", res.StatusCode, raw)
		}
	})

	t.Run("owner creates: next run, owner binding, audit", func(t *testing.T) {
		r := newSchedRig(t) // 2026-09-28 03:00 UTC = 11:00 in Shanghai
		res := r.do(schedOwner, "POST", schedBase,
			`{"label":"夜间重启","action":"restart","minute_of_day":240,"weekdays":127,"timezone":"Asia/Shanghai","warn_minutes":5}`)
		if res.StatusCode != 201 {
			t.Fatalf("POST = %d", res.StatusCode)
		}
		got := decodeSchedule(t, res)
		want := time.Date(2026, 9, 28, 20, 0, 0, 0, time.UTC) // 04:00 on the 29th in Shanghai
		if got.ID != 1 || got.Label != "夜间重启" || !sameTime(got.NextRunAt, want) || !got.Enabled ||
			got.CreatedBy != "owner1@example.net" || got.LastRunAt != nil || got.RunState != "" {
			t.Fatalf("created %+v", got)
		}
		if row := r.st.row(t, 1); row.OwnerID != "owner1" {
			t.Fatalf("stored owner %q, want owner1", row.OwnerID)
		}
		if len(r.repo.audits) != 1 || r.repo.audits[0].Action != "schedule.create" ||
			r.repo.audits[0].Actor != "owner1@example.net" || r.repo.audits[0].ActorUserID != "owner1" || r.repo.audits[0].ServerName != "survival" ||
			string(r.repo.audits[0].Payload) != `{"action":"restart","schedule":1}` {
			t.Fatalf("audits %+v", r.repo.audits)
		}
	})

	t.Run("admin creates on an owned server: it belongs to the owner", func(t *testing.T) {
		r := newSchedRig(t)
		res := r.do(schedAdmin, "POST", schedBase, `{"action":"stop","minute_of_day":0,"weekdays":1,"timezone":"UTC","enabled":false}`)
		if res.StatusCode != 201 {
			t.Fatalf("POST = %d", res.StatusCode)
		}
		got := decodeSchedule(t, res)
		if got.NextRunAt != nil || got.Enabled || got.CreatedBy != "adm@example.net" {
			t.Fatalf("created %+v", got)
		}
		if row := r.st.row(t, got.ID); row.OwnerID != "owner1" {
			t.Fatalf("stored owner %q, want owner1", row.OwnerID)
		}
	})

	t.Run("bad settings and unknown fields are 400 and store nothing", func(t *testing.T) {
		r := newSchedRig(t)
		for body, code := range map[string]string{
			`{"action":"restart","minute_of_day":240,"weekdays":127,"timezone":"Nowhere/City"}`: "bad_schedule",
			`{"action":"command","weekdays":127,"timezone":"UTC"}`:                              "bad_request",
			`{"action":"restart","weekdays":127,"timezone":"UTC","owner_id":"me"}`:              "bad_request",
		} {
			res := r.do(schedOwner, "POST", schedBase, body)
			if res.StatusCode != 400 || schedErrCode(t, res) != code {
				t.Fatalf("POST %s = %d, want 400 %s", body, res.StatusCode, code)
			}
		}
		if len(r.st.rows) != 0 || len(r.repo.audits) != 0 {
			t.Fatalf("stored %d rows, %d audits", len(r.st.rows), len(r.repo.audits))
		}
	})

	t.Run("the 21st is 409 schedule_limit", func(t *testing.T) {
		r := newSchedRig(t)
		for range maxSchedulesPerServer {
			r.schedule(ScheduleStop, schedT0.Add(time.Hour))
		}
		res := r.do(schedOwner, "POST", schedBase, `{"action":"stop","weekdays":127,"timezone":"UTC"}`)
		if res.StatusCode != 409 || schedErrCode(t, res) != "schedule_limit" || len(r.st.rows) != maxSchedulesPerServer {
			t.Fatalf("POST = %d, rows %d", res.StatusCode, len(r.st.rows))
		}
	})

	t.Run("update: new settings, current owner, fresh next run and warning marker", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleRestart, schedT0.Add(time.Hour), func(s *Schedule) {
			s.OwnerID, s.WarnedFor = "previous", ptrTime(schedT0.Add(time.Hour))
		})
		res := r.do(schedOwner, "PUT", fmt.Sprintf("%s/%d", schedBase, id),
			`{"action":"command","command":"/save-all","every_minutes":30,"weekdays":127,"timezone":"UTC"}`)
		if res.StatusCode != 200 {
			t.Fatalf("PUT = %d", res.StatusCode)
		}
		got := decodeSchedule(t, res)
		row := r.st.row(t, id)
		if got.Action != ScheduleCommand || got.Command != "save-all" || !sameTime(got.NextRunAt, schedT0.Add(30*time.Minute)) ||
			row.OwnerID != "owner1" || row.WarnedFor != nil || row.Command != "save-all" {
			t.Fatalf("PUT answered %+v, stored %+v", got, row)
		}
		if len(r.repo.audits) != 1 || r.repo.audits[0].Action != "schedule.update" {
			t.Fatalf("audits %+v", r.repo.audits)
		}
	})

	t.Run("update and delete refuse a running or unknown schedule", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleRestart, schedT0.Add(time.Hour), func(s *Schedule) { s.RunState = runStopping })
		body := `{"action":"stop","weekdays":127,"timezone":"UTC"}`
		for _, c := range []struct{ method, path, body, code string }{
			{"PUT", fmt.Sprintf("/%d", id), body, "schedule_running"},
			{"DELETE", fmt.Sprintf("/%d", id), "", "schedule_running"},
			{"PUT", "/99", body, "not_found"},
			{"DELETE", "/99", "", "not_found"},
		} {
			res := r.do(schedOwner, c.method, schedBase+c.path, c.body)
			want := 409
			if c.code == "not_found" {
				want = 404
			}
			if res.StatusCode != want || schedErrCode(t, res) != c.code {
				t.Fatalf("%s %s = %d, want %d %s", c.method, c.path, res.StatusCode, want, c.code)
			}
		}
		if row := r.st.row(t, id); row.Action != ScheduleRestart || len(r.repo.audits) != 0 {
			t.Fatalf("refused writes changed %+v / audited %+v", row, r.repo.audits)
		}
	})

	t.Run("a schedule of another server is 404", func(t *testing.T) {
		r := newSchedRig(t)
		r.repo.byName["creative"] = &ServerRecord{Name: "creative", OwnerID: "owner1"}
		id := r.st.put(Schedule{Server: "creative", Action: ScheduleStop, Weekdays: 1, Timezone: "UTC"})
		res := r.do(schedOwner, "DELETE", fmt.Sprintf("%s/%d", schedBase, id), "")
		if res.StatusCode != 404 || len(r.st.rows) != 1 {
			t.Fatalf("DELETE = %d, rows %d", res.StatusCode, len(r.st.rows))
		}
	})

	t.Run("delete", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleStop, schedT0.Add(time.Hour))
		res := r.do(schedOwner, "DELETE", fmt.Sprintf("%s/%d", schedBase, id), "")
		if res.StatusCode != 204 || len(r.st.rows) != 0 {
			t.Fatalf("DELETE = %d, rows %d", res.StatusCode, len(r.st.rows))
		}
		if len(r.repo.audits) != 1 || r.repo.audits[0].Action != "schedule.delete" ||
			string(r.repo.audits[0].Payload) != fmt.Sprintf(`{"action":"stop","schedule":%d}`, id) {
			t.Fatalf("audits %+v", r.repo.audits)
		}
	})

	t.Run("list shows the schedules oldest first", func(t *testing.T) {
		r := newSchedRig(t)
		a := r.schedule(ScheduleStop, schedT0.Add(2*time.Hour))
		b := r.schedule(ScheduleStart, schedT0.Add(time.Hour))
		res := r.do(schedOwner, "GET", schedBase, "")
		var body struct {
			Schedules []Schedule `json:"schedules"`
		}
		if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Schedules) != 2 || body.Schedules[0].ID != a || body.Schedules[1].ID != b {
			t.Fatalf("listed %+v", body.Schedules)
		}
	})
}

// TestScheduleRunNow: a run on request starts at once, without the warning,
// whether or not the schedule is enabled, and leaves its next run alone.
func TestScheduleRunNow(t *testing.T) {
	t.Run("a command runs and the answer carries its outcome", func(t *testing.T) {
		r := newSchedRig(t)
		r.con.reply = "§aSaved the game"
		next := schedT0.Add(5 * time.Hour)
		id := r.schedule(ScheduleCommand, next, func(s *Schedule) { s.Command = "save-all" })
		res := r.do(schedOwner, "POST", fmt.Sprintf("%s/%d/run", schedBase, id), "")
		if res.StatusCode != 202 {
			t.Fatalf("run = %d", res.StatusCode)
		}
		got := decodeSchedule(t, res)
		if got.LastResult != ScheduleOK || got.LastDetail != "Saved the game" || got.RunState != "" ||
			!sameTime(got.LastRunAt, schedT0) || !sameTime(got.NextRunAt, next) {
			t.Fatalf("after the run %+v", got)
		}
		if r.con.gotCommand != "save-all" || r.con.calls != 1 {
			t.Fatalf("console ran %q (%d calls)", r.con.gotCommand, r.con.calls)
		}
		var actions []string
		for _, e := range r.repo.audits {
			actions = append(actions, e.Action+"/"+e.Actor)
		}
		if strings.Join(actions, ",") != "schedule.run_now/owner1@example.net,schedule.run/scheduler" {
			t.Fatalf("audits %v", actions)
		}
	})

	t.Run("a disabled restart starts and goes on in the background", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleRestart, schedT0, func(s *Schedule) { s.Enabled, s.NextRunAt, s.WarnMinutes = false, nil, 5 })
		res := r.do(schedOwner, "POST", fmt.Sprintf("%s/%d/run", schedBase, id), "")
		got := decodeSchedule(t, res)
		if res.StatusCode != 202 || got.RunState != runStopping || got.NextRunAt != nil {
			t.Fatalf("run = %d %+v", res.StatusCode, got)
		}
		if r.cl.desired["survival"] != v1alpha1.DesiredStopped || r.con.calls != 0 {
			t.Fatalf("desired %q, %d console calls (no warning on request)", r.cl.desired["survival"], r.con.calls)
		}
	})

	t.Run("a running schedule is 409 schedule_running", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleRestart, schedT0, func(s *Schedule) { s.RunState = runStarting })
		res := r.do(schedOwner, "POST", fmt.Sprintf("%s/%d/run", schedBase, id), "")
		if res.StatusCode != 409 || schedErrCode(t, res) != "schedule_running" || len(r.repo.audits) != 0 {
			t.Fatalf("run = %d, audits %+v", res.StatusCode, r.repo.audits)
		}
	})

	t.Run("a schedule of the previous owner is 409 schedule_stale", func(t *testing.T) {
		r := newSchedRig(t)
		id := r.schedule(ScheduleCommand, schedT0, func(s *Schedule) { s.OwnerID = "previous" })
		res := r.do(schedAdmin, "POST", fmt.Sprintf("%s/%d/run", schedBase, id), "")
		if res.StatusCode != 409 || schedErrCode(t, res) != "schedule_stale" || r.con.calls != 0 || r.st.claims != 0 {
			t.Fatalf("run = %d, console %d, claims %d", res.StatusCode, r.con.calls, r.st.claims)
		}
	})

	t.Run("unknown schedule is 404", func(t *testing.T) {
		r := newSchedRig(t)
		res := r.do(schedOwner, "POST", schedBase+"/7/run", "")
		if res.StatusCode != 404 {
			t.Fatalf("run = %d", res.StatusCode)
		}
	})
}
