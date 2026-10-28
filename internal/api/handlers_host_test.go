package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/nodecontrol"
)

// hostOverviewFixture is an overview as node-control writes it.
const hostOverviewFixture = `{"host":"ctl-a","at":"2026-10-12T00:00:00Z","version":"v1.0.0","disks":[{"path":"/var","total":100,"available":40}],` +
	`"memory":{"total":8,"available":4},"units":[{"name":"felis-db-backup.timer","active":"active","next":"2026-10-13T03:30:00Z"}],"proxy":{"address":"127.0.0.1:25565","reachable":true},` +
	`"alerts":[],"databaseBackups":[{"name":"felis-db-20261012T033000Z-daily.tar","label":"daily","size":1024,"created":"2026-10-12T03:30:00Z"}],` +
	`"offsite":{"configured":false,"keyKept":false},"settings":{"dbBackupTime":"03:30","dbBackupKeep":14,"velocityXmx":"1G"},` +
	`"connection":{"mode":"local","panelHost":"console.example.com","adminHost":"op.console.example.com"},"storage":{"mode":"local"},"rootDomain":"example.com"}`

func TestHostRoutesAreOwnerOnly(t *testing.T) {
	for _, role := range []string{"user", "admin"} {
		a := newTestAPI(newFakeRepo(), newFakeCluster())
		a.External = staticExternal{p: &Principal{UserID: "actor", Role: role, ViaAdminAccess: role != "user"}}
		control := &fakeNodeControl{host: json.RawMessage(hostOverviewFixture), key: "k", file: "bundle"}
		a.NodeControl = control
		a.AuditExport = func(context.Context, time.Time, time.Time, io.Writer) (int, error) { return 0, nil }
		for _, tc := range []struct{ method, path, body string }{
			{"GET", "/api/v1/platform/host", ""},
			{"POST", "/api/v1/platform/host/quiet", `{"until":null}`},
			{"GET", "/api/v1/settings/node-control/tasks/task/artifact", ""},
			{"POST", "/api/v1/platform/offsite/key", ""},
			{"GET", "/api/v1/platform/audit", ""},
			{"GET", "/api/v1/platform/audit-export", ""},
		} {
			if w := do(a.ExternalHandler(), tc.method, tc.path, tc.body, jsonHeader); w.Code != http.StatusForbidden {
				t.Fatalf("%s %s %s = %d", role, tc.method, tc.path, w.Code)
			}
		}
		if len(control.quiet) != 0 {
			t.Fatal("a non-Owner held the watchdog")
		}
	}
}

func TestHostRoutesAskForAFreshProof(t *testing.T) {
	repo := newFakeRepo()
	a := newTestAPI(repo, newFakeCluster())
	p := &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true, ViaSession: true, EmailVerified: true}
	repo.passkeyCreds["owner-key"] = PasskeyCredential{ID: "owner-key", UserID: p.UserID, UserVerified: true}
	a.External = staticExternal{p: p}
	control := &fakeNodeControl{host: json.RawMessage(hostOverviewFixture), key: "k", file: "bundle"}
	a.NodeControl = control
	a.AuditExport = func(context.Context, time.Time, time.Time, io.Writer) (int, error) { return 0, nil }
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/v1/platform/host/quiet", `{"until":null}`},
		{"GET", "/api/v1/settings/node-control/tasks/task/artifact", ""},
		{"POST", "/api/v1/platform/offsite/key", ""},
		{"GET", "/api/v1/platform/audit-export", ""},
	} {
		if w := do(a.ExternalHandler(), tc.method, tc.path, tc.body, jsonHeader); w.Code != http.StatusForbidden || decodeErr(t, w) != "reauth_required" {
			t.Fatalf("%s %s = %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	if w := do(a.ExternalHandler(), "GET", "/api/v1/platform/host", "", nil); w.Code != http.StatusOK || w.Body.String() != hostOverviewFixture {
		t.Fatalf("overview = %d %s", w.Code, w.Body.String())
	}
	if w := do(a.ExternalHandler(), "GET", "/api/v1/platform/audit", "", nil); w.Code != http.StatusOK {
		t.Fatalf("browsing the audit log without a fresh proof = %d %s", w.Code, w.Body.String())
	}
	p.ReauthAt = a.now()
	if w := do(a.ExternalHandler(), "POST", "/api/v1/platform/offsite/key", "", jsonHeader); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"key":"k"`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("key = %d %s %v", w.Code, w.Body.String(), w.Header())
	}
	if !hasAudit(repo, "platform.host.offsite-key") {
		t.Fatal("revealing the off-site key was not audited")
	}
}

func ownerAPI() (*API, *fakeRepo, *fakeNodeControl) {
	repo := newFakeRepo()
	a := newTestAPI(repo, newFakeCluster())
	a.External = staticExternal{p: &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true}}
	control := &fakeNodeControl{}
	a.NodeControl = control
	return a, repo, control
}

func TestHostQuietTakesAHoldWithinAWeek(t *testing.T) {
	a, repo, control := ownerAPI()
	post := func(body string) int {
		return do(a.ExternalHandler(), "POST", "/api/v1/platform/host/quiet", body, jsonHeader).Code
	}
	at := func(d time.Duration) string {
		return fmt.Sprintf(`{"until":%q}`, time.Now().Add(d).UTC().Format(time.RFC3339))
	}
	if code := post(at(-time.Minute)); code != http.StatusBadRequest {
		t.Fatalf("a hold in the past = %d", code)
	}
	if code := post(at(8 * 24 * time.Hour)); code != http.StatusBadRequest {
		t.Fatalf("a hold past a week = %d", code)
	}
	if len(control.quiet) != 0 {
		t.Fatal("a refused hold reached the host")
	}
	if code := post(at(2 * time.Hour)); code != http.StatusNoContent {
		t.Fatalf("a two-hour hold = %d", code)
	}
	if code := post(`{"until":null}`); code != http.StatusNoContent {
		t.Fatalf("lifting = %d", code)
	}
	if len(control.quiet) != 2 || control.quiet[0].Before(time.Now().Add(time.Hour)) || !control.quiet[1].IsZero() {
		t.Fatalf("holds = %v", control.quiet)
	}
	if !hasAudit(repo, "platform.host.quiet") {
		t.Fatal("the hold was not audited")
	}
}

func TestNodeTaskArtifactDownloads(t *testing.T) {
	a, repo, control := ownerAPI()
	if w := do(a.ExternalHandler(), "GET", "/api/v1/settings/node-control/tasks/task/artifact", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("no file = %d", w.Code)
	}
	control.file = "tarball"
	w := do(a.ExternalHandler(), "GET", "/api/v1/settings/node-control/tasks/task/artifact", "", nil)
	if w.Code != http.StatusOK || w.Body.String() != "tarball" {
		t.Fatalf("download = %d %q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Disposition"); got != `attachment; filename="felis-support-task.tar.gz"` {
		t.Fatalf("disposition = %q", got)
	}
	if w.Header().Get("Content-Type") != "application/gzip" || w.Header().Get("Content-Length") != "7" {
		t.Fatalf("headers = %v", w.Header())
	}
	if !hasAudit(repo, "platform.host.download") {
		t.Fatal("the download was not audited")
	}
}

func TestAuditExportReadsAPeriod(t *testing.T) {
	a, _, _ := ownerAPI()
	if w := do(a.ExternalHandler(), "GET", "/api/v1/platform/audit-export", "", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("without an exporter = %d", w.Code)
	}
	var since, until time.Time
	a.AuditExport = func(_ context.Context, s, u time.Time, w io.Writer) (int, error) {
		since, until = s, u
		io.WriteString(w, "{\"action\":\"x\"}\n")
		return 1, nil
	}
	for _, q := range []string{"?since=yesterday", "?until=2026-13-01", "?since=2026-02-01&until=2026-01-01", "?since=2026-01-01&until=2026-01-01"} {
		if w := do(a.ExternalHandler(), "GET", "/api/v1/platform/audit-export"+q, "", nil); w.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d", q, w.Code)
		}
	}
	w := do(a.ExternalHandler(), "GET", "/api/v1/platform/audit-export?since=2026-01-01&until=2026-02-01", "", nil)
	if w.Code != http.StatusOK || w.Body.String() != "{\"action\":\"x\"}\n" {
		t.Fatalf("export = %d %q", w.Code, w.Body.String())
	}
	if since.Format(time.DateOnly) != "2026-01-01" || until.Format(time.DateOnly) != "2026-02-01" {
		t.Fatalf("bounds = %v %v", since, until)
	}
	if got := w.Header().Get("Content-Disposition"); got != `attachment; filename="felis-audit-from-2026-01-01-before-2026-02-01.jsonl"` {
		t.Fatalf("disposition = %q", got)
	}
	if w := do(a.ExternalHandler(), "GET", "/api/v1/platform/audit-export", "", nil); w.Code != http.StatusOK || !since.IsZero() || !until.IsZero() {
		t.Fatalf("the whole log = %d %v %v", w.Code, since, until)
	}
}

func TestMaintenanceGuardHoldsOnlyForBlockingTasks(t *testing.T) {
	guard := NodeMaintenanceGuard(&fakeNodeControl{tasks: []nodecontrol.Task{
		{ID: "a", State: "running", Request: nodecontrol.Request{Action: "doctor"}},
		{ID: "b", State: "running", Request: nodecontrol.Request{Action: "db-backup"}},
		{ID: "c", State: "failed", Request: nodecontrol.Request{Action: "db-restore"}},
	}})
	if err := guard(context.Background()); err != nil {
		t.Fatalf("non-blocking tasks held starts: %v", err)
	}
	guard = NodeMaintenanceGuard(&fakeNodeControl{tasks: []nodecontrol.Task{{ID: "d", State: "running", Request: nodecontrol.Request{Action: "db-restore"}}}})
	if err := guard(context.Background()); err == nil {
		t.Fatal("a running database restore let starts through")
	}
}

func TestHostTasksAuditUnderTheirScope(t *testing.T) {
	a, repo, control := ownerAPI()
	if w := do(a.ExternalHandler(), "POST", nodeTaskPath, `{"action":"doctor","confirmMaintenance":false}`, jsonHeader); w.Code != http.StatusAccepted {
		t.Fatalf("doctor = %d %s", w.Code, w.Body.String())
	}
	if w := do(a.ExternalHandler(), "POST", nodeTaskPath, nodeTaskBody, jsonHeader); w.Code != http.StatusAccepted {
		t.Fatalf("approve = %d %s", w.Code, w.Body.String())
	}
	if !hasAudit(repo, "platform.host.doctor") || !hasAudit(repo, "platform.node.approve") {
		t.Fatalf("audits = %+v", repo.audits)
	}
	if w := do(a.ExternalHandler(), "POST", nodeTaskPath, `{"action":"db-restore","params":{"bundle":"felis-db-20261001T033000Z-daily.tar"},"confirmMaintenance":false}`, jsonHeader); w.Code != http.StatusBadRequest {
		t.Fatalf("an unconfirmed restore = %d", w.Code)
	}
	if control.calls != 2 {
		t.Fatalf("host calls = %d", control.calls)
	}
}
