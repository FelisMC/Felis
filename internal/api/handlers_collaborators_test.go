package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/fileedit"
)

func seedCollaboratorFixture(t *testing.T, phase string, protected bool) (*API, *fakeRepo, *fakeCluster, *switchExternal) {
	t.Helper()
	repo := newFakeRepo()
	repo.byName["survival"] = &ServerRecord{
		Name:      "survival",
		Subdomain: "survival",
		OwnerID:   "u-srv-owner",
		Protected: protected,
	}
	repo.owners["survival"] = ServerOwnership{
		OwnerID:   "u-srv-owner",
		Owner:     "srv-owner@example.net",
		Protected: protected,
	}
	repo.backups = []fakeBackup{
		{
			view: BackupView{
				ID:          "bk1",
				ServerName:  "survival",
				FormerOwner: "u-srv-owner",
				Status:      "present",
				SizeBytes:   1024,
				CreatedAt:   time.Unix(1_700_000_000, 0),
			},
			ref: "/archives/survival-bk1.tar.gz",
		},
	}

	users := []struct {
		id, name, email, role string
	}{
		{"u-owner", "owner", "owner@example.net", "owner"},
		{"u-admin", "admin", "admin@example.net", "admin"},
		{"u-srv-owner", "srvowner", "srv-owner@example.net", "user"},
		{"u-op", "opuser", "op@example.net", "user"},
		{"u-mgr", "mgruser", "mgr@example.net", "user"},
		{"u-stranger", "stranger", "stranger@example.net", "user"},
	}
	for _, u := range users {
		uv := UserView{ID: u.id, Username: u.name, Email: u.email, Role: u.role, EmailVerified: true}
		repo.seededUsers = append(repo.seededUsers, seededUser{view: uv, detail: UserDetail{UserView: uv}})
		repo.staff[u.name] = &StaffUser{ID: u.id, Username: u.name, Email: u.email, Role: u.role, EmailVerified: true}
	}

	desired := string(v1alpha1.DesiredStopped)
	ready := false
	if phase == "Running" {
		desired = string(v1alpha1.DesiredRunning)
		ready = true
	}
	cl := newFakeCluster()
	cl.byName["survival"] = &ServerInfo{
		Name:            "survival",
		Subdomain:       "survival",
		Phase:           phase,
		Ready:           ready,
		DesiredState:    desired,
		AutostartPolicy: "ownerOnly",
		Memory:          "4Gi",
		StorageSize:     "20Gi",
	}
	cl.list = []ServerInfo{*cl.byName["survival"]}

	ext := &switchExternal{
		p: &Principal{UserID: "u-owner", Username: "owner", Email: "owner@example.net", Role: "owner", ViaAdminAccess: true, ReauthAt: time.Unix(1_700_000_000, 0)},
	}
	a := newTestAPI(repo, cl)
	a.External = ext
	a.Files = &fakeFileEditor{paperGlobal: fileedit.PaperGlobalConfig{Values: map[string]any{}, SHA256: testSum}}
	a.FileStage = &fileedit.Stage{Dir: t.TempDir(), MinFree: 1e-9}
	a.InternalBaseURL = "http://felis-api-internal.felis.svc.cluster.local:8081"
	a.Console = &fakeConsole{reply: "ok"}
	a.Backuper = &fakeBackuper{}
	a.Restorer = &fakeRestorer{}
	a.Exporter = &fakeExporter{stopped: make(chan string, 16)}
	a.Schedules = newFakeSchedules()
	a.JobStatus = &fakeJobStatus{}
	a.Distribution = &fakeDistribution{}
	return a, repo, cl, ext
}

func TestServerProtectionRequiresOwnerAndReauth(t *testing.T) {
	a, repo, _, ext := seedCollaboratorFixture(t, "Stopped", false)
	h := a.ExternalHandler()

	// Owner with an enrolled factor and without recent reauth is blocked.
	repo.passkeyCreds["pk-owner"] = PasskeyCredential{ID: "pk-owner", UserID: "u-owner", CredentialID: "cred-owner"}
	ext.set(&Principal{UserID: "u-owner", Role: "owner", ViaAdminAccess: true, ViaSession: true, EmailVerified: true}, nil)
	w := do(h, "PUT", "/api/v1/servers/survival/protection", `{"protected":true}`, jsonHeader)
	if w.Code != http.StatusForbidden || decodeErr(t, w) != "reauth_required" {
		t.Fatalf("owner without reauth: %d %s", w.Code, w.Body.String())
	}

	// Admin even with reauth cannot toggle server protection.
	ext.set(&Principal{UserID: "u-admin", Role: "admin", ViaAdminAccess: true, ViaSession: true, EmailVerified: true, ReauthAt: time.Unix(1_700_000_000, 0)}, nil)
	w = do(h, "PUT", "/api/v1/servers/survival/protection", `{"protected":true}`, jsonHeader)
	if w.Code != http.StatusForbidden {
		t.Fatalf("admin protection toggle: %d %s", w.Code, w.Body.String())
	}

	// Owner with reauth succeeds and records audit.
	ext.set(&Principal{UserID: "u-owner", Email: "owner@example.net", Role: "owner", ViaAdminAccess: true, ViaSession: true, EmailVerified: true, ReauthAt: time.Unix(1_700_000_000, 0)}, nil)
	w = do(h, "PUT", "/api/v1/servers/survival/protection", `{"protected":true}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("owner enable protection: %d %s", w.Code, w.Body.String())
	}
	if !repo.byName["survival"].Protected {
		t.Fatal("server protection was not persisted")
	}
}

func TestProtectedServerIsolatesUnauthorizedAdminAndServerOwner(t *testing.T) {
	a, _, _, ext := seedCollaboratorFixture(t, "Stopped", true)
	h := a.ExternalHandler()

	// Unauthorized Admin receives readonly view on status and fleet.
	ext.set(&Principal{UserID: "u-admin", Role: "admin", ViaAdminAccess: true}, nil)
	w := do(h, "GET", "/api/v1/servers/survival/status", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("admin status on protected server: %d %s", w.Code, w.Body.String())
	}
	var st ServerInfo
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || !st.Protected || st.AccessRole != AccessRoleReadonly {
		t.Fatalf("admin protected status = %+v (err=%v), want protected+readonly", st, err)
	}

	w = do(h, "GET", "/api/v1/fleet", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("fleet: %d %s", w.Code, w.Body.String())
	}
	var fl struct {
		Servers []fleetServerView `json:"servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &fl); err != nil || len(fl.Servers) != 1 ||
		!fl.Servers[0].Protected || fl.Servers[0].AccessRole != AccessRoleReadonly || fl.Servers[0].Claimable {
		t.Fatalf("fleet view = %+v (err=%v)", fl.Servers, err)
	}

	// Unauthorized Admin cannot manage backups listing or mutating routes on protected server.
	w = do(h, "GET", "/api/v1/backups?server=survival", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list backups: %d", w.Code)
	}
	var bks struct {
		Backups []BackupView `json:"backups"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &bks); err != nil || len(bks.Backups) != 0 {
		t.Fatalf("protected server backup leaked to unauthorized admin: %+v", bks.Backups)
	}

	for _, tc := range []struct {
		method, path, body string
		hdr                map[string]string
	}{
		{"POST", "/api/v1/servers/survival/wake", "", nil},
		{"POST", "/api/v1/servers/survival/stop", "", nil},
		{"POST", "/api/v1/servers/survival/restart", "", nil},
		{"POST", "/api/v1/servers/survival/kill", "", nil},
		{"POST", "/api/v1/servers/survival/command", `{"command":"list"}`, jsonHeader},
		{"GET", "/api/v1/servers/survival/console", "", nil},
		{"GET", "/api/v1/servers/survival/console/history", "", nil},
		{"GET", "/api/v1/servers/survival/usage", "", nil},
		{"GET", "/api/v1/servers/survival/jobs", "", nil},
		{"GET", "/api/v1/servers/survival/files", "", nil},
		{"PATCH", "/api/v1/servers/survival", `{"displayName":"X"}`, jsonHeader},
		{"GET", "/api/v1/servers/survival/startup-settings", "", nil},
		{"PUT", "/api/v1/servers/survival/tags", `{"tags":["vip"]}`, jsonHeader},
		{"POST", "/api/v1/servers/survival/backup", "", nil},
		{"POST", "/api/v1/servers/survival/restore-backup", `{"backup_id":"bk1"}`, jsonHeader},
		{"DELETE", "/api/v1/backups/bk1", "", nil},
		{"PUT", "/api/v1/servers/survival/retirement", `{"confirm":"survival","delete":true}`, jsonHeader},
		{"POST", "/api/v1/servers/survival/migrations", `{"targetNode":"b"}`, jsonHeader},
		{"GET", "/api/v1/servers/survival/collaborators", "", nil},
		{"PUT", "/api/v1/servers/survival/collaborators/u-admin", `{"role":"manager"}`, jsonHeader},
		{"DELETE", "/api/v1/servers/survival/collaborators/u-op", "", nil},
	} {
		w := do(h, tc.method, tc.path, tc.body, tc.hdr)
		if w.Code != http.StatusForbidden {
			t.Errorf("unauthorized admin %s %s: status=%d body=%s, want 403", tc.method, tc.path, w.Code, w.Body.String())
		}
	}

	// Non-collaborator server owner also loses access once server is marked protected.
	ext.set(&Principal{UserID: "u-srv-owner", Role: "user"}, nil)
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/v1/servers/survival/wake", ""},
		{"GET", "/api/v1/servers/survival/files", ""},
		{"PATCH", "/api/v1/me/servers/survival", `{"displayName":"Hijack"}`},
		{"PUT", "/api/v1/servers/survival/retirement", `{"confirm":"survival"}`},
	} {
		w := do(h, tc.method, tc.path, tc.body, jsonHeader)
		if w.Code != http.StatusForbidden {
			t.Errorf("non-collaborator owner on protected server %s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}

func TestCollaboratorManagementAndRoleBoundaries(t *testing.T) {
	a, repo, _, ext := seedCollaboratorFixture(t, "Stopped", false)
	h := a.ExternalHandler()

	// On an unprotected server, Admin manages collaborators without escalating onto platform owner.
	ext.set(&Principal{UserID: "u-admin", Role: "admin", ViaAdminAccess: true}, nil)
	w := do(h, "GET", "/api/v1/servers/survival/collaborators", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("admin list collaborators on unprotected: %d %s", w.Code, w.Body.String())
	}
	w = do(h, "PUT", "/api/v1/servers/survival/collaborators/u-owner", `{"role":"manager"}`, jsonHeader)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("assigning platform owner as collaborator: %d %s", w.Code, w.Body.String())
	}
	w = do(h, "PUT", "/api/v1/servers/survival/collaborators/u-op", `{"role":"operator"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("add operator collaborator: %d %s", w.Code, w.Body.String())
	}
	w = do(h, "PUT", "/api/v1/servers/survival/collaborators/u-mgr", `{"role":"manager"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("add manager collaborator: %d %s", w.Code, w.Body.String())
	}

	// Owner enables protected mode; unauthorized Admin is locked out while collaborators retain their server roles.
	ext.set(&Principal{UserID: "u-owner", Role: "owner", ViaAdminAccess: true, ReauthAt: time.Unix(1_700_000_000, 0)}, nil)
	if w := do(h, "PUT", "/api/v1/servers/survival/protection", `{"protected":true}`, jsonHeader); w.Code != http.StatusOK {
		t.Fatalf("protect: %d %s", w.Code, w.Body.String())
	}

	// Verify operator permissions (allowed operate endpoints, refused manage/destructive endpoints).
	ext.set(&Principal{UserID: "u-op", Email: "op@example.net", Role: "user"}, nil)
	for _, tc := range []struct {
		method, path, body string
		hdr                map[string]string
		want               int
	}{
		{"GET", "/api/v1/servers/survival/status", "", nil, http.StatusOK},
		{"POST", "/api/v1/servers/survival/wake", "", nil, http.StatusAccepted},
		{"POST", "/api/v1/servers/survival/stop", "", nil, http.StatusAccepted},
		{"POST", "/api/v1/servers/survival/restart", "", nil, http.StatusAccepted},
		{"POST", "/api/v1/servers/survival/kill", "", nil, http.StatusAccepted},
		{"GET", "/api/v1/servers/survival/console/history", "", nil, http.StatusOK},
		{"GET", "/api/v1/servers/survival/usage", "", nil, http.StatusOK},
		{"GET", "/api/v1/servers/survival/jobs", "", nil, http.StatusOK},
		{"GET", "/api/v1/servers/survival/allowlist", "", nil, http.StatusOK},
		{"GET", "/api/v1/backups?server=survival", "", nil, http.StatusOK},
		{"POST", "/api/v1/servers/survival/backup", "", nil, http.StatusAccepted},
		{"GET", "/api/v1/servers/survival/files", "", nil, http.StatusForbidden},
		{"GET", "/api/v1/servers/survival/schedules", "", nil, http.StatusForbidden},
		{"PUT", "/api/v1/servers/survival/tags", `{"tags":["x"]}`, jsonHeader, http.StatusForbidden},
		{"GET", "/api/v1/servers/survival/startup-settings", "", nil, http.StatusForbidden},
		{"PATCH", "/api/v1/me/servers/survival", `{"displayName":"Op"}`, jsonHeader, http.StatusForbidden},
		{"POST", "/api/v1/servers/survival/restore-backup", `{"backup_id":"bk1"}`, jsonHeader, http.StatusForbidden},
		{"POST", "/api/v1/servers/survival/world/export", "", nil, http.StatusForbidden},
		{"POST", "/api/v1/servers/survival/backups/bk1/export", "", nil, http.StatusForbidden},
		{"DELETE", "/api/v1/backups/bk1", "", nil, http.StatusNotFound},
		{"PUT", "/api/v1/servers/survival/retirement", `{"confirm":"survival"}`, jsonHeader, http.StatusForbidden},
	} {
		w := do(h, tc.method, tc.path, tc.body, tc.hdr)
		if w.Code != tc.want {
			t.Errorf("operator %s %s: status=%d body=%s, want %d", tc.method, tc.path, w.Code, w.Body.String(), tc.want)
		}
	}

	// Verify manager collaborator gets manage routes while staying excluded from retirement and admin routes.
	ext.set(&Principal{UserID: "u-mgr", Email: "mgr@example.net", Role: "user"}, nil)
	for _, tc := range []struct {
		method, path, body string
		hdr                map[string]string
		want               int
	}{
		{"GET", "/api/v1/servers/survival/files", "", nil, http.StatusOK},
		{"GET", "/api/v1/servers/survival/schedules", "", nil, http.StatusOK},
		{"PUT", "/api/v1/servers/survival/tags", `{"tags":["vip"]}`, jsonHeader, http.StatusOK},
		{"GET", "/api/v1/servers/survival/startup-settings", "", nil, http.StatusOK},
		{"PATCH", "/api/v1/me/servers/survival", `{"displayName":"Managed"}`, jsonHeader, http.StatusOK},
		{"POST", "/api/v1/servers/survival/restore-backup", `{"backup_id":"bk1"}`, jsonHeader, http.StatusAccepted},
		{"POST", "/api/v1/servers/survival/world/export", "", nil, http.StatusAccepted},
		{"DELETE", "/api/v1/backups/bk1", "", nil, http.StatusOK},
		{"PUT", "/api/v1/servers/survival/retirement", `{"confirm":"survival"}`, jsonHeader, http.StatusForbidden},
		{"PUT", "/api/v1/servers/survival/retirement", `{"confirm":"survival","delete":true}`, jsonHeader, http.StatusForbidden},
		{"PUT", "/api/v1/servers/survival/protection", `{"protected":false}`, jsonHeader, http.StatusForbidden},
		{"GET", "/api/v1/servers/survival/collaborators", "", nil, http.StatusForbidden},
	} {
		w := do(h, tc.method, tc.path, tc.body, tc.hdr)
		if w.Code != tc.want {
			t.Errorf("manager %s %s: status=%d body=%s, want %d", tc.method, tc.path, w.Code, w.Body.String(), tc.want)
		}
	}

	// Previous owner backup cannot be deleted by manager collaborator once ownership belongs to u-srv-owner.
	repo.backups = append(repo.backups, fakeBackup{
		view: BackupView{ID: "bk-prev", ServerName: "survival", FormerOwner: "u-prev", Status: "present", SizeBytes: 512},
		ref:  "/archives/prev.tar.gz",
	})
	w = do(h, "DELETE", "/api/v1/backups/bk-prev", "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("manager deleting previous owner archive: %d %s, want 404", w.Code, w.Body.String())
	}

	// In team mode, removing collaborator access revokes panel access immediately.
	a.DefaultMode = ModeTeam
	if w := do(h, "GET", "/api/v1/me", "", nil); w.Code != http.StatusOK {
		t.Fatalf("collaborator /me in team mode: %d %s", w.Code, w.Body.String())
	}
	ext.set(&Principal{UserID: "u-owner", Role: "owner", ViaAdminAccess: true, ReauthAt: time.Unix(1_700_000_000, 0)}, nil)
	if w := do(h, "DELETE", "/api/v1/servers/survival/collaborators/u-mgr", "", nil); w.Code != http.StatusNoContent {
		t.Fatalf("remove collaborator: %d %s", w.Code, w.Body.String())
	}
	ext.set(&Principal{UserID: "u-mgr", Email: "mgr@example.net", Role: "user"}, nil)
	w = do(h, "GET", "/api/v1/me", "", nil)
	if w.Code != http.StatusForbidden || decodeErr(t, w) != "team_mode_staff_only" {
		t.Fatalf("removed collaborator /me in team mode: %d %s", w.Code, w.Body.String())
	}
}

func TestStreamRevokedWhenProtectedOrCollaboratorRemoved(t *testing.T) {
	for _, tc := range []struct {
		name      string
		protected bool
		setup     func(*fakeRepo) *Principal
		revoke    func(*fakeRepo)
	}{
		{
			name:      "admin disconnected when server becomes protected",
			protected: false,
			setup: func(_ *fakeRepo) *Principal {
				return &Principal{UserID: "u-admin", Role: "admin", ViaAdminAccess: true}
			},
			revoke: func(repo *fakeRepo) {
				_ = repo.SetServerProtected(context.Background(), "survival", true)
			},
		},
		{
			name:      "collaborator disconnected when removed",
			protected: true,
			setup: func(repo *fakeRepo) *Principal {
				_, _ = repo.UpsertServerCollaborator(context.Background(), "survival", "u-op", CollabRoleOperator, "u-owner")
				return &Principal{UserID: "u-op", Role: "user"}
			},
			revoke: func(repo *fakeRepo) {
				_ = repo.DeleteServerCollaborator(context.Background(), "survival", "u-op")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shrinkStreamTimers(t, 20*time.Millisecond, time.Hour)
			a, repo, _, ext := seedCollaboratorFixture(t, "Running", tc.protected)
			ext.set(tc.setup(repo), nil)
			started := make(chan struct{})
			a.Logs = &fakeLogStreamer{srcFromCtx: func(ctx context.Context) io.ReadCloser {
				return &ctxBlockingReadCloser{ctx: ctx, first: []byte("boot\n"), firstRead: started, closed: make(chan struct{})}
			}}

			done := make(chan *httptest.ResponseRecorder)
			go func() { done <- do(a.ExternalHandler(), "GET", "/api/v1/servers/survival/console", "", nil) }()
			<-started
			tc.revoke(repo)

			select {
			case w := <-done:
				if !strings.HasSuffix(w.Body.String(), sseRevoked) {
					t.Fatalf("stream ended with %q, want %q", w.Body.String(), sseRevoked)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("stream did not close after access revocation")
			}
		})
	}
}
