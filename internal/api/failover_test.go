package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/backupjob"
	"felis.lolicon.best/internal/distributed"
	"felis.lolicon.best/internal/nodecontrol"
)

var failoverT0 = time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

type failoverRig struct {
	a    *API
	repo *fakeRepo
	d    *fakeDistribution
	nc   *fakeNodeControl
	now  time.Time
}

// newFailoverRig includes recoverable, backup-less, migrating, missing-record and offline-node cases.
func newFailoverRig() *failoverRig {
	g := &failoverRig{repo: newFakeRepo(), nc: &fakeNodeControl{}, now: failoverT0}
	for _, name := range []string{"alice", "bob", "carol"} {
		g.repo.byName[name] = &ServerRecord{Name: name, OwnerID: "u-" + name}
	}
	backup := func(id string, age time.Duration, corrupt bool) fakeBackup {
		return fakeBackup{view: BackupView{ID: id, ServerName: "alice", Status: "present", CreatedAt: failoverT0.Add(-age), SizeBytes: int64(age / time.Minute), Corrupt: corrupt},
			ref: "/backups/" + id + ".tar.gz", sha256: strings.Repeat(id[len(id)-1:], 64)}
	}
	g.repo.backups = append(g.repo.backups, backup("bk-1", 3*time.Hour, false), backup("bk-2", 40*time.Minute, false), backup("bk-3", 5*time.Minute, true))
	lostSince := failoverT0.Add(-20 * time.Minute)
	g.d = &fakeDistribution{
		targets: map[string]bool{"a": true, "c": true},
		nodes: []distributed.Node{
			{Name: "a", Role: "controller", Ready: true, Addresses: []string{}},
			{Name: "b", Role: "worker", LostSince: &lostSince, Addresses: []string{}},
			{Name: "c", Role: "worker", Ready: true, Approved: true, Addresses: []string{}},
			{Name: "d", Role: "worker", Ready: true, Addresses: []string{}},
			{Name: "e", Role: "worker", Approved: true, Addresses: []string{}},
		},
		placed: map[string][]distributed.PlacedServer{"b": {
			{Name: "alice", DesiredState: "Running"},
			{Name: "bob", DesiredState: "Stopped"},
			{Name: "carol", DesiredState: "Stopped", Operation: &distributed.Operation{ID: "fo-carol", Kind: distributed.KindFailover, Server: "carol", State: "restoring"}},
			{Name: "ghost", DesiredState: "Stopped"},
		}},
	}
	g.a = newTestAPI(g.repo, newFakeCluster())
	g.a.Distribution, g.a.NodeControl = g.d, g.nc
	g.a.Now = func() time.Time { return g.now }
	g.a.External = staticExternal{p: &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true}}
	return g
}

func (g *failoverRig) failOver(body string) *httptest.ResponseRecorder {
	return do(g.a.ExternalHandler(), "POST", "/api/v1/nodes/b/failover", body, jsonHeader)
}

func TestFailoverRoutesAreOwnerOnly(t *testing.T) {
	for _, role := range []string{"user", "admin"} {
		g := newFailoverRig()
		g.a.External = staticExternal{p: &Principal{UserID: "x", Role: role, ViaAdminAccess: role == "admin"}}
		for _, tc := range []struct{ method, path, body string }{
			{"GET", "/api/v1/nodes/b/failover", ""},
			{"POST", "/api/v1/nodes/b/failover", `{"targetNode":"c","confirm":"b"}`},
			{"GET", "/api/v1/settings/failover", ""},
			{"PUT", "/api/v1/settings/failover", `{"automatic":true,"afterMinutes":15,"replicaMinutes":60}`},
		} {
			if w := do(g.a.ExternalHandler(), tc.method, tc.path, tc.body, jsonHeader); w.Code != http.StatusForbidden {
				t.Errorf("%s %s %s = %d", role, tc.method, tc.path, w.Code)
			}
		}
		if len(g.d.begun) != 0 || g.nc.calls != 0 || len(g.repo.settings) != 0 {
			t.Fatalf("%s reached a failover: begun %v, node tasks %d", role, g.d.begun, g.nc.calls)
		}
	}
}

func TestFailoverPlanShowsTheRecoveryPoint(t *testing.T) {
	g := newFailoverRig()
	w := do(g.a.ExternalHandler(), "GET", "/api/v1/nodes/b/failover", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("plan = %d %s", w.Code, w.Body.String())
	}
	var plan FailoverPlan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if !plan.Lost || plan.Fenced || plan.LostSince == nil || strings.Join(plan.Targets, ",") != "a,c" || len(plan.Servers) != 4 {
		t.Fatalf("plan = %+v", plan)
	}
	alice, bob, carol := plan.Servers[0], plan.Servers[1], plan.Servers[2]
	if alice.Backup == nil || alice.Backup.ID != "bk-2" || !alice.Backup.CreatedAt.Equal(failoverT0.Add(-40*time.Minute)) || alice.Backup.SizeBytes != 40 {
		t.Errorf("alice = %+v", alice.Backup)
	}
	if bob.Backup != nil || carol.Operation == nil || carol.Operation.ID != "fo-carol" {
		t.Errorf("bob = %+v, carol = %+v", bob, carol)
	}
	if strings.Contains(w.Body.String(), "/backups/") {
		t.Error("the plan leaks an archive path")
	}

	for path, want := range map[string]int{"/api/v1/nodes/zed/failover": 404, "/api/v1/nodes/-b/failover": 400} {
		if w := do(g.a.ExternalHandler(), "GET", path, "", nil); w.Code != want {
			t.Errorf("GET %s = %d, want %d", path, w.Code, want)
		}
	}
	g.d.nodes[1].Ready = true
	w = do(g.a.ExternalHandler(), "GET", "/api/v1/nodes/b/failover", "", nil)
	if json.Unmarshal(w.Body.Bytes(), &plan); plan.Lost {
		t.Error("an online worker reads as lost")
	}
}

func TestFailoverStartsEachServerAndFencesTheNode(t *testing.T) {
	g := newFailoverRig()
	for body, want := range map[string]string{
		`{"targetNode":"c","confirm":"c"}`: "confirm_mismatch",
		`{"targetNode":"d","confirm":"b"}`: "bad_node",
	} {
		if w := g.failOver(body); w.Code != http.StatusBadRequest || decodeErr(t, w) != want {
			t.Errorf("%s = %d %s", body, w.Code, w.Body.String())
		}
	}
	g.d.lostErr = distributed.ErrNodeNotLost
	if w := g.failOver(`{"targetNode":"c","confirm":"b"}`); w.Code != http.StatusConflict || decodeErr(t, w) != "node_not_lost" {
		t.Errorf("an online node = %d %s", w.Code, w.Body.String())
	}
	if len(g.d.begun) != 0 || g.nc.calls != 0 {
		t.Fatalf("a refused failover acted: %v, %d node tasks", g.d.begun, g.nc.calls)
	}

	g.d.lostErr = nil
	g.repo.byName["dave"] = &ServerRecord{Name: "dave", OwnerID: "u-dave"}
	g.repo.backups = append(g.repo.backups, fakeBackup{view: BackupView{ID: "bk-d", ServerName: "dave", Status: "present", CreatedAt: failoverT0}, ref: "/backups/bk-d.tar.gz"})
	g.d.placed["b"] = append(g.d.placed["b"], distributed.PlacedServer{Name: "dave"})
	g.d.beginErr = map[string]error{"dave": errors.New("target claim refused")}
	w := g.failOver(`{"targetNode":"c","confirm":"b"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("failover = %d %s", w.Code, w.Body.String())
	}
	if len(g.d.begun) != 1 {
		t.Fatalf("begun = %+v", g.d.begun)
	}
	if f := g.d.begun[0]; f.server != "alice" || f.target != "c" || f.owner != "u-alice" || f.backup.Ref != "/backups/bk-2.tar.gz" || f.backup.SHA256 != strings.Repeat("2", 64) || f.backup.Size != 40 {
		t.Errorf("alice's failover = %+v", f)
	}
	var res FailoverResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Operations) != 2 || res.Operations[0].Server != "alice" || res.Operations[1].ID != "fo-carol" {
		t.Errorf("operations = %+v", res.Operations)
	}
	skipped := map[string]string{}
	for _, s := range res.Skipped {
		skipped[s.Name] = s.Reason
	}
	if len(skipped) != 3 || skipped["bob"] != "no_backup" || skipped["ghost"] != "unknown_server" || skipped["dave"] != "failed" {
		t.Errorf("skipped = %+v", res.Skipped)
	}
	if g.nc.calls != 1 || g.nc.request.Action != "fence" || g.nc.request.Name != "b" || !g.nc.request.ConfirmMaintenance || res.FenceTask == nil {
		t.Errorf("fence = %d calls, %+v (task %v)", g.nc.calls, g.nc.request, res.FenceTask)
	}
	if !hasAudit(g.repo, "platform.node.failover") {
		t.Error("the failover was not audited")
	}

	g.d.placed["b"][2].Operation.State = "failed"
	g.repo.backups = append(g.repo.backups, fakeBackup{view: BackupView{ID: "bk-c", ServerName: "carol", Status: "present", CreatedAt: failoverT0}, ref: "/backups/bk-c.tar.gz"})
	g.d.begun = nil
	if w := g.failOver(`{"targetNode":"a","confirm":"b"}`); w.Code != http.StatusAccepted {
		t.Fatalf("second failover = %d %s", w.Code, w.Body.String())
	}
	if len(g.d.begun) != 2 || g.d.begun[1].server != "carol" || g.d.begun[1].target != "a" {
		t.Errorf("after carol's failover failed: begun %+v", g.d.begun)
	}
}

func TestFailoverAsksForAFreshProof(t *testing.T) {
	g := newFailoverRig()
	p := &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true, ViaSession: true, EmailVerified: true}
	g.repo.passkeyCreds["owner-key"] = PasskeyCredential{ID: "owner-key", UserID: p.UserID, UserVerified: true}
	g.a.External = staticExternal{p: p}
	if w := g.failOver(`{"targetNode":"c","confirm":"b"}`); w.Code != http.StatusForbidden || decodeErr(t, w) != "reauth_required" {
		t.Errorf("failover = %d %s", w.Code, w.Body.String())
	}
	if w := do(g.a.ExternalHandler(), "PUT", "/api/v1/settings/failover", `{"automatic":true,"afterMinutes":15,"replicaMinutes":60}`, jsonHeader); w.Code != http.StatusForbidden || decodeErr(t, w) != "reauth_required" {
		t.Errorf("setting = %d %s", w.Code, w.Body.String())
	}
	if len(g.d.begun) != 0 || len(g.repo.settings) != 0 {
		t.Fatal("an unproven request acted")
	}
}

func TestFailoverPolicySetting(t *testing.T) {
	g := newFailoverRig()
	read := func() failoverPolicyView {
		t.Helper()
		w := do(g.a.ExternalHandler(), "GET", "/api/v1/settings/failover", "", nil)
		var v failoverPolicyView
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
			t.Fatalf("read = %d %s", w.Code, w.Body.String())
		}
		return v
	}
	v := read()
	if v.Managed || v.Automatic || v.AfterMinutes != 15 || v.ReplicaMinutes != 60 {
		t.Fatalf("default = %+v", v)
	}
	put := func(body string) *httptest.ResponseRecorder {
		return do(g.a.ExternalHandler(), "PUT", "/api/v1/settings/failover", body, jsonHeader)
	}
	for _, bad := range []string{
		`{"automatic":true,"afterMinutes":9,"replicaMinutes":60,"revision":"` + v.Revision + `"}`,
		`{"automatic":true,"afterMinutes":1441,"replicaMinutes":60,"revision":"` + v.Revision + `"}`,
		`{"automatic":true,"afterMinutes":15,"replicaMinutes":14,"revision":"` + v.Revision + `"}`,
		`{"automatic":true,"afterMinutes":15,"replicaMinutes":1441,"revision":"` + v.Revision + `"}`,
	} {
		if w := put(bad); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d", bad, w.Code)
		}
	}
	if w := put(`{"automatic":true,"afterMinutes":15,"replicaMinutes":60,"revision":"stale"}`); w.Code != http.StatusConflict {
		t.Errorf("a stale revision = %d", w.Code)
	}
	if w := put(`{"automatic":true,"afterMinutes":30,"replicaMinutes":0,"revision":"` + v.Revision + `"}`); w.Code != http.StatusOK {
		t.Fatalf("save = %d %s", w.Code, w.Body.String())
	}
	if v := read(); !v.Managed || !v.Automatic || v.AfterMinutes != 30 || v.ReplicaMinutes != 0 {
		t.Fatalf("saved = %+v", v)
	}
	if !hasAudit(g.repo, "platform.failover_policy") {
		t.Error("the setting change was not audited")
	}
}

func (g *failoverRig) savePolicy(p FailoverPolicy) {
	raw, _ := json.Marshal(p)
	g.repo.settings[failoverPolicyKey] = raw
}

func TestFailoverTickFencesWhatFailoversWaitOn(t *testing.T) {
	g := newFailoverRig()
	g.savePolicy(FailoverPolicy{AfterMinutes: 15})
	g.d.pending = []string{"b"}
	tick := func() {
		t.Helper()
		if err := g.a.FailoverTick(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	tick()
	if g.nc.calls != 1 || g.nc.request.Name != "b" || g.nc.actor != failoverActor {
		t.Fatalf("first tick: %d calls, %+v by %q", g.nc.calls, g.nc.request, g.nc.actor)
	}
	g.now = g.now.Add(time.Minute)
	tick()
	if g.nc.calls != 1 {
		t.Fatal("a fence was started again within the retry window")
	}
	g.now = g.now.Add(fenceRetry)
	g.nc.tasks = []nodecontrol.Task{{State: "running", Request: nodecontrol.Request{Action: "fence", Name: "b"}}}
	tick()
	if g.nc.calls != 1 {
		t.Fatal("a fence was started beside a running one")
	}
	g.nc.tasks = nil
	tick()
	if g.nc.calls != 2 {
		t.Fatalf("no new fence after the window: %d calls", g.nc.calls)
	}
}

func TestAutomaticFailover(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*failoverRig)
		want   bool
	}{
		{"lost long enough", func(*failoverRig) {}, true},
		{"switched off", func(g *failoverRig) { g.savePolicy(FailoverPolicy{AfterMinutes: 15}) }, false},
		{"not lost long enough", func(g *failoverRig) { g.d.lost["b"] = failoverT0.Add(-14 * time.Minute) }, false},
		{"loss time unknown", func(g *failoverRig) { g.d.lost["b"] = time.Time{} }, false},
		{"two workers lost", func(g *failoverRig) { g.d.lost["e"] = failoverT0.Add(-time.Hour) }, false},
		{"controller down", func(g *failoverRig) { g.d.controllerUp = false }, false},
		{"every server already failing over", func(g *failoverRig) {
			for i := range g.d.placed["b"] {
				g.d.placed["b"][i].Operation = &distributed.Operation{Kind: distributed.KindFailover, State: "failed"}
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newFailoverRig()
			g.savePolicy(FailoverPolicy{Automatic: true, AfterMinutes: 15})
			g.d.lost, g.d.controllerUp, g.d.failTarget = map[string]time.Time{"b": failoverT0.Add(-20 * time.Minute)}, true, "c"
			tc.change(g)
			if err := g.a.FailoverTick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := len(g.d.begun) > 0; got != tc.want {
				t.Fatalf("failed over = %v (begun %+v), want %v", got, g.d.begun, tc.want)
			}
			if tc.want {
				if g.d.begun[0].target != "c" || g.nc.request.Action != "fence" || !hasAudit(g.repo, "platform.node.failover") {
					t.Errorf("automatic failover = %+v, fence %+v", g.d.begun, g.nc.request)
				}
			}
		})
	}
}

func TestReplicasKeepWorkerWorldsRecent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		age     time.Duration
		minutes int
		want    bool
	}{
		{"stale", 2 * time.Hour, 60, true},
		{"recent", 30 * time.Minute, 60, false},
		{"switched off", 2 * time.Hour, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLiveFixture()
			f.a.Now = func() time.Time { return failoverT0 }
			f.a.Distribution = &fakeDistribution{replicas: []string{"survival"}}
			repo := f.a.Repo.(*fakeRepo)
			repo.backups = append(repo.backups, fakeBackup{view: BackupView{ID: "bk-s", ServerName: "survival", Status: "present", CreatedAt: failoverT0.Add(-tc.age)}, ref: "/backups/bk-s.tar.gz"})
			raw, _ := json.Marshal(FailoverPolicy{AfterMinutes: 15, ReplicaMinutes: tc.minutes})
			repo.settings[failoverPolicyKey] = raw
			if err := f.a.FailoverTick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := len(f.backup.live) == 1; got != tc.want {
				t.Fatalf("replica started = %v (live %v)", got, f.backup.live)
			}
			if tc.want && (f.backup.reasons[0] != backupjob.ReasonReplica || !hasAudit(repo, replicaAction)) {
				t.Errorf("reasons %v", f.backup.reasons)
			}
			if !tc.want {
				return
			}
			delete(f.cl.freezes, "survival")
			if err := f.a.FailoverTick(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(f.backup.live) != 1 {
				t.Fatalf("a second replica within the interval: %v", f.backup.live)
			}
		})
	}
}

func hasAudit(repo *fakeRepo, action string) bool {
	for _, e := range repo.audits {
		if e.Action == action {
			return true
		}
	}
	return false
}
