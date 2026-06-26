package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"github.com/golang-jwt/jwt/v5"
)

const testRoot = "mc.example.net" // neutral; never a deployment domain

// ---- fakes ----

type fakeRepo struct {
	bySub     map[string]*ServerRecord
	byName    map[string]*ServerRecord
	linked    map[string]bool
	quota     map[string]bool
	allowlist map[string]map[string]bool // name -> user_id -> on list (web face)
	// allowUUID mirrors the UUID-keyed server_allowlist table itself, the gate the
	// internal/velocity wake uses (name -> mc_uuid -> on list). allowlist above is
	// the account_links-bridged web view of the same data.
	allowUUID map[string]map[string]bool
	mine      map[string][]MyServerView
	claimOK   map[string]bool // name -> claim succeeds; absent name -> ErrNotFound
	audits    []AuditEntry
	joins     []string
	// create-server seeding (spec §15)
	seeded  map[string]bool   // name -> servers row exists
	aliases map[string]string // subdomain -> bound server name
	seedErr error
	// account linking (spec §10)
	linkCodes map[string]fakeLinkCode // code -> pending binding
	links     map[string]string       // mc_uuid -> user_id (mirrors UNIQUE(mc_uuid))
	// world backups (spec §7, §22). A nil slice lists empty.
	backups []fakeBackup
	// local-password auth (spec §B). staff is keyed by username (the login key);
	// sessions by token_hash; settings by key. They mirror the PG contract so the
	// hermetic tests exercise the same fail-closed semantics the integration impl
	// honors.
	staff    map[string]*StaffUser   // username -> staff login row
	sessions map[string]*fakeSession // token_hash -> session
	settings map[string][]byte       // key -> jsonb value
}

// fakeSession mirrors a sessions row: its owner, its expiry, and whether it has
// been revoked.
type fakeSession struct {
	userID    string
	expiresAt time.Time
	revoked   bool
}

// fakeBackup mirrors a world_backups row: the client-facing view plus the
// server-side backup_ref the list queries never expose.
type fakeBackup struct {
	view BackupView
	ref  string
}

// fakeLinkCode mirrors an account_link_codes row.
type fakeLinkCode struct {
	mcUUID    string
	expiresAt time.Time
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{
		bySub: map[string]*ServerRecord{}, byName: map[string]*ServerRecord{},
		linked: map[string]bool{}, quota: map[string]bool{},
		allowlist: map[string]map[string]bool{}, allowUUID: map[string]map[string]bool{},
		mine:    map[string][]MyServerView{},
		claimOK: map[string]bool{},
		seeded:  map[string]bool{}, aliases: map[string]string{},
		linkCodes: map[string]fakeLinkCode{}, links: map[string]string{},
		staff:    map[string]*StaffUser{},
		sessions: map[string]*fakeSession{},
		settings: map[string][]byte{},
	}
}

func (f *fakeRepo) ServerBySubdomain(_ context.Context, s string) (*ServerRecord, error) {
	if r, ok := f.bySub[s]; ok {
		return r, nil
	}
	return nil, ErrNotFound
}
func (f *fakeRepo) ServerByName(_ context.Context, n string) (*ServerRecord, error) {
	if r, ok := f.byName[n]; ok {
		return r, nil
	}
	return nil, ErrNotFound
}
func (f *fakeRepo) IsLinked(_ context.Context, u string) (bool, error)       { return f.linked[u], nil }
func (f *fakeRepo) QuotaAvailable(_ context.Context, u string) (bool, error) { return f.quota[u], nil }
func (f *fakeRepo) CreateLinkCode(_ context.Context, code, mcUUID string, expiresAt time.Time) error {
	f.linkCodes[code] = fakeLinkCode{mcUUID: mcUUID, expiresAt: expiresAt}
	return nil
}

// VerifyLinkCode mirrors PGRepo.VerifyLinkCode exactly so the hermetic tests
// exercise the same contract the integration impl honors: strict expiry against
// the passed clock, a different-user UUID → ErrConflict WITHOUT consuming the
// code, same (user, uuid) idempotent, and the code consumed only on success.
func (f *fakeRepo) VerifyLinkCode(_ context.Context, userID, code string, now time.Time) (string, error) {
	rec, ok := f.linkCodes[code]
	if !ok || !rec.expiresAt.After(now) {
		return "", ErrLinkCodeInvalid
	}
	if existing, ok := f.links[rec.mcUUID]; ok && existing != userID {
		return "", ErrConflict // do not consume another user's pending code
	}
	f.links[rec.mcUUID] = userID
	f.linked[userID] = true
	delete(f.linkCodes, code)
	return rec.mcUUID, nil
}
func (f *fakeRepo) UserInAllowlist(_ context.Context, n, u string) (bool, error) {
	return f.allowlist[n][u], nil
}
func (f *fakeRepo) UUIDInAllowlist(_ context.Context, n, uuid string) (bool, error) {
	return f.allowUUID[n][uuid], nil
}
func (f *fakeRepo) UserByMCUUID(_ context.Context, uuid string) (string, error) {
	if u, ok := f.links[uuid]; ok {
		return u, nil
	}
	return "", ErrNotFound
}
func (f *fakeRepo) ClaimServer(_ context.Context, n, u string) (bool, error) {
	ok, present := f.claimOK[n]
	if !present {
		return false, ErrNotFound
	}
	return ok, nil
}
func (f *fakeRepo) RecordJoin(_ context.Context, n, uuid string) error {
	if _, ok := f.byName[n]; !ok {
		return ErrNotFound
	}
	f.joins = append(f.joins, n+":"+uuid)
	return nil
}
func (f *fakeRepo) MyServers(_ context.Context, u string) ([]MyServerView, error) {
	return f.mine[u], nil
}
func (f *fakeRepo) SeedServer(_ context.Context, name, subdomain string) error {
	if f.seedErr != nil {
		return f.seedErr
	}
	if bound, ok := f.aliases[subdomain]; ok && bound != name {
		return ErrConflict
	}
	f.seeded[name] = true
	f.aliases[subdomain] = name
	return nil
}
func (f *fakeRepo) Audit(_ context.Context, e AuditEntry) error {
	f.audits = append(f.audits, e)
	return nil
}

// AllBackups / BackupsForUser / LatestBackup mirror the PG queries' contract so
// the hermetic tests can't pass against a too-lenient fake: only status='present'
// rows are visible, the user scope is the former_owner column, and LatestBackup
// is the newest present row for a server (or ErrNotFound).
func (f *fakeRepo) AllBackups(_ context.Context) ([]BackupView, error) {
	var out []BackupView
	for _, b := range f.backups {
		if b.view.Status == "present" {
			out = append(out, b.view)
		}
	}
	return out, nil
}
func (f *fakeRepo) BackupsForUser(_ context.Context, userID string) ([]BackupView, error) {
	var out []BackupView
	for _, b := range f.backups {
		if b.view.Status == "present" && b.view.FormerOwner == userID {
			out = append(out, b.view)
		}
	}
	return out, nil
}
func (f *fakeRepo) LatestBackup(_ context.Context, serverName string) (*BackupRecord, error) {
	var latest *fakeBackup
	for i := range f.backups {
		b := &f.backups[i]
		if b.view.Status != "present" || b.view.ServerName != serverName {
			continue
		}
		if latest == nil || b.view.CreatedAt.After(latest.view.CreatedAt) {
			latest = b
		}
	}
	if latest == nil {
		return nil, ErrNotFound
	}
	return &BackupRecord{
		ID: latest.view.ID, ServerName: latest.view.ServerName,
		FormerOwner: latest.view.FormerOwner, BackupRef: latest.ref,
		SizeBytes: latest.view.SizeBytes,
	}, nil
}

// ---- local-password auth fakes (spec §B) ----
// Each method mirrors the PGRepo contract: a returned StaffUser is copied so a
// test cannot mutate the stored row by reference, SessionUser re-reads the
// CURRENT staff flags (so a password change clears must_change_password for live
// sessions just as the PG JOIN does), and the settings/sessions semantics match.

func (f *fakeRepo) UserByUsername(_ context.Context, username string) (*StaffUser, error) {
	if u, ok := f.staff[username]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, ErrNotFound
}
func (f *fakeRepo) UserByID(_ context.Context, id string) (*StaffUser, error) {
	for _, u := range f.staff {
		if u.ID == id {
			cp := *u
			return &cp, nil
		}
	}
	return nil, ErrNotFound
}
func (f *fakeRepo) UpsertOwner(_ context.Context, id, username, email, passwordHash string, mustChange bool) error {
	// Mirror PG ON CONFLICT (username): preserve the existing id so live sessions
	// survive a password reset.
	if existing, ok := f.staff[username]; ok {
		id = existing.ID
	}
	f.staff[username] = &StaffUser{
		ID: id, Username: username, Email: email, Role: "admin",
		PasswordHash: passwordHash, MustChangePassword: mustChange,
	}
	return nil
}
func (f *fakeRepo) SetPassword(_ context.Context, userID, passwordHash string) error {
	for _, u := range f.staff {
		if u.ID == userID {
			u.PasswordHash = passwordHash
			u.MustChangePassword = false
			return nil
		}
	}
	return ErrNotFound
}
func (f *fakeRepo) CreateSession(_ context.Context, tokenHash, userID string, expiresAt time.Time) error {
	f.sessions[tokenHash] = &fakeSession{userID: userID, expiresAt: expiresAt}
	return nil
}
func (f *fakeRepo) SessionUser(_ context.Context, tokenHash string, now time.Time) (*SessionedUser, error) {
	s, ok := f.sessions[tokenHash]
	if !ok || s.revoked || !s.expiresAt.After(now) {
		return nil, ErrNotFound
	}
	for _, u := range f.staff {
		if u.ID == s.userID {
			return &SessionedUser{
				ID: u.ID, Email: u.Email, Role: u.Role,
				MustChangePassword: u.MustChangePassword,
			}, nil
		}
	}
	return nil, ErrNotFound
}
func (f *fakeRepo) RevokeSession(_ context.Context, tokenHash string) error {
	if s, ok := f.sessions[tokenHash]; ok {
		s.revoked = true
	}
	return nil
}
func (f *fakeRepo) RevokeUserSessionsExcept(_ context.Context, userID, keepTokenHash string) error {
	for h, s := range f.sessions {
		if s.userID == userID && h != keepTokenHash {
			s.revoked = true
		}
	}
	return nil
}
func (f *fakeRepo) GetSetting(_ context.Context, key string) ([]byte, error) {
	if v, ok := f.settings[key]; ok {
		return v, nil
	}
	return nil, ErrNotFound
}
func (f *fakeRepo) SetSetting(_ context.Context, key string, value []byte) error {
	f.settings[key] = value
	return nil
}

// fakeRestorer records the restore it was asked to start and returns a canned
// error, mirroring the Restorer kick-off contract. The real restore Job is
// integration-only, so the handler is tested against this fake (spec §466).
type fakeRestorer struct {
	err     error
	calls   int
	gotName string
	gotRef  string
}

func (f *fakeRestorer) Restore(_ context.Context, name, ref string) error {
	f.calls++
	f.gotName, f.gotRef = name, ref
	return f.err
}

type fakeCluster struct {
	byName    map[string]*ServerInfo
	bySub     map[string]*ServerInfo
	list      []ServerInfo
	desired   map[string]v1alpha1.DesiredState
	created   map[string]CreateServerInput // name -> the validated input it was created from
	patched   map[string]ServerSpecPatch   // name -> the validated spec patch it received
	createErr error
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{byName: map[string]*ServerInfo{}, bySub: map[string]*ServerInfo{},
		desired: map[string]v1alpha1.DesiredState{}, created: map[string]CreateServerInput{},
		patched: map[string]ServerSpecPatch{}}
}
func (c *fakeCluster) GetServer(_ context.Context, n string) (*ServerInfo, error) {
	if s, ok := c.byName[n]; ok {
		return s, nil
	}
	return nil, ErrNotFound
}
func (c *fakeCluster) GetBySubdomain(_ context.Context, s string) (*ServerInfo, error) {
	if v, ok := c.bySub[s]; ok {
		return v, nil
	}
	return nil, ErrNotFound
}
func (c *fakeCluster) ListServers(_ context.Context) ([]ServerInfo, error) { return c.list, nil }
func (c *fakeCluster) SetDesiredState(_ context.Context, n string, s v1alpha1.DesiredState) error {
	c.desired[n] = s
	return nil
}
func (c *fakeCluster) CreateServer(_ context.Context, in CreateServerInput) error {
	if c.createErr != nil {
		return c.createErr
	}
	if _, ok := c.byName[in.Name]; ok {
		return ErrConflict
	}
	c.created[in.Name] = in
	info := &ServerInfo{Name: in.Name, Subdomain: in.Subdomain,
		AutostartPolicy: string(in.AutostartPolicy),
		DesiredState:    string(v1alpha1.DesiredStopped), Phase: string(v1alpha1.PhaseStopped)}
	c.byName[in.Name] = info
	c.bySub[in.Subdomain] = info
	return nil
}
func (c *fakeCluster) PatchServerSpec(_ context.Context, n string, p ServerSpecPatch) error {
	info, ok := c.byName[n]
	if !ok {
		return ErrNotFound
	}
	c.patched[n] = p
	// Apply only the fields the lifecycle view exposes, so a follow-up read sees
	// the mutation (mirrors the real merge patch touching only non-nil fields).
	if p.AutostartPolicy != nil {
		info.AutostartPolicy = string(*p.AutostartPolicy)
	}
	return nil
}

// fakeConsole records the command it was asked to run and returns a canned reply
// or error, mirroring the Console contract. The real K8sConsole's password
// resolution and RCON dial are integration-only, so the handler is tested
// against this fake (spec §8 写=RCON).
type fakeConsole struct {
	reply      string
	err        error
	calls      int
	gotName    string
	gotCommand string
}

func (f *fakeConsole) RunCommand(_ context.Context, name, command string) (string, error) {
	f.calls++
	f.gotName, f.gotCommand = name, command
	if f.err != nil {
		return "", f.err
	}
	return f.reply, nil
}

// staticExternal injects a fixed principal so handler logic is tested without
// real JWT crypto (which is exercised separately in TestAccessVerifier).
type staticExternal struct {
	p   *Principal
	err error
}

func (s staticExternal) Authenticate(*http.Request) (*Principal, error) { return s.p, s.err }

type okInternal struct{}

func (okInternal) Authenticate(*http.Request) error { return nil }

// ---- helpers ----

func newTestAPI(repo Repo, cl Cluster) *API {
	return &API{Repo: repo, Cluster: cl, Internal: okInternal{}, RootDomain: testRoot,
		Now: func() time.Time { return time.Unix(1_700_000_000, 0) }}
}

func do(h http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var raw map[string]map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("error body not JSON: %v (%s)", err, w.Body.String())
	}
	return raw["error"]["code"]
}

// ---- dual-face separation ----

func TestInternalFaceRequiresServiceToken(t *testing.T) {
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.Internal = BearerTokenAuth{Token: "s3cr3t"}
	h := api.InternalHandler()

	// no token -> 401
	if w := do(h, "GET", "/api/v1/servers", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: code = %d, want 401", w.Code)
	}
	// wrong token -> 401
	if w := do(h, "GET", "/api/v1/servers", "", map[string]string{"Authorization": "Bearer nope"}); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: code = %d, want 401", w.Code)
	}
	// right token -> 200
	if w := do(h, "GET", "/api/v1/servers", "", map[string]string{"Authorization": "Bearer s3cr3t"}); w.Code != http.StatusOK {
		t.Fatalf("right token: code = %d, want 200", w.Code)
	}
}

func TestHealthzIsUnauthenticated(t *testing.T) {
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.Internal = BearerTokenAuth{Token: "s3cr3t"}
	if w := do(api.InternalHandler(), "GET", "/healthz", "", nil); w.Code != http.StatusOK {
		t.Fatalf("healthz code = %d, want 200", w.Code)
	}
}

func TestExternalFaceRequiresPrincipal(t *testing.T) {
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.External = staticExternal{err: http.ErrNoCookie} // any auth error
	if w := do(api.ExternalHandler(), "GET", "/api/v1/me/servers", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", w.Code)
	}
}

// TestMeIdentity proves GET /api/v1/me reports the server-computed identity the
// panel uses to gate its Admin / SysAdmin navigation. The load-bearing assertion
// is the third subtest: is_admin tracks Principal.IsAdmin(), so the admin ROLE is
// not sufficient — the request must ALSO have arrived via the admin Access path
// (ViaAdminAccess). An admin who reached the panel through the ordinary app path
// therefore reads is_admin=false and the panel hides the admin surfaces (which the
// backend would 403 regardless). This keeps the client from re-deriving graded ZT.
func TestMeIdentity(t *testing.T) {
	get := func(p *Principal) map[string]any {
		api := newTestAPI(newFakeRepo(), newFakeCluster())
		api.External = staticExternal{p: p}
		w := do(api.ExternalHandler(), "GET", "/api/v1/me", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("body not JSON: %v (%s)", err, w.Body.String())
		}
		return got
	}

	t.Run("ordinary user: is_admin false", func(t *testing.T) {
		got := get(&Principal{UserID: "u1", Email: "u1@example.net", Role: "user"})
		if got["user_id"] != "u1" || got["role"] != "user" || got["is_admin"] != false {
			t.Fatalf("got %v, want user_id=u1 role=user is_admin=false", got)
		}
	})

	t.Run("admin via admin Access path: is_admin true", func(t *testing.T) {
		got := get(&Principal{UserID: "a1", Email: "a1@example.net", Role: "admin", ViaAdminAccess: true})
		if got["role"] != "admin" || got["is_admin"] != true {
			t.Fatalf("got %v, want role=admin is_admin=true", got)
		}
	})

	t.Run("admin via ordinary app path: is_admin false (graded ZT)", func(t *testing.T) {
		got := get(&Principal{UserID: "a1", Email: "a1@example.net", Role: "admin", ViaAdminAccess: false})
		if got["role"] != "admin" {
			t.Fatalf("role = %v, want admin", got["role"])
		}
		if got["is_admin"] != false {
			t.Fatalf("is_admin = %v, want false — the admin role alone must not grant admin tier "+
				"without the admin Access path", got["is_admin"])
		}
	})
}

// ---- by-host ----

func TestByHost(t *testing.T) {
	cl := newFakeCluster()
	cl.bySub["survival"] = &ServerInfo{Name: "survival", Subdomain: "survival", Phase: "Running", Ready: true}
	api := newTestAPI(newFakeRepo(), cl)
	h := api.InternalHandler()
	tok := map[string]string{"Authorization": "Bearer "} // okInternal ignores it

	t.Run("foreign domain rejected", func(t *testing.T) {
		w := do(h, "GET", "/api/v1/servers/by-host/survival.evil.example.org", "", tok)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
	})
	t.Run("multi-label rejected", func(t *testing.T) {
		w := do(h, "GET", "/api/v1/servers/by-host/a.b."+testRoot, "", tok)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
	})
	t.Run("unknown server 404", func(t *testing.T) {
		w := do(h, "GET", "/api/v1/servers/by-host/creative."+testRoot, "", tok)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
	})
	t.Run("found", func(t *testing.T) {
		w := do(h, "GET", "/api/v1/servers/by-host/survival."+testRoot, "", tok)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		var info ServerInfo
		if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil || info.Name != "survival" {
			t.Fatalf("unexpected body %s err %v", w.Body.String(), err)
		}
	})
}

// ---- fleet (SysAdmin cockpit read) ----

// TestFleetAdminRead proves the SysAdmin cockpit's fleet read is admin-tier AND
// fleet-wide. Two properties distinguish it from the app-tier /me/servers: a plain
// user is rejected by adminOnly before the handler runs, and an admin sees EVERY
// server the cluster reports (CRD truth via ListServers, §1) rather than a
// caller-scoped slice.
func TestFleetAdminRead(t *testing.T) {
	cl := newFakeCluster()
	cl.list = []ServerInfo{
		{Name: "survival", Phase: "Running", Ready: true},
		{Name: "creative", Phase: "Stopped"},
		{Name: "skyblock", Phase: "Running", Ready: true},
	}

	t.Run("plain user forbidden", func(t *testing.T) {
		api := newTestAPI(newFakeRepo(), cl)
		api.External = staticExternal{p: &Principal{UserID: "u", Role: "user"}}
		w := do(api.ExternalHandler(), "GET", "/api/v1/fleet", "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403 (a plain user must not read the fleet)", w.Code)
		}
	})

	t.Run("admin via ordinary app path forbidden (graded ZT)", func(t *testing.T) {
		// The admin ROLE alone is not enough: without the admin Access path adminOnly
		// rejects, so the cockpit read cannot be reached by an admin who arrived via
		// the ordinary app face — exactly as GET /api/v1/me reports is_admin=false there.
		api := newTestAPI(newFakeRepo(), cl)
		api.External = staticExternal{p: &Principal{UserID: "a1", Email: "a1@example.net",
			Role: "admin", ViaAdminAccess: false}}
		w := do(api.ExternalHandler(), "GET", "/api/v1/fleet", "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403 (admin role without the admin Access path)", w.Code)
		}
	})

	t.Run("admin reads the whole fleet", func(t *testing.T) {
		api := newTestAPI(newFakeRepo(), cl)
		api.External = staticExternal{p: &Principal{UserID: "a1", Email: "a1@example.net",
			Role: "admin", ViaAdminAccess: true}}
		w := do(api.ExternalHandler(), "GET", "/api/v1/fleet", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200 (%s)", w.Code, w.Body.String())
		}
		var got map[string][]ServerInfo
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		// Fleet-wide: all three servers, not a caller-scoped subset.
		if len(got["servers"]) != 3 {
			t.Fatalf("servers = %d, want 3 (the fleet read must not be caller-scoped)", len(got["servers"]))
		}
	})
}

// ---- join-event ----

func TestJoinEvent(t *testing.T) {
	repo := newFakeRepo()
	repo.byName["survival"] = &ServerRecord{Name: "survival"}
	api := newTestAPI(repo, newFakeCluster())
	h := api.InternalHandler()

	t.Run("missing uuid 400", func(t *testing.T) {
		w := do(h, "POST", "/api/v1/internal/servers/survival/join-event", `{}`, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400", w.Code)
		}
	})
	t.Run("records join", func(t *testing.T) {
		w := do(h, "POST", "/api/v1/internal/servers/survival/join-event",
			`{"mc_uuid":"11111111-1111-1111-1111-111111111111"}`, nil)
		if w.Code != http.StatusNoContent {
			t.Fatalf("code = %d, want 204 (%s)", w.Code, w.Body.String())
		}
		if len(repo.joins) != 1 {
			t.Fatalf("expected 1 recorded join, got %d", len(repo.joins))
		}
	})
	t.Run("unknown server 404", func(t *testing.T) {
		w := do(h, "POST", "/api/v1/internal/servers/missing/join-event",
			`{"mc_uuid":"11111111-1111-1111-1111-111111111111"}`, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
	})
}

// ---- claim (§9.3) ----

func TestClaimStateMachine(t *testing.T) {
	user := &Principal{UserID: "u1", Email: "u1@example.net", Role: "user", ViaAdminAccess: false}

	t.Run("not linked -> 412", func(t *testing.T) {
		repo := newFakeRepo()
		api := newTestAPI(repo, newFakeCluster())
		api.External = staticExternal{p: user}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/claim", "", nil)
		if w.Code != http.StatusPreconditionFailed || decodeErr(t, w) != "not_linked" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
	t.Run("over quota -> 403", func(t *testing.T) {
		repo := newFakeRepo()
		repo.linked["u1"] = true
		api := newTestAPI(repo, newFakeCluster())
		api.External = staticExternal{p: user}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/claim", "", nil)
		if w.Code != http.StatusForbidden || decodeErr(t, w) != "quota_exceeded" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
	t.Run("already claimed -> 409", func(t *testing.T) {
		repo := newFakeRepo()
		repo.linked["u1"] = true
		repo.quota["u1"] = true
		repo.claimOK["survival"] = false // row exists but owner already set
		api := newTestAPI(repo, newFakeCluster())
		api.External = staticExternal{p: user}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/claim", "", nil)
		if w.Code != http.StatusConflict || decodeErr(t, w) != "already_claimed" {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
	t.Run("success -> 200 + audit", func(t *testing.T) {
		repo := newFakeRepo()
		repo.linked["u1"] = true
		repo.quota["u1"] = true
		repo.claimOK["survival"] = true
		api := newTestAPI(repo, newFakeCluster())
		api.External = staticExternal{p: user}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/claim", "", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if len(repo.audits) != 1 || repo.audits[0].Action != "claim" || repo.audits[0].Actor != "u1@example.net" {
			t.Fatalf("audit not written as expected: %+v", repo.audits)
		}
	})
}

// ---- wake (autostartPolicy gate + cooldown) ----

func TestWakeAutostartGate(t *testing.T) {
	mk := func(policy string) (*API, *fakeCluster) {
		repo := newFakeRepo()
		cl := newFakeCluster()
		cl.byName["survival"] = &ServerInfo{Name: "survival", AutostartPolicy: policy}
		api := newTestAPI(repo, cl)
		return api, cl
	}
	stranger := &Principal{UserID: "stranger", Role: "user"}

	t.Run("public: any user wakes", func(t *testing.T) {
		api, cl := mk("public")
		api.External = staticExternal{p: stranger}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
		if cl.desired["survival"] != v1alpha1.DesiredRunning {
			t.Fatalf("desiredState = %q, want Running", cl.desired["survival"])
		}
	})
	t.Run("ownerOnly: stranger forbidden", func(t *testing.T) {
		api, cl := mk("ownerOnly")
		api.External = staticExternal{p: stranger}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
		if _, set := cl.desired["survival"]; set {
			t.Fatal("desiredState must not change on a forbidden wake")
		}
	})
	t.Run("ownerOnly: owner wakes", func(t *testing.T) {
		api, _ := mk("ownerOnly")
		api.Repo.(*fakeRepo).byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
		api.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d body %s", w.Code, w.Body.String())
		}
	})
	t.Run("allowlist: only listed user wakes", func(t *testing.T) {
		api, _ := mk("allowlist")
		repo := api.Repo.(*fakeRepo)
		repo.allowlist["survival"] = map[string]bool{"friend": true}
		api.External = staticExternal{p: &Principal{UserID: "friend", Role: "user"}}
		if w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != http.StatusAccepted {
			t.Fatalf("listed user: code = %d", w.Code)
		}
		api.External = staticExternal{p: stranger}
		if w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != http.StatusForbidden {
			t.Fatalf("stranger: code = %d, want 403", w.Code)
		}
	})
}

func TestWakeCooldown(t *testing.T) {
	repo := newFakeRepo()
	cl := newFakeCluster()
	cl.byName["survival"] = &ServerInfo{Name: "survival", AutostartPolicy: "public"}
	api := newTestAPI(repo, cl)
	api.WakeCooldown = time.Minute
	api.External = staticExternal{p: &Principal{UserID: "u", Role: "user"}}
	h := api.ExternalHandler()

	if w := do(h, "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != http.StatusAccepted {
		t.Fatalf("first wake code = %d", w.Code)
	}
	// clock is frozen, so the second wake is inside the cooldown window
	if w := do(h, "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second wake code = %d, want 429", w.Code)
	}
}

// TestWakeRunningCap exercises the §9.1 cluster-wide concurrency lever on the
// external wake path. The cap counts CRD truth via ListServers (never Postgres,
// per §1) and is a default-off lever: MaxRunningServers <= 0 disables it exactly
// as a zero WakeCooldown disables the per-server throttle. A full cluster answers
// 503 at_capacity — distinct from the cooldown's 429 — and an already-Running
// target re-wakes idempotently regardless of the cap.
func TestWakeRunningCap(t *testing.T) {
	// capAPI builds an external-face API whose cluster already holds `running`
	// servers desired-Running plus a stopped "survival" target, with the cap set.
	capAPI := func(cap, running int) (*API, *fakeCluster) {
		cl := newFakeCluster()
		target := &ServerInfo{Name: "survival", AutostartPolicy: "public",
			DesiredState: string(v1alpha1.DesiredStopped)}
		cl.byName["survival"] = target
		cl.list = []ServerInfo{*target}
		for i := 0; i < running; i++ {
			cl.list = append(cl.list, ServerInfo{Name: fmt.Sprintf("running-%d", i),
				DesiredState: string(v1alpha1.DesiredRunning)})
		}
		api := newTestAPI(newFakeRepo(), cl)
		api.MaxRunningServers = cap
		api.External = staticExternal{p: &Principal{UserID: "u", Role: "user"}}
		return api, cl
	}

	t.Run("at cap: wake rejected with 503 at_capacity", func(t *testing.T) {
		api, cl := capAPI(2, 2)
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("code = %d, want 503", w.Code)
		}
		if code := decodeErr(t, w); code != "at_capacity" {
			t.Fatalf("error code = %q, want at_capacity", code)
		}
		if _, set := cl.desired["survival"]; set {
			t.Fatal("desiredState must not change when the cluster is at capacity")
		}
	})

	t.Run("below cap: wake accepted", func(t *testing.T) {
		api, cl := capAPI(3, 2)
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil)
		if w.Code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202", w.Code)
		}
		if cl.desired["survival"] != v1alpha1.DesiredRunning {
			t.Fatalf("desired = %q, want Running", cl.desired["survival"])
		}
	})

	t.Run("cap disabled (0): wake accepted even when many run", func(t *testing.T) {
		api, _ := capAPI(0, 5)
		if w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202", w.Code)
		}
	})

	t.Run("already-Running target re-wakes despite a full cap (idempotent)", func(t *testing.T) {
		api, cl := capAPI(2, 2)
		cl.byName["survival"].DesiredState = string(v1alpha1.DesiredRunning)
		if w := do(api.ExternalHandler(), "POST", "/api/v1/servers/survival/wake", "", nil); w.Code != http.StatusAccepted {
			t.Fatalf("code = %d, want 202 (idempotent re-wake)", w.Code)
		}
	})
}

// ---- Zero-Trust admin boundary (§14) ----

func TestAdminBoundary(t *testing.T) {
	api := newTestAPI(newFakeRepo(), newFakeCluster())

	t.Run("user role rejected before handler", func(t *testing.T) {
		api.External = staticExternal{p: &Principal{UserID: "u", Role: "user", ViaAdminAccess: false}}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers", `{}`, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
	})
	t.Run("admin role without admin access rejected", func(t *testing.T) {
		api.External = staticExternal{p: &Principal{UserID: "a", Role: "admin", ViaAdminAccess: false}}
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers", `{}`, nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("role=admin but panel path: code = %d, want 403", w.Code)
		}
	})
	t.Run("admin via admin-access reaches handler", func(t *testing.T) {
		api.External = staticExternal{p: &Principal{UserID: "a", Role: "admin", ViaAdminAccess: true}}
		// The body is empty so the real handler rejects it (validation 400 with no
		// Builder → 503), but the point is that the admin Zero-Trust path is NOT
		// stopped at the 403 boundary — it reaches the handler.
		w := do(api.ExternalHandler(), "POST", "/api/v1/servers", `{}`, nil)
		if w.Code == http.StatusForbidden {
			t.Fatalf("admin via admin-access must reach the handler, got 403")
		}
	})
}

// ---- error envelope ----

func TestErrorEnvelopeHasRequestID(t *testing.T) {
	api := newTestAPI(newFakeRepo(), newFakeCluster())
	api.External = staticExternal{p: &Principal{UserID: "u", Role: "user"}}
	w := do(api.ExternalHandler(), "GET", "/api/v1/servers/survival/status", "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
	if w.Header().Get("X-Request-Id") == "" {
		t.Fatal("missing X-Request-Id response header")
	}
	var raw map[string]map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if raw["error"]["request_id"] == "" {
		t.Fatal("error envelope missing request_id")
	}
}

// ---- real AccessVerifier (JWT aud) ----

func TestAccessVerifier(t *testing.T) {
	key := []byte("test-signing-key")
	keyfunc := func(*jwt.Token) (any, error) { return key, nil }
	v := AccessVerifier{Audience: "felis-app", AdminAudience: "felis-admin", Keyfunc: keyfunc}

	sign := func(claims accessClaims) string {
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
		s, err := tok.SignedString(key)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return s
	}
	exp := jwt.NewNumericDate(time.Now().Add(time.Hour))

	t.Run("valid app token", func(t *testing.T) {
		s := sign(accessClaims{Email: "u@example.net", RegisteredClaims: jwt.RegisteredClaims{
			Subject: "u1", Audience: jwt.ClaimStrings{"felis-app"}, ExpiresAt: exp}})
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+s)
		p, err := v.Authenticate(r)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if p.UserID != "u1" || p.Email != "u@example.net" || p.Role != "user" || p.ViaAdminAccess {
			t.Fatalf("unexpected principal %+v", p)
		}
	})
	t.Run("admin audience sets ViaAdminAccess", func(t *testing.T) {
		s := sign(accessClaims{Role: "admin", RegisteredClaims: jwt.RegisteredClaims{
			Subject: "a1", Audience: jwt.ClaimStrings{"felis-app", "felis-admin"}, ExpiresAt: exp}})
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Cf-Access-Jwt-Assertion", s)
		p, err := v.Authenticate(r)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if !p.IsAdmin() {
			t.Fatalf("expected admin principal, got %+v", p)
		}
	})
	t.Run("wrong audience rejected", func(t *testing.T) {
		s := sign(accessClaims{RegisteredClaims: jwt.RegisteredClaims{
			Subject: "u1", Audience: jwt.ClaimStrings{"someone-else"}, ExpiresAt: exp}})
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+s)
		if _, err := v.Authenticate(r); err == nil {
			t.Fatal("expected audience rejection")
		}
	})
	t.Run("wrong signing key rejected", func(t *testing.T) {
		tok := jwt.NewWithClaims(jwt.SigningMethodHS256, accessClaims{RegisteredClaims: jwt.RegisteredClaims{
			Subject: "u1", Audience: jwt.ClaimStrings{"felis-app"}, ExpiresAt: exp}})
		s, _ := tok.SignedString([]byte("attacker-key"))
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+s)
		if _, err := v.Authenticate(r); err == nil {
			t.Fatal("expected signature rejection")
		}
	})
	t.Run("missing expiry rejected", func(t *testing.T) {
		s := sign(accessClaims{RegisteredClaims: jwt.RegisteredClaims{
			Subject: "u1", Audience: jwt.ClaimStrings{"felis-app"}}})
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+s)
		if _, err := v.Authenticate(r); err == nil {
			t.Fatal("expected missing-expiry rejection")
		}
	})
}
