package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/worldexport"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type fakeExporter struct {
	reqs []worldexport.Request
	err  error
}

func (f *fakeExporter) Start(_ context.Context, r worldexport.Request) (string, error) {
	f.reqs = append(f.reqs, r)
	if f.err != nil {
		return "", f.err
	}
	return worldexport.JobName(r.Server, r.ID), nil
}

// asUser authenticates each request as the principal its X-Test-User header
// names, so one handler serves several users.
type asUser map[string]*Principal

func (u asUser) Authenticate(r *http.Request) (*Principal, error) {
	if p, ok := u[r.Header.Get("X-Test-User")]; ok {
		return p, nil
	}
	return nil, errUnauthorized
}

var exportUsers = asUser{
	"owner1":   {UserID: "owner1", Email: "owner1@example.net", Role: "user"},
	"owner3":   {UserID: "owner3", Email: "owner3@example.net", Role: "user"},
	"newowner": {UserID: "newowner", Email: "newowner@example.net", Role: "user"},
	"stranger": {UserID: "stranger", Email: "stranger@example.net", Role: "user"},
	"admin1":   {UserID: "admin1", Email: "admin1@example.net", Role: "admin", ViaAdminAccess: true},
	"nouser":   {Email: "legacy@example.net", Role: "admin", ViaAdminAccess: true},
}

func as(user string) map[string]string { return map[string]string{"X-Test-User": user} }

const (
	exportBase  = "http://felis-api-internal.felis.svc:8081"
	worldPath   = "/api/v1/servers/survival/world/export"
	backupPath  = "/api/v1/servers/survival/backups/bk1/export"
	exportStamp = "20231114-221320" // newTestAPI's clock, UTC
)

var (
	hex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex16 = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// exportFixture: owner1 owns survival (stopped) and creative, owner3 owns gamma
// (stopped); bk1 is survival's backup, bk2 creative's, both formerly owner1's.
func exportFixture() (*API, *fakeRepo, *fakeCluster, *fakeExporter) {
	repo := newFakeRepo()
	for name, owner := range map[string]string{"survival": "owner1", "creative": "owner1", "gamma": "owner3"} {
		repo.byName[name] = &ServerRecord{Name: name, OwnerID: owner}
	}
	repo.backups = []fakeBackup{
		{view: BackupView{ID: "bk1", ServerName: "survival", FormerOwner: "owner1", Status: "present", SizeBytes: 1024},
			ref: "/backups/survival-bk1.tar.gz"},
		{view: BackupView{ID: "bk2", ServerName: "creative", FormerOwner: "owner1", Status: "present", SizeBytes: 2048},
			ref: "/backups/creative-bk2.tar.gz"},
	}
	cl := newFakeCluster()
	for _, name := range []string{"survival", "gamma"} {
		cl.byName[name] = &ServerInfo{Name: name, Phase: "Stopped", DesiredState: string(v1alpha1.DesiredStopped)}
	}
	ex := &fakeExporter{}
	a := newTestAPI(repo, cl)
	a.External = exportUsers
	a.Exporter, a.InternalBaseURL = ex, exportBase
	return a, repo, cl, ex
}

func beginExport(t *testing.T, h http.Handler, path, user string) exportTicketView {
	t.Helper()
	w := do(h, "POST", path, "", as(user))
	if w.Code != http.StatusAccepted {
		t.Fatalf("POST %s as %s = %d, want 202 (%s)", path, user, w.Code, w.Body.String())
	}
	var v exportTicketView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("ticket not JSON: %v (%s)", err, w.Body.String())
	}
	return v
}

func exportState(t *testing.T, h http.Handler, ticket, user string) (int, exportStatusView) {
	t.Helper()
	w := do(h, "GET", "/api/v1/exports/"+ticket, "", as(user))
	var v exportStatusView
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("status not JSON: %v (%s)", err, w.Body.String())
		}
	} else {
		v.State = decodeErr(t, w)
	}
	return w.Code, v
}

func waitExportReady(t *testing.T, h http.Handler, ticket, user string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		if code, v := exportState(t, h, ticket, user); code == http.StatusOK && v.State == exportReady {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the export never read ready")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// uploadExport serves the Job's PUT on the internal face in the background.
func uploadExport(h http.Handler, id, token string, body io.Reader) <-chan *httptest.ResponseRecorder {
	out := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest("PUT", "/api/v1/internal/exports/"+id, body)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		out <- w
	}()
	return out
}

func awaitUpload(t *testing.T, up <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case w := <-up:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("the upload never answered")
		return nil
	}
}

// doSoon is do for a request that must be refused outright. A download that
// claimed the export by mistake would block on the parked body, and an upload
// let in by mistake would wait for a browser, so either fails the test after a
// bound instead of hanging it.
func doSoon(t *testing.T, h http.Handler, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	out := make(chan *httptest.ResponseRecorder, 1)
	go func() { out <- do(h, method, path, body, headers) }()
	select {
	case w := <-out:
		return w
	case <-time.After(2 * time.Second):
		t.Fatalf("%s %s never answered: it claimed the export", method, path)
		return nil
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func TestExportBackupGate(t *testing.T) {
	t.Run("former owner starts a backup export", func(t *testing.T) {
		a, repo, cl, ex := exportFixture()
		v := beginExport(t, a.ExternalHandler(), backupPath, "owner1")
		if !hex64.MatchString(v.Ticket) || v.State != "pending" || v.Filename != "survival-backup-bk1.tar.gz" {
			t.Fatalf("ticket = %+v", v)
		}
		if len(ex.reqs) != 1 {
			t.Fatalf("exporter started %d Jobs, want 1", len(ex.reqs))
		}
		r := ex.reqs[0]
		if r.Server != "survival" || r.Mode != worldexport.ModeBackup || r.BackupRef != "/backups/survival-bk1.tar.gz" ||
			!hex16.MatchString(r.ID) || !hex64.MatchString(r.Token) || r.Token == v.Ticket ||
			r.TargetURL != exportBase+"/api/v1/internal/exports/"+r.ID {
			t.Fatalf("export request = %+v", r)
		}
		if len(cl.acquired) != 0 {
			t.Fatalf("a backup export took the world lock: %v", cl.acquired)
		}
		if len(repo.audits) != 1 || repo.audits[0].Action != "backup.export" || repo.audits[0].ServerName != "survival" ||
			repo.audits[0].ActorUserID != "owner1" || string(repo.audits[0].Payload) != `{"backup_id":"bk1","size_bytes":1024}` {
			t.Fatalf("audit = %+v", repo.audits)
		}
	})

	for _, tc := range []struct {
		name, user, path string
		edit             func(*API, *fakeRepo)
		code             int
		errCode          string
	}{
		{name: "admin exports another's world", user: "admin1", path: backupPath,
			edit: func(_ *API, r *fakeRepo) { r.backups[0].view.FormerOwner = "someone" }, code: http.StatusAccepted},
		{name: "stranger", user: "stranger", path: backupPath, code: http.StatusForbidden, errCode: "forbidden"},
		{name: "principal without an account", user: "nouser", path: backupPath, code: http.StatusForbidden, errCode: "forbidden"},
		{name: "current owner who is not the former owner", user: "newowner", path: backupPath,
			edit: func(_ *API, r *fakeRepo) { r.byName["survival"].OwnerID = "newowner" },
			code: http.StatusNotFound, errCode: "no_backup"},
		{name: "unknown backup", user: "owner1", path: "/api/v1/servers/survival/backups/nope/export",
			code: http.StatusNotFound, errCode: "no_backup"},
		{name: "another server's backup", user: "owner1", path: "/api/v1/servers/survival/backups/bk2/export",
			code: http.StatusForbidden, errCode: "forbidden"},
		{name: "corrupt backup", user: "owner1", path: backupPath,
			edit: func(_ *API, r *fakeRepo) { r.backups[0].view.Corrupt = true }, code: http.StatusConflict, errCode: "backup_corrupt"},
		{name: "bad name", user: "owner1", path: "/api/v1/servers/Bad_Name/backups/bk1/export",
			code: http.StatusBadRequest, errCode: "bad_name"},
		{name: "unknown server", user: "owner1", path: "/api/v1/servers/nosuch/backups/bk1/export",
			code: http.StatusNotFound, errCode: "not_found"},
		{name: "no exporter", user: "owner1", path: backupPath,
			edit: func(a *API, _ *fakeRepo) { a.Exporter = nil }, code: http.StatusServiceUnavailable, errCode: "export_unavailable"},
		{name: "no internal URL", user: "owner1", path: backupPath,
			edit: func(a *API, _ *fakeRepo) { a.InternalBaseURL = "" }, code: http.StatusServiceUnavailable, errCode: "export_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, _, ex := exportFixture()
			if tc.edit != nil {
				tc.edit(a, repo)
			}
			w := do(a.ExternalHandler(), "POST", tc.path, "", as(tc.user))
			if w.Code != tc.code {
				t.Fatalf("code = %d, want %d (%s)", w.Code, tc.code, w.Body.String())
			}
			if tc.code == http.StatusAccepted {
				if len(ex.reqs) != 1 {
					t.Fatalf("exporter started %d Jobs, want 1", len(ex.reqs))
				}
				return
			}
			if got := decodeErr(t, w); got != tc.errCode {
				t.Errorf("error code = %q, want %q", got, tc.errCode)
			}
			if len(ex.reqs) != 0 || len(repo.audits) != 0 {
				t.Errorf("a refused export started %d Jobs and wrote %d audits", len(ex.reqs), len(repo.audits))
			}
		})
	}
}

func TestExportWorldGate(t *testing.T) {
	t.Run("owner exports a stopped world under the world lock", func(t *testing.T) {
		a, repo, cl, ex := exportFixture()
		v := beginExport(t, a.ExternalHandler(), worldPath, "owner1")
		if v.Filename != "survival-world-"+exportStamp+".tar.gz" || v.State != "pending" {
			t.Fatalf("ticket = %+v", v)
		}
		if len(ex.reqs) != 1 || ex.reqs[0].Mode != worldexport.ModeWorld || ex.reqs[0].BackupRef != "" || ex.reqs[0].Server != "survival" {
			t.Fatalf("export requests = %+v", ex.reqs)
		}
		if strings.Join(cl.acquired, ",") != "survival:"+maintenance.KindExport || strings.Join(cl.released, ",") != "survival" {
			t.Fatalf("lock acquired %v, released %v", cl.acquired, cl.released)
		}
		if len(repo.audits) != 1 || repo.audits[0].Action != "world.export" || repo.audits[0].ServerName != "survival" {
			t.Fatalf("audit = %+v", repo.audits)
		}
	})

	for _, tc := range []struct {
		name, user string
		edit       func(*API, *fakeCluster)
		code       int
		errCode    string
	}{
		{name: "admin", user: "admin1", code: http.StatusAccepted},
		{name: "stranger", user: "stranger", code: http.StatusForbidden, errCode: "forbidden"},
		{name: "running", user: "owner1", edit: func(_ *API, c *fakeCluster) { c.byName["survival"].Ready = true },
			code: http.StatusConflict, errCode: "not_stopped"},
		{name: "coming up", user: "owner1",
			edit: func(_ *API, c *fakeCluster) { c.byName["survival"].DesiredState = string(v1alpha1.DesiredRunning) },
			code: http.StatusConflict, errCode: "not_stopped"},
		{name: "no world volume", user: "owner1", edit: func(_ *API, c *fakeCluster) { c.noWorld["survival"] = true },
			code: http.StatusConflict, errCode: "no_world_volume"},
		{name: "world busy", user: "owner1",
			edit: func(_ *API, c *fakeCluster) {
				c.maintErr["survival"] = &MaintenanceBusyError{Kind: maintenance.KindRestore}
			},
			code: http.StatusConflict, errCode: "maintenance_in_progress"},
		{name: "no exporter", user: "owner1", edit: func(a *API, _ *fakeCluster) { a.Exporter = nil },
			code: http.StatusServiceUnavailable, errCode: "export_unavailable"},
		{name: "no internal URL", user: "owner1", edit: func(a *API, _ *fakeCluster) { a.InternalBaseURL = "" },
			code: http.StatusServiceUnavailable, errCode: "export_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, cl, ex := exportFixture()
			if tc.edit != nil {
				tc.edit(a, cl)
			}
			w := do(a.ExternalHandler(), "POST", worldPath, "", as(tc.user))
			if w.Code != tc.code {
				t.Fatalf("code = %d, want %d (%s)", w.Code, tc.code, w.Body.String())
			}
			if tc.code == http.StatusAccepted {
				if len(ex.reqs) != 1 {
					t.Fatalf("exporter started %d Jobs, want 1", len(ex.reqs))
				}
				return
			}
			if got := decodeErr(t, w); got != tc.errCode {
				t.Errorf("error code = %q, want %q", got, tc.errCode)
			}
			if len(ex.reqs) != 0 || len(repo.audits) != 0 || len(cl.released) != 0 {
				t.Errorf("a refused export started %d Jobs, wrote %d audits, released %v", len(ex.reqs), len(repo.audits), cl.released)
			}
		})
	}

	// What anyone refused by a running world export hears.
	t.Run("a world export holding the lock is named", func(t *testing.T) {
		a, _, cl, _ := exportFixture()
		cl.maintErr["survival"] = &MaintenanceBusyError{Kind: maintenance.KindExport}
		w := do(a.ExternalHandler(), "POST", worldPath, "", as("admin1"))
		var raw map[string]map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &raw)
		if want := "a world export is running on this server's world; retry once it finishes"; w.Code != http.StatusConflict ||
			raw["error"]["code"] != "maintenance_in_progress" || raw["error"]["message"] != want {
			t.Fatalf("busy = %d %s, want 409 %q", w.Code, w.Body.String(), want)
		}
	})

	// A refusal after admission gives the export back: the owner is not left
	// blocked by an export that never got a Job.
	t.Run("lock conflict and a failed Job are refunded", func(t *testing.T) {
		a, _, cl, ex := exportFixture()
		h := a.ExternalHandler()
		cl.maintErr["survival"] = &MaintenanceBusyError{Kind: maintenance.KindBackup}
		if w := do(h, "POST", worldPath, "", as("owner1")); w.Code != http.StatusConflict {
			t.Fatalf("busy world: code = %d, want 409", w.Code)
		}
		delete(cl.maintErr, "survival")
		ex.err = errors.New("jobs is forbidden")
		if w := do(h, "POST", worldPath, "", as("owner1")); w.Code != http.StatusInternalServerError {
			t.Fatalf("failed Job: code = %d, want 500 (%s)", w.Code, w.Body.String())
		}
		if strings.Join(cl.released, ",") != "survival" {
			t.Fatalf("a failed Job left the lock held: released %v", cl.released)
		}
		ex.err = nil
		beginExport(t, h, worldPath, "owner1")
		if n := len(a.exportTickets().starts["owner1"]); n != 1 {
			t.Fatalf("hourly starts = %d, want only the export that got a Job", n)
		}
	})
}

func TestExportLimits(t *testing.T) {
	t.Run("one per user", func(t *testing.T) {
		a, _, _, ex := exportFixture()
		h := a.ExternalHandler()
		beginExport(t, h, worldPath, "owner1")
		w := do(h, "POST", backupPath, "", as("owner1"))
		if w.Code != http.StatusTooManyRequests || decodeErr(t, w) != "export_busy" || w.Header().Get("Retry-After") != "90" {
			t.Fatalf("second export: %d %s Retry-After %q", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
		}
		if len(ex.reqs) != 1 {
			t.Fatalf("exporter started %d Jobs, want 1", len(ex.reqs))
		}
	})

	t.Run("two across the install", func(t *testing.T) {
		a, _, cl, ex := exportFixture()
		h := a.ExternalHandler()
		beginExport(t, h, worldPath, "owner1")
		beginExport(t, h, "/api/v1/servers/creative/backups/bk2/export", "admin1")
		w := do(h, "POST", "/api/v1/servers/gamma/world/export", "", as("owner3"))
		if w.Code != http.StatusTooManyRequests || decodeErr(t, w) != "export_busy" || w.Header().Get("Retry-After") != "60" {
			t.Fatalf("third export: %d %s Retry-After %q", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
		}
		if len(ex.reqs) != 2 || strings.Join(cl.acquired, ",") != "survival:export" {
			t.Fatalf("a refused export started a Job or took gamma's lock: %d Jobs, acquired %v", len(ex.reqs), cl.acquired)
		}
	})

	t.Run("six per user per hour", func(t *testing.T) {
		defer func(old time.Duration) { exportPendingTTL = old }(exportPendingTTL)
		exportPendingTTL = time.Minute
		a, _, _, _ := exportFixture()
		var clock atomic.Int64
		clock.Store(1_700_000_000)
		a.Now = func() time.Time { return time.Unix(clock.Load(), 0) }
		h := a.ExternalHandler()
		for i := range exportPerHour {
			clock.Store(1_700_000_000 + int64(i)*120) // each start outlives the last one's pending TTL
			beginExport(t, h, worldPath, "owner1")
		}
		clock.Store(1_700_000_000 + 12*60)
		w := do(h, "POST", worldPath, "", as("owner1"))
		if w.Code != http.StatusTooManyRequests || decodeErr(t, w) != "export_busy" || w.Header().Get("Retry-After") != "2880" {
			t.Fatalf("seventh export: %d %s Retry-After %q", w.Code, w.Body.String(), w.Header().Get("Retry-After"))
		}
		clock.Store(1_700_000_000 + 3600)
		beginExport(t, h, worldPath, "owner1")
	})
}

// TestExportRendezvous walks one world export end to end through the handlers:
// the Job's PUT parks until the owner's download claims it, the archive streams
// through untouched, the ticket works once and for its user only.
func TestExportRendezvous(t *testing.T) {
	a, repo, _, ex := exportFixture()
	var clock atomic.Int64
	clock.Store(1_700_000_000)
	a.Now = func() time.Time { return time.Unix(clock.Load(), 0) }
	ext, in := a.ExternalHandler(), a.InternalHandler()
	v := beginExport(t, ext, worldPath, "owner1")
	job := ex.reqs[0]
	download := "/api/v1/exports/" + v.Ticket + "/download"

	if code, s := exportState(t, ext, v.Ticket, "owner1"); code != http.StatusOK || s != (exportStatusView{State: "pending"}) {
		t.Fatalf("fresh status = %d %+v", code, s)
	}
	for _, user := range []string{"stranger", "admin1"} {
		if code, s := exportState(t, ext, v.Ticket, user); code != http.StatusNotFound || s.State != "not_found" {
			t.Fatalf("%s reads the owner's ticket: %d %+v", user, code, s)
		}
	}
	if w := do(ext, "GET", download, "", as("owner1")); w.Code != http.StatusConflict || decodeErr(t, w) != "export_not_ready" {
		t.Fatalf("download while pending = %d %s", w.Code, w.Body.String())
	}
	for name, hdr := range map[string]map[string]string{
		"no token":    nil,
		"wrong token": {"Authorization": "Bearer " + strings.Repeat("0", 64)},
		"the ticket":  {"Authorization": "Bearer " + v.Ticket},
	} {
		if w := doSoon(t, in, "PUT", "/api/v1/internal/exports/"+job.ID, "x", hdr); w.Code != http.StatusNotFound || decodeErr(t, w) != "not_found" {
			t.Fatalf("PUT with %s = %d %s", name, w.Code, w.Body.String())
		}
	}
	if w := doSoon(t, in, "PUT", "/api/v1/internal/exports/ffffffffffffffff", "x", map[string]string{"Authorization": "Bearer " + job.Token}); w.Code != http.StatusNotFound {
		t.Fatalf("PUT to another id = %d", w.Code)
	}

	archive := randomBytes(3*exportCopyBuffer + 4321)
	pr, pw := io.Pipe()
	up := uploadExport(in, job.ID, job.Token, pr)
	waitExportReady(t, ext, v.Ticket, "owner1")
	if w := doSoon(t, in, "PUT", "/api/v1/internal/exports/"+job.ID, "x", map[string]string{"Authorization": "Bearer " + job.Token}); w.Code != http.StatusNotFound {
		t.Fatalf("a second PUT with the spent token = %d, want 404", w.Code)
	}

	// Neither a HEAD nor another user's GET claims it.
	if w := doSoon(t, ext, "HEAD", download, "", as("owner1")); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET" {
		t.Fatalf("HEAD = %d, Allow %q", w.Code, w.Header().Get("Allow"))
	}
	if w := doSoon(t, ext, "GET", download, "", as("stranger")); w.Code != http.StatusNotFound {
		t.Fatalf("stranger's download = %d", w.Code)
	}
	if code, s := exportState(t, ext, v.Ticket, "owner1"); code != http.StatusOK || s.State != exportReady {
		t.Fatalf("after HEAD and a stranger: %d %+v, want still ready", code, s)
	}

	got := make(chan *httptest.ResponseRecorder, 1)
	go func() { got <- do(ext, "GET", download, "", as("owner1")) }()
	_, _ = pw.Write(archive[:10_000]) // returns once the download has read it
	if code, s := exportState(t, ext, v.Ticket, "owner1"); code != http.StatusGone || s.State != "export_expired" {
		t.Fatalf("status mid-download = %d %+v, want 410 export_expired", code, s)
	}
	if w := doSoon(t, ext, "GET", download, "", as("owner1")); w.Code != http.StatusGone || decodeErr(t, w) != "export_expired" {
		t.Fatalf("a second download mid-stream = %d %s", w.Code, w.Body.String())
	}
	go func() {
		for rest := archive[10_000:]; len(rest) > 0; {
			n := min(len(rest), 10_000)
			_, _ = pw.Write(rest[:n])
			rest = rest[n:]
		}
		pw.Close()
	}()
	w := <-got
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), archive) {
		t.Fatalf("download = %d, %d bytes; want 200 and the %d archive bytes", w.Code, w.Body.Len(), len(archive))
	}
	h := w.Header()
	if h.Get("Content-Type") != "application/gzip" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" ||
		h.Get("Content-Disposition") != "attachment; filename=survival-world-"+exportStamp+".tar.gz" || h.Get("Content-Length") != "" {
		t.Fatalf("download headers = %v", h)
	}
	if w := awaitUpload(t, up); w.Code != http.StatusNoContent {
		t.Fatalf("upload answered %d %s, want 204", w.Code, w.Body.String())
	}

	if w := do(ext, "GET", download, "", as("owner1")); w.Code != http.StatusGone || decodeErr(t, w) != "export_expired" {
		t.Fatalf("second download = %d %s", w.Code, w.Body.String())
	}
	if code, s := exportState(t, ext, v.Ticket, "owner1"); code != http.StatusGone || s.State != "export_expired" {
		t.Fatalf("status after download = %d %+v", code, s)
	}
	clock.Add(int64(exportKeepSpent / time.Second))
	if code, _ := exportState(t, ext, v.Ticket, "owner1"); code != http.StatusNotFound {
		t.Fatalf("status long after = %d, want 404", code)
	}
	if len(repo.audits) != 1 {
		t.Fatalf("audits = %+v, want the start only", repo.audits)
	}
	beginExport(t, ext, worldPath, "owner1") // the finished export no longer counts
}

func TestExportExpiry(t *testing.T) {
	t.Run("a Job that never connects", func(t *testing.T) {
		a, _, _, ex := exportFixture()
		var clock atomic.Int64
		clock.Store(1_700_000_000)
		a.Now = func() time.Time { return time.Unix(clock.Load(), 0) }
		ext := a.ExternalHandler()
		v := beginExport(t, ext, worldPath, "owner1")
		clock.Add(int64(exportPendingTTL/time.Second) - 1)
		if code, s := exportState(t, ext, v.Ticket, "owner1"); code != http.StatusOK || s.State != "pending" {
			t.Fatalf("just inside the pending TTL: %d %+v", code, s)
		}
		clock.Add(1)
		if code, s := exportState(t, ext, v.Ticket, "owner1"); code != http.StatusGone || s.State != "export_expired" {
			t.Fatalf("past the pending TTL: %d %+v", code, s)
		}
		job := ex.reqs[0]
		if w := doSoon(t, a.InternalHandler(), "PUT", "/api/v1/internal/exports/"+job.ID, "x", map[string]string{"Authorization": "Bearer " + job.Token}); w.Code != http.StatusNotFound {
			t.Fatalf("a late Job's PUT = %d, want 404", w.Code)
		}
	})

	t.Run("a browser that never comes", func(t *testing.T) {
		defer func(old time.Duration) { exportClaimTTL = old }(exportClaimTTL)
		exportClaimTTL = 30 * time.Millisecond
		a, _, _, ex := exportFixture()
		ext := a.ExternalHandler()
		v := beginExport(t, ext, backupPath, "owner1")
		job := ex.reqs[0]
		w := awaitUpload(t, uploadExport(a.InternalHandler(), job.ID, job.Token, strings.NewReader("archive")))
		if w.Code != http.StatusGone || decodeErr(t, w) != "export_expired" {
			t.Fatalf("unclaimed upload = %d %s, want 410 export_expired", w.Code, w.Body.String())
		}
		if w := do(ext, "GET", "/api/v1/exports/"+v.Ticket+"/download", "", as("owner1")); w.Code != http.StatusGone {
			t.Fatalf("download after the claim TTL = %d, want 410", w.Code)
		}
	})
}

// TestExportStatusReportsFailedJob: a Job that dies before it connects turns
// the export failed with the Job's own error, and frees the owner's slot.
func TestExportStatusReportsFailedJob(t *testing.T) {
	a, _, _, ex := exportFixture()
	js := &fakeJobStatus{}
	a.JobStatus = js
	ext := a.ExternalHandler()
	v := beginExport(t, ext, worldPath, "owner1")
	job := ex.reqs[0]
	name := worldexport.JobName("survival", job.ID)

	js.jobs = []AsyncJob{
		{Name: "export-survival-0000000000000000", Kind: "export_world", State: "failed", Message: "someone else's"},
		{Name: name, Kind: "export_world", State: "running"},
	}
	if code, s := exportState(t, ext, v.Ticket, "owner1"); code != http.StatusOK || s != (exportStatusView{State: "pending"}) {
		t.Fatalf("running Job: %d %+v", code, s)
	}
	js.jobs[1] = AsyncJob{Name: name, Kind: "export_world", State: "failed", Message: "felis export: open /world: permission denied"}
	want := exportStatusView{State: "failed", Message: "felis export: open /world: permission denied"}
	if code, s := exportState(t, ext, v.Ticket, "owner1"); code != http.StatusOK || s != want || js.got != "survival" {
		t.Fatalf("failed Job: %d %+v (asked about %q)", code, s, js.got)
	}
	js.jobs = nil
	if code, s := exportState(t, ext, v.Ticket, "owner1"); code != http.StatusOK || s != want {
		t.Fatalf("failed export once the Job is gone: %d %+v", code, s)
	}
	if w := doSoon(t, a.InternalHandler(), "PUT", "/api/v1/internal/exports/"+job.ID, "x", map[string]string{"Authorization": "Bearer " + job.Token}); w.Code != http.StatusNotFound {
		t.Fatalf("PUT on a failed export = %d, want 404", w.Code)
	}
	beginExport(t, ext, backupPath, "owner1")
}

// TestExportRegistryRaces: two transitions the handlers reach only in a race.
// A Job reported failed just as its upload arrived must not fail the ready
// export, and the claim timer firing as the browser claims must not spend the
// export under its download.
func TestExportRegistryRaces(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	reg := (&API{}).exportTickets()
	e := &exportEntry{userID: "owner1", server: "survival", mode: worldexport.ModeWorld, filename: "survival.tar.gz"}
	token, err := reg.admit(e, now)
	if err != nil {
		t.Fatal(err)
	}
	up := &exportUpload{body: strings.NewReader("x"), size: 1, claimed: make(chan struct{}), done: make(chan error, 1)}
	if !reg.arrive(e.id, token, up, now) {
		t.Fatal("the upload did not arrive")
	}

	if reg.fail(e.ticket, "felis export: killed", now) {
		t.Fatal("a late Job failure failed a ready export")
	}
	if got, err := reg.lookup(e.ticket, "owner1", now); err != nil || got.state != exportReady || got.message != "" {
		t.Fatalf("after a late Job failure: state %q, message %q, err %v; want ready", got.state, got.message, err)
	}

	if _, err := reg.claim(e.ticket, "owner1", now); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if reg.giveUp(up, now) {
		t.Fatal("the claim timer spent an export being downloaded")
	}
	if e.state != exportStreaming {
		t.Fatalf("state after a late giveUp = %q, want streaming", e.state)
	}
}

// exportServers runs both faces for real, so deadlines apply and an aborted
// download reaches the client as an error.
func exportServers(t *testing.T, a *API) (ext, in *httptest.Server) {
	ext, in = httptest.NewServer(a.ExternalHandler()), httptest.NewServer(a.InternalHandler())
	t.Cleanup(func() { ext.Close(); in.Close() })
	return ext, in
}

func realUpload(t *testing.T, in *httptest.Server, job worldexport.Request, body io.Reader, size int64) <-chan *http.Response {
	req, err := http.NewRequest("PUT", in.URL+"/api/v1/internal/exports/"+job.ID, body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+job.Token)
	out := make(chan *http.Response, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			out <- &http.Response{StatusCode: -1, Status: err.Error(), Body: io.NopCloser(strings.NewReader(""))}
			return
		}
		out <- resp
	}()
	return out
}

// realDownload fetches the owner's download: the response (nil when the request
// failed outright), the bytes read and the error that ended the read.
func realDownload(ext *httptest.Server, ticket string) (*http.Response, []byte, error) {
	req, _ := http.NewRequest("GET", ext.URL+"/api/v1/exports/"+ticket+"/download", nil)
	req.Header.Set("X-Test-User", "owner1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	return resp, got, err
}

func awaitResponse(t *testing.T, ch <-chan *http.Response) *http.Response {
	t.Helper()
	select {
	case resp := <-ch:
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	case <-time.After(5 * time.Second):
		t.Fatal("the upload never answered")
		return nil
	}
}

// TestExportBackupDigest: a backup streams with its length and is checked
// against the sha256 recorded when it was written. A match downloads whole; a
// mismatch withholds the tail and aborts, so the browser never holds a
// complete-looking corrupt file, and the Job hears backup_corrupt.
func TestExportBackupDigest(t *testing.T) {
	archive := randomBytes(3*exportCopyBuffer + 4321)
	sum := sha256.Sum256(archive)
	for _, tc := range []struct {
		name   string
		digest string
	}{
		{"recorded digest matches", hex.EncodeToString(sum[:])},
		{"no digest recorded", ""},
		{"digest mismatch", strings.Repeat("ab", 32)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, _, ex := exportFixture()
			repo.backups[0].sha256 = tc.digest
			ext, in := exportServers(t, a)
			v := beginExport(t, a.ExternalHandler(), backupPath, "owner1")
			up := realUpload(t, in, ex.reqs[0], bytes.NewReader(archive), int64(len(archive)))
			waitExportReady(t, a.ExternalHandler(), v.Ticket, "owner1")
			resp, got, err := realDownload(ext, v.Ticket)
			if resp == nil {
				t.Fatalf("download: %v", err)
			}
			if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Length") != strconv.Itoa(len(archive)) {
				t.Fatalf("download = %d, Content-Length %q", resp.StatusCode, resp.Header.Get("Content-Length"))
			}
			upResp := awaitResponse(t, up)
			if tc.digest == strings.Repeat("ab", 32) {
				if err == nil || len(got) >= len(archive) || !bytes.Equal(got, archive[:len(got)]) {
					t.Fatalf("mismatch: read %d of %d bytes, err %v; want an error short of the end", len(got), len(archive), err)
				}
				if upResp.StatusCode != http.StatusConflict || errCode(mustRead(t, upResp.Body)) != "backup_corrupt" {
					t.Fatalf("upload answered %d, want 409 backup_corrupt", upResp.StatusCode)
				}
				return
			}
			if err != nil || !bytes.Equal(got, archive) {
				t.Fatalf("read %d of %d bytes, err %v", len(got), len(archive), err)
			}
			if upResp.StatusCode != http.StatusNoContent {
				t.Fatalf("upload answered %d, want 204", upResp.StatusCode)
			}
		})
	}

	// The tail is withheld from the response writer itself, not only from
	// whatever the connection had yet to send: with every write captured, a
	// mismatch still ends short of the archive.
	t.Run("the tail waits for the digest", func(t *testing.T) {
		a, repo, _, ex := exportFixture()
		repo.backups[0].sha256 = strings.Repeat("ab", 32)
		ext := a.ExternalHandler()
		v := beginExport(t, ext, backupPath, "owner1")
		up := uploadExport(a.InternalHandler(), ex.reqs[0].ID, ex.reqs[0].Token, bytes.NewReader(archive))
		waitExportReady(t, ext, v.Ticket, "owner1")
		w := httptest.NewRecorder()
		func() {
			defer func() {
				if p := recover(); p != http.ErrAbortHandler {
					t.Errorf("the download ended with %v, want the abort", p)
				}
			}()
			r := httptest.NewRequest("GET", "/api/v1/exports/"+v.Ticket+"/download", nil)
			r.Header.Set("X-Test-User", "owner1")
			ext.ServeHTTP(w, r)
		}()
		if got := w.Body.Bytes(); len(got) != 3*exportCopyBuffer || !bytes.Equal(got, archive[:len(got)]) {
			t.Fatalf("wrote %d of %d bytes, want all but the last read (%d)", len(got), len(archive), 3*exportCopyBuffer)
		}
		if w := awaitUpload(t, up); w.Code != http.StatusConflict || decodeErr(t, w) != "backup_corrupt" {
			t.Fatalf("upload answered %d %s, want 409 backup_corrupt", w.Code, w.Body.String())
		}
	})
}

// zeros reads as an endless run of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func mustRead(t *testing.T, r io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestExportUploadPace: the Job's upload waits for the browser longer than the
// body-deadline grace, and then moves at the browser's pace, without being cut
// off; a body that stops moving for exportStall is.
func TestExportUploadPace(t *testing.T) {
	defer func(g time.Duration, r float64, s time.Duration) { bodyGrace, bodyMinRate, exportStall = g, r, s }(bodyGrace, bodyMinRate, exportStall)
	bodyGrace, bodyMinRate, exportStall = 100*time.Millisecond, 1<<30, 300*time.Millisecond

	t.Run("a slow claim is not a slow body", func(t *testing.T) {
		a, _, _, ex := exportFixture()
		ext, in := exportServers(t, a)
		v := beginExport(t, a.ExternalHandler(), worldPath, "owner1")
		archive := randomBytes(64 << 10)
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write(archive[:1000])
			time.Sleep(3 * bodyGrace)
			_, _ = pw.Write(archive[1000:])
			pw.Close()
		}()
		up := realUpload(t, in, ex.reqs[0], pr, -1)
		waitExportReady(t, a.ExternalHandler(), v.Ticket, "owner1")
		time.Sleep(3 * bodyGrace)
		_, got, err := realDownload(ext, v.Ticket)
		if err != nil || !bytes.Equal(got, archive) {
			t.Fatalf("read %d of %d bytes, err %v", len(got), len(archive), err)
		}
		if resp := awaitResponse(t, up); resp.StatusCode != http.StatusNoContent {
			t.Fatalf("upload answered %d, want 204", resp.StatusCode)
		}
	})

	t.Run("a stalled body is cut off", func(t *testing.T) {
		a, _, _, ex := exportFixture()
		ext, in := exportServers(t, a)
		v := beginExport(t, a.ExternalHandler(), worldPath, "owner1")
		pr, pw := io.Pipe()
		defer pw.Close()
		go func() { _, _ = pw.Write(make([]byte, 1000)) }() // then nothing, ever
		up := realUpload(t, in, ex.reqs[0], pr, -1)
		waitExportReady(t, a.ExternalHandler(), v.Ticket, "owner1")
		done := make(chan error, 1)
		go func() { _, _, err := realDownload(ext, v.Ticket); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a stalled archive downloaded as complete")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a stalled upload was never cut off")
		}
		pw.CloseWithError(errors.New("test over"))
		awaitResponse(t, up)
	})

	t.Run("a browser that stops reading is cut off", func(t *testing.T) {
		a, _, _, ex := exportFixture()
		ext, in := exportServers(t, a)
		v := beginExport(t, a.ExternalHandler(), worldPath, "owner1")
		realUpload(t, in, ex.reqs[0], io.LimitReader(zeros{}, 1<<30), -1)
		waitExportReady(t, a.ExternalHandler(), v.Ticket, "owner1")
		req, _ := http.NewRequest("GET", ext.URL+"/api/v1/exports/"+v.Ticket+"/download", nil)
		req.Header.Set("X-Test-User", "owner1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() }) // before the servers close, so a held handler ends
		// Read nothing: the socket buffers fill and the next write waits.
		reg := a.exportTickets()
		for deadline := time.Now().Add(5 * time.Second); ; {
			reg.mu.Lock()
			state := reg.byTicket[v.Ticket].state
			reg.mu.Unlock()
			if state == exportSpent {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("a browser that stopped reading held the download open: state %q", state)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := io.ReadAll(resp.Body); err == nil {
			t.Fatal("the cut-off download read as complete")
		}
	})
}

// TestK8sExportJobs: export Jobs show in the jobs list by what they archive,
// and never count as world work the backup scheduler waits for.
func TestK8sExportJobs(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	job := func(id, mode string) *batchv1.Job {
		j, err := worldexport.ExportJob(worldexport.JobParams{
			Server: "survival", ID: id, Mode: mode,
			WorldPVC: "world-survival-0", BackupPVC: "felis-backups", BackupRef: "/backups/a.tar.gz",
			TargetURL: exportBase + "/x", Token: "t", Namespace: "minecraft", Image: "felis:1",
		})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	world, backupJob := job("1111111111111111", worldexport.ModeWorld), job("2222222222222222", worldexport.ModeBackup)
	restoring := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "minecraft", Name: "restore-survival-cc",
		Labels: map[string]string{jobServerLabel: "survival", jobManagedByLabel: jobManagedByRestore}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(world, backupJob, restoring).
		WithStatusSubresource(&batchv1.Job{}).Build()
	k := NewK8sJobStatus(c, "minecraft")
	ctx := context.Background()

	if n, err := k.RunningWorldJobs(ctx); err != nil || n != 1 {
		t.Fatalf("RunningWorldJobs = %d, %v; want the restore only", n, err)
	}
	jobs, err := k.LatestJobs(ctx, "survival")
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, j := range jobs {
		kinds[j.Name] = j.Kind + "/" + j.State
	}
	want := map[string]string{world.Name: "export_world/running", backupJob.Name: "export_backup/running", restoring.Name: "restore/running"}
	if len(kinds) != len(want) {
		t.Fatalf("jobs = %v, want %v", kinds, want)
	}
	for name, k := range want {
		if kinds[name] != k {
			t.Errorf("%s = %q, want %q", name, kinds[name], k)
		}
	}

	// The kinds agree with maintenance.JobKind: a world export holds the world,
	// a backup export does not.
	if kind, holds := maintenance.JobKind(world); kind != maintenance.KindExport || !holds {
		t.Errorf("JobKind(world export) = %q, %v", kind, holds)
	}
	if _, holds := maintenance.JobKind(backupJob); holds {
		t.Error("a backup export holds the world")
	}
}
