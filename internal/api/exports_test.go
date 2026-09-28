package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/fileedit"
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
	// stopped receives each Job Stop is asked to delete; the sweep asks from
	// a goroutine of its own.
	stopped chan string
}

func (f *fakeExporter) Stop(_ context.Context, job string) error {
	f.stopped <- job
	return nil
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
	ex := &fakeExporter{stopped: make(chan string, 64)}
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
// digest, when set, is the trailer the Job sends once its body has ended.
func uploadExport(h http.Handler, id, token string, body io.Reader, digest string) <-chan *httptest.ResponseRecorder {
	out := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest("PUT", "/api/v1/internal/exports/"+id, body)
		r.Header.Set("Authorization", "Bearer "+token)
		if digest != "" {
			r.Trailer = http.Header{worldexport.DigestTrailer: {digest}}
		}
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
// awaitStop returns the next Job the sweep asked to delete.
func awaitStop(t *testing.T, ex *fakeExporter) string {
	t.Helper()
	select {
	case job := <-ex.stopped:
		return job
	case <-time.After(5 * time.Second):
		t.Fatal("no Job was stopped")
		return ""
	}
}

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
		repo.backups[0].sha256 = strings.Repeat("cd", 32)
		v := beginExport(t, a.ExternalHandler(), backupPath, "owner1")
		if !hex64.MatchString(v.Ticket) || v.State != "pending" || v.Filename != "survival-backup-bk1.tar.gz" {
			t.Fatalf("ticket = %+v", v)
		}
		if len(ex.reqs) != 1 {
			t.Fatalf("exporter started %d Jobs, want 1", len(ex.reqs))
		}
		r := ex.reqs[0]
		if r.Server != "survival" || r.Mode != worldexport.ModeBackup || r.BackupRef != "/backups/survival-bk1.tar.gz" ||
			r.BackupSHA256 != strings.Repeat("cd", 32) || r.Path != "" || r.Dir ||
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
		if len(ex.reqs) != 1 || ex.reqs[0].Mode != worldexport.ModeWorld || ex.reqs[0].BackupRef != "" || ex.reqs[0].BackupSHA256 != "" ||
			ex.reqs[0].Path != "" || ex.reqs[0].Server != "survival" {
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
		if want := "a world export or file download is running on this server's world; retry once it finishes"; w.Code != http.StatusConflict ||
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
		if n := len(a.exportTickets().starts[exportStarts{userID: "owner1"}]); n != 1 {
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

	// A length that is no byte count is refused before the token is spent.
	for _, bad := range []string{"-1", "12abc", "0x10"} {
		if w := doSoon(t, in, "PUT", "/api/v1/internal/exports/"+job.ID, "x", map[string]string{
			"Authorization": "Bearer " + job.Token, worldexport.LengthHeader: bad}); w.Code != http.StatusBadRequest || decodeErr(t, w) != "bad_request" {
			t.Fatalf("PUT declaring %q bytes = %d %s", bad, w.Code, w.Body.String())
		}
	}

	archive := randomBytes(3*exportCopyBuffer + 4321)
	pr, pw := io.Pipe()
	up := uploadExport(in, job.ID, job.Token, pr, contentDigestOf(string(archive)))
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
		if got := awaitStop(t, ex); got != worldexport.JobName(job.Server, job.ID) {
			t.Fatalf("stopped %q, want the export's own Job", got)
		}
		if w := doSoon(t, a.InternalHandler(), "PUT", "/api/v1/internal/exports/"+job.ID, "x", map[string]string{"Authorization": "Bearer " + job.Token}); w.Code != http.StatusNotFound {
			t.Fatalf("a late Job's PUT = %d, want 404", w.Code)
		}
	})

	// The owner closed the tab, so no route sweeps; felis-api's loop does.
	t.Run("the loop stops a Job left pending, and only that one", func(t *testing.T) {
		a, _, _, ex := exportFixture()
		var clock atomic.Int64
		clock.Store(1_700_000_000)
		a.Now = func() time.Time { return time.Unix(clock.Load(), 0) }
		ext := a.ExternalHandler()
		beginExport(t, ext, worldPath, "owner1")
		stuck := ex.reqs[0]
		v := beginExport(t, ext, "/api/v1/servers/creative/backups/bk2/export", "admin1")
		moving := ex.reqs[1]
		up := uploadExport(a.InternalHandler(), moving.ID, moving.Token, strings.NewReader("archive"), contentDigestOf("archive"))
		waitExportReady(t, ext, v.Ticket, "admin1")

		clock.Add(int64(exportPendingTTL/time.Second) - 1)
		a.ExpireExports()
		select {
		case job := <-ex.stopped:
			t.Fatalf("stopped %s inside the pending TTL", job)
		case <-time.After(50 * time.Millisecond):
		}
		clock.Add(1)
		a.ExpireExports()
		if got := awaitStop(t, ex); got != worldexport.JobName(stuck.Server, stuck.ID) {
			t.Fatalf("stopped %q, want the Job that never connected", got)
		}
		select {
		case job := <-ex.stopped:
			t.Fatalf("also stopped %s, whose upload was waiting for its browser", job)
		case <-time.After(50 * time.Millisecond):
		}
		// Its browser takes it, so the upload's handler has returned before the
		// next subtest changes the claim TTL that handler reads.
		if w := do(ext, "GET", "/api/v1/exports/"+v.Ticket+"/download", "", as("admin1")); w.Code != http.StatusOK || w.Body.String() != "archive" {
			t.Fatalf("download of the waiting export = %d %q", w.Code, w.Body.String())
		}
		if w := awaitUpload(t, up); w.Code != http.StatusNoContent {
			t.Fatalf("upload answered %d %s, want 204", w.Code, w.Body.String())
		}
	})

	t.Run("a browser that never comes", func(t *testing.T) {
		defer func(old time.Duration) { exportClaimTTL = old }(exportClaimTTL)
		exportClaimTTL = 30 * time.Millisecond
		a, _, _, ex := exportFixture()
		ext := a.ExternalHandler()
		v := beginExport(t, ext, backupPath, "owner1")
		job := ex.reqs[0]
		w := awaitUpload(t, uploadExport(a.InternalHandler(), job.ID, job.Token, strings.NewReader("archive"), contentDigestOf("archive")))
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

// realUpload sends the Job's PUT as cmd/felis export does: chunked, with the
// size when it is known (not -1) and, when set, digest as the trailer.
func realUpload(t *testing.T, in *httptest.Server, job worldexport.Request, body io.Reader, size int64, digest string) <-chan *http.Response {
	req, err := http.NewRequest("PUT", in.URL+"/api/v1/internal/exports/"+job.ID, body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = -1
	if size >= 0 {
		req.Header.Set(worldexport.LengthHeader, strconv.FormatInt(size, 10))
	}
	if digest != "" {
		req.Trailer = http.Header{worldexport.DigestTrailer: {digest}}
	}
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

// TestExportStreamsWhatTheJobSends: the archive reaches the browser byte for
// byte, with the length the Job declared when it declared one. The Job checks a
// backup against its recorded digest itself and, on a mismatch, cuts its upload
// short of the end; the download then aborts too, so the browser never holds a
// complete-looking file.
func TestExportStreamsWhatTheJobSends(t *testing.T) {
	archive := randomBytes(3*exportCopyBuffer + 4321)

	t.Run("whole, with its length", func(t *testing.T) {
		a, _, _, ex := exportFixture()
		ext, in := exportServers(t, a)
		v := beginExport(t, a.ExternalHandler(), backupPath, "owner1")
		up := realUpload(t, in, ex.reqs[0], bytes.NewReader(archive), int64(len(archive)), contentDigestOf(string(archive)))
		waitExportReady(t, a.ExternalHandler(), v.Ticket, "owner1")
		resp, got, err := realDownload(ext, v.Ticket)
		if resp == nil {
			t.Fatalf("download: %v", err)
		}
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Length") != strconv.Itoa(len(archive)) ||
			err != nil || !bytes.Equal(got, archive) {
			t.Fatalf("download = %d, Content-Length %q, read %d of %d bytes, err %v",
				resp.StatusCode, resp.Header.Get("Content-Length"), len(got), len(archive), err)
		}
		if upResp := awaitResponse(t, up); upResp.StatusCode != http.StatusNoContent {
			t.Fatalf("upload answered %d, want 204", upResp.StatusCode)
		}
	})

	t.Run("an upload the Job cuts short aborts the download", func(t *testing.T) {
		a, _, _, ex := exportFixture()
		ext, in := exportServers(t, a)
		v := beginExport(t, a.ExternalHandler(), backupPath, "owner1")
		// As the Job's digest check fails: all but the end, then a read error,
		// which aborts the chunked PUT.
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write(archive[:len(archive)-100])
			pw.CloseWithError(errors.New("the backup archive does not match the sha256 recorded when it was written"))
		}()
		up := realUpload(t, in, ex.reqs[0], pr, -1, contentDigestOf(string(archive)))
		waitExportReady(t, a.ExternalHandler(), v.Ticket, "owner1")
		resp, got, err := realDownload(ext, v.Ticket)
		if resp == nil {
			t.Fatalf("download: %v", err)
		}
		if err == nil || len(got) > len(archive)-100 || !bytes.Equal(got, archive[:len(got)]) {
			t.Fatalf("read %d of %d bytes, err %v; want an error short of the end", len(got), len(archive), err)
		}
		if upResp := awaitResponse(t, up); upResp.StatusCode != -1 {
			t.Fatalf("the aborted upload answered %d", upResp.StatusCode)
		}
	})

	// The Job's trailer is the SHA-256 of what it sent. Bytes that arrive
	// otherwise, or without it, or not as many as it declared, never reach the
	// browser to their end, and the Job hears the download failed.
	t.Run("bytes changed on the way never reach the browser whole", func(t *testing.T) {
		flipped := bytes.Clone(archive)
		flipped[len(flipped)/2] ^= 1
		for _, c := range []struct {
			name, digest string
			size         int64
		}{
			{"another archive's digest", contentDigestOf(string(flipped)), int64(len(archive))},
			{"no digest", "", -1},
			{"a digest that is no SHA-256", "sha-256=:AAAA:", -1},
			{"one byte more declared than sent", contentDigestOf(string(archive)), int64(len(archive)) + 1},
		} {
			t.Run(c.name, func(t *testing.T) {
				a, _, _, ex := exportFixture()
				ext, in := exportServers(t, a)
				v := beginExport(t, a.ExternalHandler(), backupPath, "owner1")
				up := realUpload(t, in, ex.reqs[0], bytes.NewReader(archive), c.size, c.digest)
				waitExportReady(t, a.ExternalHandler(), v.Ticket, "owner1")
				resp, got, err := realDownload(ext, v.Ticket)
				if resp == nil {
					t.Fatalf("download: %v", err)
				}
				if err == nil || len(got) >= len(archive) || !bytes.Equal(got, archive[:len(got)]) {
					t.Fatalf("read %d of %d bytes, err %v; want an error short of the end", len(got), len(archive), err)
				}
				if upResp := awaitResponse(t, up); upResp.StatusCode != http.StatusGone {
					t.Fatalf("the upload answered %d, want 410", upResp.StatusCode)
				}
			})
		}
	})

	t.Run("a browser that leaves early is what the Job hears", func(t *testing.T) {
		a, _, _, ex := exportFixture()
		ext, _ := exportServers(t, a)
		v := beginExport(t, a.ExternalHandler(), worldPath, "owner1")
		up := uploadExport(a.InternalHandler(), ex.reqs[0].ID, ex.reqs[0].Token, io.LimitReader(zeros{}, 1<<30), "")
		waitExportReady(t, a.ExternalHandler(), v.Ticket, "owner1")
		req, _ := http.NewRequest("GET", ext.URL+"/api/v1/exports/"+v.Ticket+"/download", nil)
		req.Header.Set("X-Test-User", "owner1")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(resp.Body, make([]byte, 1000)); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if w := awaitUpload(t, up); w.Code != http.StatusGone || decodeErr(t, w) != "export_expired" {
			t.Fatalf("upload answered %d %s, want 410 export_expired", w.Code, w.Body.String())
		}
	})
}

const fileDownloadPath = "/api/v1/servers/survival/files/download?path="

// fileDownloadFixture is exportFixture with the file manager wired, which the
// file routes' gate requires.
func fileDownloadFixture() (*API, *fakeRepo, *fakeCluster, *fakeExporter) {
	a, repo, cl, ex := exportFixture()
	a.Files = &fakeFileEditor{}
	return a, repo, cl, ex
}

func TestFileDownloadGate(t *testing.T) {
	for _, tc := range []struct {
		name, query, filename, contentType, payload string
		want                                        worldexport.Request
	}{
		{"a file", "plugins/Essentials/config.yml", "config.yml", fileedit.DownloadFileType, `{"dir":false}`,
			worldexport.Request{Server: "survival", Mode: worldexport.ModeFiles, Path: "plugins/Essentials/config.yml"}},
		{"a folder, as a zip named after it", "plugins/Essentials/&dir=true", "Essentials.zip", fileedit.DownloadZipType, `{"dir":true}`,
			worldexport.Request{Server: "survival", Mode: worldexport.ModeFiles, Path: "plugins/Essentials/", Dir: true}},
		{"dir other than true is a file", "a.yml&dir=false", "a.yml", fileedit.DownloadFileType, `{"dir":false}`,
			worldexport.Request{Server: "survival", Mode: worldexport.ModeFiles, Path: "a.yml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, cl, ex := fileDownloadFixture()
			v := beginExport(t, a.ExternalHandler(), fileDownloadPath+tc.query, "owner1")
			if !hex64.MatchString(v.Ticket) || v.State != "pending" || v.Filename != tc.filename {
				t.Fatalf("ticket = %+v", v)
			}
			if len(ex.reqs) != 1 {
				t.Fatalf("exporter started %d Jobs, want 1", len(ex.reqs))
			}
			r := ex.reqs[0]
			if !hex16.MatchString(r.ID) || !hex64.MatchString(r.Token) || r.TargetURL != exportBase+"/api/v1/internal/exports/"+r.ID {
				t.Fatalf("export request = %+v", r)
			}
			r.ID, r.Token, r.TargetURL = "", "", ""
			if r != tc.want {
				t.Fatalf("export request = %+v, want %+v", r, tc.want)
			}
			if e := a.exportTickets().byTicket[v.Ticket]; e.contentType != tc.contentType || !e.files {
				t.Fatalf("export served as %q, file download %v", e.contentType, e.files)
			}
			if strings.Join(cl.acquired, ",") != "survival:"+maintenance.KindExport || strings.Join(cl.released, ",") != "survival" {
				t.Fatalf("lock acquired %v, released %v", cl.acquired, cl.released)
			}
			if len(repo.audits) != 1 || repo.audits[0].Action != "file.download" || repo.audits[0].ActorUserID != "owner1" ||
				repo.audits[0].ServerName != "survival:"+tc.want.Path || string(repo.audits[0].Payload) != tc.payload {
				t.Fatalf("audit = %+v", repo.audits)
			}
		})
	}

	t.Run("admin", func(t *testing.T) {
		a, _, _, ex := fileDownloadFixture()
		beginExport(t, a.ExternalHandler(), fileDownloadPath+"server.properties", "admin1")
		if len(ex.reqs) != 1 {
			t.Fatalf("exporter started %d Jobs, want 1", len(ex.reqs))
		}
	})

	busy := func(_ *API, c *fakeCluster) {
		c.maintErr["survival"] = &MaintenanceBusyError{Kind: maintenance.KindFileWrite}
	}
	for _, tc := range []struct {
		name, user, query string
		edit              func(*API, *fakeCluster)
		code              int
		errCode           string
		// msg, when set, is what the refusal must say.
		msg string
	}{
		{name: "stranger", user: "stranger", query: "a.yml", code: http.StatusForbidden, errCode: "forbidden"},
		{name: "principal without an account", user: "nouser", query: "a.yml", code: http.StatusForbidden, errCode: "forbidden"},
		{name: "running", user: "owner1", query: "a.yml", edit: func(_ *API, c *fakeCluster) { c.byName["survival"].Ready = true },
			code: http.StatusConflict, errCode: "not_stopped", msg: "stop the server before working with its files"},
		{name: "no world volume", user: "owner1", query: "a.yml", edit: func(_ *API, c *fakeCluster) { c.noWorld["survival"] = true },
			code: http.StatusConflict, errCode: "no_world_volume"},
		{name: "no file editor", user: "owner1", query: "a.yml", edit: func(a *API, _ *fakeCluster) { a.Files = nil },
			code: http.StatusServiceUnavailable, errCode: "files_unavailable"},
		{name: "no path", user: "owner1", query: "", code: http.StatusBadRequest, errCode: "bad_request"},
		{name: "the root", user: "owner1", query: ".&dir=true", code: http.StatusBadRequest, errCode: "bad_path"},
		{name: "the root, slashed", user: "owner1", query: "/&dir=true", code: http.StatusBadRequest, errCode: "bad_path"},
		{name: "the root, dotted", user: "owner1", query: "./", code: http.StatusBadRequest, errCode: "bad_path"},
		{name: "the root, walked back", user: "owner1", query: "plugins/..", code: http.StatusBadRequest, errCode: "bad_path"},
		{name: "no exporter", user: "owner1", query: "a.yml", edit: func(a *API, _ *fakeCluster) { a.Exporter = nil },
			code: http.StatusServiceUnavailable, errCode: "export_unavailable"},
		{name: "no internal URL", user: "owner1", query: "a.yml", edit: func(a *API, _ *fakeCluster) { a.InternalBaseURL = "" },
			code: http.StatusServiceUnavailable, errCode: "export_unavailable"},
		{name: "world busy", user: "owner1", query: "a.yml", edit: busy, code: http.StatusConflict, errCode: "maintenance_in_progress"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, cl, ex := fileDownloadFixture()
			if tc.edit != nil {
				tc.edit(a, cl)
			}
			w := do(a.ExternalHandler(), "POST", fileDownloadPath+tc.query, "", as(tc.user))
			if w.Code != tc.code {
				t.Fatalf("code = %d, want %d (%s)", w.Code, tc.code, w.Body.String())
			}
			if got := decodeErr(t, w); got != tc.errCode {
				t.Errorf("error code = %q, want %q", got, tc.errCode)
			}
			if tc.msg != "" && !strings.Contains(w.Body.String(), tc.msg) {
				t.Errorf("body %s does not say %q", w.Body.String(), tc.msg)
			}
			// Nothing is left behind: no Job, no audit, no ticket, no start
			// counted against the hour.
			reg := a.exportTickets()
			if len(ex.reqs) != 0 || len(repo.audits) != 0 || len(cl.released) != 0 || len(reg.byTicket) != 0 || len(reg.starts) != 0 {
				t.Errorf("a refused download started %d Jobs, wrote %d audits, released %v, left %d tickets and %v",
					len(ex.reqs), len(repo.audits), cl.released, len(reg.byTicket), reg.starts)
			}
		})
	}

	t.Run("a failed Job is refunded and releases the lock", func(t *testing.T) {
		a, _, cl, ex := fileDownloadFixture()
		ex.err = errors.New("jobs is forbidden")
		if w := do(a.ExternalHandler(), "POST", fileDownloadPath+"a.yml", "", as("owner1")); w.Code != http.StatusInternalServerError {
			t.Fatalf("failed Job: code = %d, want 500 (%s)", w.Code, w.Body.String())
		}
		if reg := a.exportTickets(); len(reg.byTicket) != 0 || len(reg.starts) != 0 || strings.Join(cl.released, ",") != "survival" {
			t.Fatalf("after a failed Job: %d tickets, starts %v, released %v", len(reg.byTicket), reg.starts, cl.released)
		}
	})
}

func TestFileDownloadLimits(t *testing.T) {
	busy := func(t *testing.T, w *httptest.ResponseRecorder, message, retry string) {
		t.Helper()
		var raw map[string]map[string]string
		_ = json.Unmarshal(w.Body.Bytes(), &raw)
		if w.Code != http.StatusTooManyRequests || raw["error"]["code"] != "export_busy" || raw["error"]["message"] != message ||
			w.Header().Get("Retry-After") != retry {
			t.Fatalf("refusal = %d %s Retry-After %q; want 429 %q Retry-After %s", w.Code, w.Body.String(), w.Header().Get("Retry-After"), message, retry)
		}
	}

	t.Run("two per user, counted apart from exports", func(t *testing.T) {
		a, _, _, ex := fileDownloadFixture()
		h := a.ExternalHandler()
		beginExport(t, h, fileDownloadPath+"a.yml", "owner1")
		beginExport(t, h, fileDownloadPath+"b.yml", "owner1")
		beginExport(t, h, worldPath, "owner1") // file downloads do not hold an export off
		busy(t, do(h, "POST", fileDownloadPath+"c.yml", "", as("owner1")),
			"you already have 2 file downloads in progress; let one finish first", "90")
		if len(ex.reqs) != 3 {
			t.Fatalf("exporter started %d Jobs, want 3", len(ex.reqs))
		}
	})

	t.Run("four across the install", func(t *testing.T) {
		a, _, _, ex := fileDownloadFixture()
		h := a.ExternalHandler()
		for _, u := range []string{"owner1", "owner1", "owner3", "owner3"} {
			server := map[string]string{"owner1": "survival", "owner3": "gamma"}[u]
			beginExport(t, h, "/api/v1/servers/"+server+"/files/download?path=a.yml", u)
		}
		busy(t, do(h, "POST", fileDownloadPath+"a.yml", "", as("admin1")),
			"4 file downloads are already in progress; retry in a minute", "60")
		if len(ex.reqs) != 4 {
			t.Fatalf("exporter started %d Jobs, want 4", len(ex.reqs))
		}
	})

	t.Run("thirty per user per hour", func(t *testing.T) {
		defer func(old time.Duration) { exportPendingTTL = old }(exportPendingTTL)
		exportPendingTTL = time.Minute
		a, _, _, _ := fileDownloadFixture()
		var clock atomic.Int64
		a.Now = func() time.Time { return time.Unix(clock.Load(), 0) }
		h := a.ExternalHandler()
		for i := range fileExportPerHour {
			clock.Store(1_700_000_000 + int64(i)*100) // each start outlives the last one's pending TTL
			beginExport(t, h, fileDownloadPath+"a.yml", "owner1")
		}
		clock.Store(1_700_000_000 + 2950)
		busy(t, do(h, "POST", fileDownloadPath+"a.yml", "", as("owner1")),
			"you have started 30 file downloads in the last hour; retry later", "650")
		beginExport(t, h, worldPath, "owner1") // exports keep their own hour
		clock.Store(1_700_000_000 + 3600)
		beginExport(t, h, fileDownloadPath+"a.yml", "owner1")
	})
}

// TestFileDownloadServed: a file goes out as its raw bytes under its own name,
// with its length; a folder as a zip, streamed without one.
func TestFileDownloadServed(t *testing.T) {
	for _, tc := range []struct {
		name, query, contentType, disposition string
		size                                  int64
	}{
		{"a file", url.QueryEscape("plugins/配置 file.yml"), fileedit.DownloadFileType,
			"attachment; filename*=utf-8''%E9%85%8D%E7%BD%AE%20file.yml", 64 << 10},
		{"a folder", "plugins&dir=true", fileedit.DownloadZipType, "attachment; filename=plugins.zip", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _, ex := fileDownloadFixture()
			ext, in := exportServers(t, a)
			v := beginExport(t, a.ExternalHandler(), fileDownloadPath+tc.query, "owner1")
			// Past what the server would buffer and measure itself when the
			// handler sets no length.
			body := randomBytes(64 << 10)
			up := realUpload(t, in, ex.reqs[0], bytes.NewReader(body), tc.size, contentDigestOf(string(body)))
			waitExportReady(t, a.ExternalHandler(), v.Ticket, "owner1")
			resp, got, err := realDownload(ext, v.Ticket)
			if resp == nil || err != nil || !bytes.Equal(got, body) {
				t.Fatalf("download: read %d bytes, %v", len(got), err)
			}
			wantLength := ""
			if tc.size >= 0 {
				wantLength = strconv.FormatInt(tc.size, 10)
			}
			h := resp.Header
			if h.Get("Content-Type") != tc.contentType || h.Get("Content-Disposition") != tc.disposition ||
				h.Get("Content-Length") != wantLength || h.Get("X-Content-Type-Options") != "nosniff" {
				t.Fatalf("download headers = %v", h)
			}
			if upResp := awaitResponse(t, up); upResp.StatusCode != http.StatusNoContent {
				t.Fatalf("upload answered %d, want 204", upResp.StatusCode)
			}
		})
	}
}

// zeros reads as an endless run of zero bytes.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
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
		up := realUpload(t, in, ex.reqs[0], pr, -1, contentDigestOf(string(archive)))
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
		up := realUpload(t, in, ex.reqs[0], pr, -1, "")
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
		realUpload(t, in, ex.reqs[0], io.LimitReader(zeros{}, 1<<30), -1, "")
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
			WorldPVC: "world-survival-0", BackupPVC: "felis-backups", BackupRef: "/backups/a.tar.gz", Path: "plugins",
			TargetURL: exportBase + "/x", Token: "t", Namespace: "minecraft", Image: "felis:1",
		})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	world, backupJob := job("1111111111111111", worldexport.ModeWorld), job("2222222222222222", worldexport.ModeBackup)
	files := job("3333333333333333", worldexport.ModeFiles)
	restoring := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "minecraft", Name: "restore-survival-cc",
		Labels: map[string]string{jobServerLabel: "survival", jobManagedByLabel: jobManagedByRestore}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(world, backupJob, files, restoring).
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
	want := map[string]string{world.Name: "export_world/running", backupJob.Name: "export_backup/running",
		files.Name: "export_files/running", restoring.Name: "restore/running"}
	if len(kinds) != len(want) {
		t.Fatalf("jobs = %v, want %v", kinds, want)
	}
	for name, k := range want {
		if kinds[name] != k {
			t.Errorf("%s = %q, want %q", name, kinds[name], k)
		}
	}

	// The kinds agree with maintenance.JobKind: a world export and a file
	// download hold the world, a backup export does not.
	if kind, holds := maintenance.JobKind(world); kind != maintenance.KindExport || !holds {
		t.Errorf("JobKind(world export) = %q, %v", kind, holds)
	}
	if kind, holds := maintenance.JobKind(files); kind != maintenance.KindExport || !holds {
		t.Errorf("JobKind(file download) = %q, %v", kind, holds)
	}
	if _, holds := maintenance.JobKind(backupJob); holds {
		t.Error("a backup export holds the world")
	}
}
