package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
)

func crashStoppedServer() *ServerInfo {
	return &ServerInfo{Name: "survival", AutostartPolicy: "public",
		DesiredState: string(v1alpha1.DesiredStopped), Phase: string(v1alpha1.PhaseStopped),
		AutoStop: &AutoStopInfo{Reason: v1alpha1.StopCrashLimit}, CrashStopped: true}
}

func TestACrashStoppedServerStartsOnlyByItsOwnerOrAnAdmin(t *testing.T) {
	mk := func(p *Principal) (*API, *fakeCluster) {
		repo := newFakeRepo()
		repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		cl := newFakeCluster()
		cl.byName["survival"] = crashStoppedServer()
		api := newTestAPI(repo, cl)
		api.External = staticExternal{p: p}
		return api, cl
	}
	api, cl := mk(&Principal{UserID: "stranger", Role: "user"})
	w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil)
	if w.Code != http.StatusConflict || decodeErr(t, w) != "start_failed" {
		t.Fatalf("a player on a public server: %d %s, want 409 start_failed", w.Code, w.Body.String())
	}
	if _, set := cl.desired["survival"]; set {
		t.Fatal("the refused wake started the server")
	}
	for _, p := range []*Principal{{UserID: "owner1", Role: "user"}, {UserID: "staff", Role: "admin", ViaAdminAccess: true}} {
		api, cl := mk(p)
		if w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != http.StatusAccepted {
			t.Fatalf("%s (%s): %d %s, want 202", p.UserID, p.Role, w.Code, w.Body.String())
		}
		if cl.desired["survival"] != v1alpha1.DesiredRunning {
			t.Fatalf("%s did not start it", p.UserID)
		}
	}
}

func TestAJoinDoesNotWakeACrashStoppedServer(t *testing.T) {
	api, cl := newInternalWakeAPI("public")
	cl.byName["survival"] = crashStoppedServer()
	w := internalWake(api, `{"mc_uuid":"`+wakeUUID+`"}`)
	if w.Code != http.StatusConflict || decodeErr(t, w) != "start_failed" {
		t.Fatalf("code = %d body %s, want 409 start_failed", w.Code, w.Body.String())
	}
	if _, set := cl.desired["survival"]; set {
		t.Fatal("a join started a server that stopped for crashing")
	}
	held := crashStoppedServer()
	held.AutoStop, held.CrashStopped = &AutoStopInfo{Reason: v1alpha1.StopHostRestart}, false
	cl.byName["survival"] = held
	if w := internalWake(api, `{"mc_uuid":"`+wakeUUID+`"}`); w.Code != http.StatusAccepted {
		t.Fatalf("a server held back after a host restart: %d %s, want a join to wake it", w.Code, w.Body.String())
	}
}

func TestPatchServerRecoveryAndNotify(t *testing.T) {
	api, _, cl, _ := newPatchAPI()
	w := patchSurvival(api, `{"resumeAfterHostRestart":false,"restartOnCrash":true,"maxCrashRestarts":5,"notify":{"started":true,"problems":false}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
	}
	info := cl.byName["survival"]
	if info.ResumeAfterHostRestart || !info.RestartOnCrash || info.MaxCrashRestarts != 5 {
		t.Fatalf("recovery = resume %v restart %v max %d", info.ResumeAfterHostRestart, info.RestartOnCrash, info.MaxCrashRestarts)
	}
	if info.Notify != (ServerNotify{Started: true}) {
		t.Fatalf("notify = %+v, want only started", info.Notify)
	}
	if p := cl.patched["survival"]; p.NotifyStopped != nil {
		t.Fatal("an untouched notify switch was written")
	}
	for _, body := range []string{`{"maxCrashRestarts":0}`, `{"maxCrashRestarts":21}`} {
		api, _, cl, _ := newPatchAPI()
		if w := patchSurvival(api, body); w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_crash_restarts" {
			t.Fatalf("%s: %d %s, want 400 bad_crash_restarts", body, w.Code, w.Body.String())
		}
		if _, patched := cl.patched["survival"]; patched {
			t.Fatalf("%s reached the cluster", body)
		}
	}
}

func TestOwnersSetTheirServersRecovery(t *testing.T) {
	a, repo, cl := newSelfServiceAPI()
	enableDefaultQuota(repo, 1, 4096)
	if status, _, body := selfCreate(a, "mine", "2Gi"); status != http.StatusCreated {
		t.Fatalf("create = %d (%s)", status, body)
	}
	if w := do(a.ExternalHandler(), "PATCH", "/api/v1/me/servers/mine", `{"restartOnCrash":false,"notify":{"stopped":true}}`, nil); w.Code != http.StatusOK {
		t.Fatalf("owner patch = %d %s", w.Code, w.Body.String())
	}
	p := cl.patched["mine"]
	if p.RestartOnCrash == nil || *p.RestartOnCrash || p.NotifyStopped == nil || !*p.NotifyStopped {
		t.Fatalf("patch = %+v", p)
	}
}

func TestSystemServersKeepTheirRecovery(t *testing.T) {
	api, _, cl, _ := newPatchAPI()
	cl.byName["lobby"] = &ServerInfo{Name: "lobby", ReaperExempt: true}
	for _, body := range []string{`{"restartOnCrash":false}`, `{"resumeAfterHostRestart":false}`, `{"notify":{"started":true}}`} {
		if w := do(api.ExternalHandler(), "PATCH", "/api/v1/servers/lobby", body, jsonHeader); w.Code != http.StatusBadRequest {
			t.Fatalf("%s on the lobby: %d %s, want 400", body, w.Code, w.Body.String())
		}
	}
}

// noticeAPI mails lifecycle events of survival, owned by owner@example.net, at a fixed clock.
func noticeAPI(t *testing.T, notify ServerNotify, events ...ServerEvent) (*API, *fakeCluster, *noticeMailer, *time.Time) {
	t.Helper()
	repo := newFakeRepo()
	repo.noticeTo = map[string][]string{"survival": {"owner@example.net"}}
	cl := newFakeCluster()
	cl.feeds = []ServerEventFeed{{Name: "survival", DisplayName: "生存服", Notify: notify, Events: events}}
	api := newTestAPI(repo, cl)
	mailer := &noticeMailer{}
	api.Mailer = mailer
	clock := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	api.Now = func() time.Time { return clock }
	return api, cl, mailer, &clock
}

func tick(t *testing.T, api *API) {
	t.Helper()
	if err := api.NotifyServerEvents(context.Background()); err != nil {
		t.Fatal(err)
	}
}

var problemsOnly = ServerNotify{Problems: true}

func TestACrashMailWaitsForTheServerToComeBack(t *testing.T) {
	at := time.Date(2026, 10, 21, 7, 59, 0, 0, time.UTC)
	api, cl, mailer, clock := noticeAPI(t, problemsOnly,
		ServerEvent{Seq: 4, Type: v1alpha1.EventCrashed, At: at, Reason: "Error", ExitCode: 1, Crashes: 1, Limit: 3})
	tick(t, api)
	if len(mailer.notices) != 0 || cl.mailed["survival"] != 0 {
		t.Fatalf("mailed %d, marked %d one minute after the crash; want a wait for the outcome", len(mailer.notices), cl.mailed["survival"])
	}
	cl.feeds[0].Events = append(cl.feeds[0].Events, ServerEvent{Seq: 5, Type: v1alpha1.EventStarted, At: clock.Add(-10 * time.Second)})
	tick(t, api)
	if len(mailer.notices) != 1 || cl.mailed["survival"] != 5 {
		t.Fatalf("mailed %d, marked %d; want one mail for both events", len(mailer.notices), cl.mailed["survival"])
	}
	n := mailer.notices[0]
	for _, want := range []string{"owner@example.net|", "进程意外退出", "退出码 1", "连续第 1 次，上限 3 次", "服务器已启动", "exit code 1", "/servers/survival"} {
		if !strings.Contains(n, want) {
			t.Errorf("mail lacks %q:\n%s", want, n)
		}
	}
	tick(t, api)
	if len(mailer.notices) != 1 {
		t.Fatal("the same events were mailed twice")
	}
}

func TestACrashThatStopsTheServerIsMailedAtOnce(t *testing.T) {
	api, cl, mailer, clock := noticeAPI(t, ServerNotify{Stopped: true, Problems: true})
	cl.feeds[0].Events = []ServerEvent{
		{Seq: 1, Type: v1alpha1.EventCrashed, At: clock.Add(-30 * time.Second), Reason: "OOMKilled", ExitCode: 137, Crashes: 4, Limit: 3, Stopping: true},
		{Seq: 2, Type: v1alpha1.EventStopped, At: clock.Add(-5 * time.Second), Reason: v1alpha1.StopCrashLimit},
	}
	tick(t, api)
	if len(mailer.notices) != 1 {
		t.Fatalf("mailed %d, want 1", len(mailer.notices))
	}
	n := mailer.notices[0]
	subject := strings.SplitN(n, "|", 3)[1]
	if !strings.Contains(subject, "崩溃后已停止") || !strings.Contains(subject, "stopped after crashing") {
		t.Errorf("subject = %q", subject)
	}
	if !strings.Contains(n, "内存不足") || !strings.Contains(n, "超过自动重启上限 3 次") {
		t.Errorf("body lacks the cause or the limit:\n%s", n)
	}
	if strings.Contains(n, "服务器在崩溃后停止") {
		t.Errorf("the stop is mailed again on its own line:\n%s", n)
	}
}

func TestStartsAndStopsAreMailedOnlyWhenTurnedOn(t *testing.T) {
	events := func(now time.Time) []ServerEvent {
		return []ServerEvent{
			{Seq: 1, Type: v1alpha1.EventStarted, At: now.Add(-2 * time.Minute)},
			{Seq: 2, Type: v1alpha1.EventStopped, At: now.Add(-time.Minute), Reason: v1alpha1.StopIdle},
		}
	}
	api, cl, mailer, clock := noticeAPI(t, problemsOnly)
	cl.feeds[0].Events = events(*clock)
	tick(t, api)
	if len(mailer.notices) != 0 || cl.mailed["survival"] != 2 {
		t.Fatalf("problems only: mailed %d, marked %d; want none mailed and both handled", len(mailer.notices), cl.mailed["survival"])
	}
	api, cl, mailer, clock = noticeAPI(t, ServerNotify{Started: true, Stopped: true})
	cl.feeds[0].Events = events(*clock)
	tick(t, api)
	if len(mailer.notices) != 1 || !strings.Contains(mailer.notices[0], "服务器已启动") || !strings.Contains(mailer.notices[0], "无人在线") {
		t.Fatalf("starts and stops on: %q", mailer.notices)
	}
}

func TestStaleEventsAreSkipped(t *testing.T) {
	api, cl, mailer, clock := noticeAPI(t, problemsOnly)
	cl.feeds[0].Events = []ServerEvent{{Seq: 9, Type: v1alpha1.EventHostRestarted, At: clock.Add(-2 * time.Hour)}}
	tick(t, api)
	if len(mailer.notices) != 0 || cl.mailed["survival"] != 9 {
		t.Fatalf("mailed %d, marked %d; want an event from two hours ago skipped", len(mailer.notices), cl.mailed["survival"])
	}
}

func TestAHostRestartIsMailed(t *testing.T) {
	api, cl, mailer, clock := noticeAPI(t, problemsOnly)
	cl.feeds[0].Events = []ServerEvent{{Seq: 1, Type: v1alpha1.EventHostRestarted, At: clock.Add(-time.Minute), Stopping: true}}
	tick(t, api)
	if len(mailer.notices) != 1 || !strings.Contains(mailer.notices[0], "主机重启后保持停止") || !strings.Contains(mailer.notices[0], "set not to resume") {
		t.Fatalf("notices = %q", mailer.notices)
	}
}

func TestSystemServersGetNoLifecycleMail(t *testing.T) {
	api, cl, mailer, clock := noticeAPI(t, problemsOnly)
	cl.feeds[0].System = true
	cl.feeds[0].Events = []ServerEvent{{Seq: 1, Type: v1alpha1.EventStartFailed, At: clock.Add(-time.Minute)}}
	tick(t, api)
	if len(mailer.notices) != 0 || cl.mailed["survival"] != 1 {
		t.Fatalf("mailed %d, marked %d", len(mailer.notices), cl.mailed["survival"])
	}
}

type failingNotices struct {
	noticeMailer
	fail bool
}

func (m *failingNotices) SendNotice(ctx context.Context, email, subject, body string) error {
	if m.fail {
		return errors.New("relay refused")
	}
	return m.noticeMailer.SendNotice(ctx, email, subject, body)
}

func TestAFailedSendIsRetried(t *testing.T) {
	api, cl, _, clock := noticeAPI(t, problemsOnly)
	mailer := &failingNotices{fail: true}
	api.Mailer = mailer
	cl.feeds[0].Events = []ServerEvent{{Seq: 3, Type: v1alpha1.EventStartFailed, At: clock.Add(-time.Minute), Reason: v1alpha1.ReasonStartupTimeout}}
	tick(t, api)
	if cl.mailed["survival"] != 0 {
		t.Fatal("events were marked handled though the mail failed")
	}
	mailer.fail = false
	tick(t, api)
	if len(mailer.notices) != 1 || cl.mailed["survival"] != 3 || !strings.Contains(mailer.notices[0], "启动超时") {
		t.Fatalf("after the relay recovered: %q marked %d", mailer.notices, cl.mailed["survival"])
	}
}

func TestEventsOfAServerWithNoReachableOwnerAreHandled(t *testing.T) {
	api, cl, mailer, clock := noticeAPI(t, problemsOnly)
	api.Repo.(*fakeRepo).noticeTo = nil
	cl.feeds[0].Events = []ServerEvent{{Seq: 2, Type: v1alpha1.EventStartFailed, At: clock.Add(-time.Minute)}}
	tick(t, api)
	if len(mailer.notices) != 0 || cl.mailed["survival"] != 2 {
		t.Fatalf("mailed %d, marked %d", len(mailer.notices), cl.mailed["survival"])
	}
}
