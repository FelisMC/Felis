package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"felis.lolicon.best/internal/archivepolicy"
	"felis.lolicon.best/internal/reaper"
)

func archiveAPI() (*API, *fakeRepo) {
	repo := newFakeRepo()
	a := newTestAPI(repo, newFakeCluster())
	host := reaper.DefaultConfig()
	host.ManualCooldown = time.Hour
	a.ArchivePolicy = &ArchivePolicy{Repo: repo, Host: host, HostAudit: 365 * 24 * time.Hour}
	a.External = staticExternal{p: &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true}}
	return a, repo
}

const archivePath = "/api/v1/settings/archive-policy"

func readArchivePolicy(t *testing.T, a *API) ArchivePolicyView {
	t.Helper()
	w := do(a.ExternalHandler(), "GET", archivePath, "", nil)
	var v ArchivePolicyView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatalf("read = %d %s", w.Code, w.Body.String())
	}
	return v
}

func putArchivePolicy(a *API, p archivepolicy.Policy, revision string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(struct {
		archivepolicy.Policy
		Revision string `json:"revision"`
	}{p, revision})
	return do(a.ExternalHandler(), "PUT", archivePath, string(body), jsonHeader)
}

func TestArchivePolicyRoutesAreOwnerOnly(t *testing.T) {
	for _, role := range []string{"user", "admin"} {
		a, repo := archiveAPI()
		a.External = staticExternal{p: &Principal{UserID: "x", Role: role, ViaAdminAccess: role == "admin"}}
		for _, method := range []string{"GET", "PUT", "DELETE"} {
			if w := do(a.ExternalHandler(), method, archivePath, `{}`, jsonHeader); w.Code != http.StatusForbidden {
				t.Errorf("%s %s = %d", role, method, w.Code)
			}
		}
		if len(repo.settings) != 0 {
			t.Fatalf("%s wrote the policy", role)
		}
	}
}

func TestArchivePolicyOverridesTheHostAndHandsItBack(t *testing.T) {
	a, _ := archiveAPI()
	ctx := context.Background()
	v := readArchivePolicy(t, a)
	if v.Source != "host" || v.ManualCooldownMinutes != 60 || v.ManualKeep != 5 || v.AuditRetentionDays != 365 {
		t.Fatalf("host view = %+v", v)
	}
	if a.backupCooldown(ctx) != time.Hour {
		t.Fatalf("host cooldown = %v", a.backupCooldown(ctx))
	}

	bad := v.Policy
	bad.ManualKeep = 0
	if w := putArchivePolicy(a, bad, v.Revision); w.Code != http.StatusBadRequest {
		t.Errorf("an invalid policy = %d %s", w.Code, w.Body.String())
	}
	next := v.Policy
	next.ManualCooldownMinutes, next.MaxStoreGiB, next.ScheduledEveryHours, next.AuditRetentionDays = 0, 7, 6, 0
	if w := putArchivePolicy(a, next, "stale"); w.Code != http.StatusConflict {
		t.Errorf("a stale revision = %d", w.Code)
	}
	w := putArchivePolicy(a, next, v.Revision)
	if w.Code != http.StatusOK {
		t.Fatalf("save = %d %s", w.Code, w.Body.String())
	}
	saved := readArchivePolicy(t, a)
	if saved.Source != "panel" || saved.Policy != next || saved.Revision == v.Revision {
		t.Fatalf("saved view = %+v", saved)
	}
	rc := a.ArchivePolicy.Reaper(ctx)
	if a.backupCooldown(ctx) != 0 || a.backupStoreCap(ctx) != 7<<30 || rc.ScheduledEvery != 6*time.Hour || a.ArchivePolicy.Audit(ctx) != 0 {
		t.Fatalf("in force after saving: cooldown %v, cap %d, every %v, audit %v", a.backupCooldown(ctx), a.backupStoreCap(ctx), rc.ScheduledEvery, a.ArchivePolicy.Audit(ctx))
	}
	if !hasAudit(a.Repo.(*fakeRepo), "platform.archive_policy") {
		t.Error("the save was not audited")
	}

	if w := do(a.ExternalHandler(), "DELETE", archivePath, "", nil); w.Code != http.StatusOK {
		t.Fatalf("clear = %d %s", w.Code, w.Body.String())
	}
	if v := readArchivePolicy(t, a); v.Source != "host" || v.ManualCooldownMinutes != 60 {
		t.Fatalf("after clearing = %+v", v)
	}
	if a.backupCooldown(ctx) != time.Hour {
		t.Fatal("felis.toml's cooldown is not back in force")
	}
}

func TestArchivePolicyAsksForAFreshProof(t *testing.T) {
	a, repo := archiveAPI()
	v := readArchivePolicy(t, a)
	p := &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true, ViaSession: true, EmailVerified: true}
	repo.passkeyCreds["owner-key"] = PasskeyCredential{ID: "owner-key", UserID: p.UserID, UserVerified: true}
	a.External = staticExternal{p: p}
	if w := putArchivePolicy(a, v.Policy, v.Revision); w.Code != http.StatusForbidden || decodeErr(t, w) != "reauth_required" {
		t.Errorf("save = %d %s", w.Code, w.Body.String())
	}
	if w := do(a.ExternalHandler(), "DELETE", archivePath, "", nil); w.Code != http.StatusForbidden || decodeErr(t, w) != "reauth_required" {
		t.Errorf("clear = %d %s", w.Code, w.Body.String())
	}
	if len(repo.settings) != 0 {
		t.Fatal("an unproven request wrote the policy")
	}
}

func TestBackupLimitsFollowThePolicy(t *testing.T) {
	a, repo := archiveAPI()
	repo.backupRequested = map[string]time.Time{"survival": a.now().Add(-time.Minute)}
	if err := a.backupAllowance(context.Background(), "survival"); err == nil {
		t.Fatal("felis.toml's hour-long cooldown let a backup through")
	}
	p := archivepolicy.FromConfig(a.ArchivePolicy.Host, a.ArchivePolicy.HostAudit)
	p.ManualCooldownMinutes = 0
	raw, _ := json.Marshal(p)
	repo.settings[archivepolicy.Key] = raw
	a.ArchivePolicy.invalidate()
	if err := a.backupAllowance(context.Background(), "survival"); err != nil {
		t.Fatalf("the panel's zero cooldown still refused: %v", err)
	}
}

func TestScheduledBackupsFollowThePolicy(t *testing.T) {
	a, repo := archiveAPI()
	a.Backuper = &liveScheduledBackuper{}
	jobs := &countingJobs{}
	s := &BackupScheduler{API: a, Store: &fakeScheduleStore{}, Jobs: jobs, Every: 24 * time.Hour}
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.calls != 1 {
		t.Fatalf("with felis.toml's interval the tick looked at %d job counts, want 1", jobs.calls)
	}
	p := archivepolicy.FromConfig(a.ArchivePolicy.Host, a.ArchivePolicy.HostAudit)
	p.ScheduledEveryHours = 0
	raw, _ := json.Marshal(p)
	repo.settings[archivepolicy.Key] = raw
	a.ArchivePolicy.invalidate()
	if err := s.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if jobs.calls != 1 {
		t.Fatal("the scheduler ran with the points switched off in the panel")
	}
}

type countingJobs struct{ calls int }

func (c *countingJobs) RunningWorldJobs(context.Context) (int, error) {
	c.calls++
	return 1, nil
}
