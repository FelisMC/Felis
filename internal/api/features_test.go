package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/software"
)

type teamFakeSoftware struct{}

func (teamFakeSoftware) Versions(context.Context, software.Type) ([]software.Version, error) {
	return []software.Version{{ID: "1.21.4", Stable: true}}, nil
}
func (teamFakeSoftware) Builds(context.Context, software.Type, string) ([]software.Build, error) {
	return []software.Build{{ID: "100", Recommended: true}}, nil
}
func (teamFakeSoftware) RequiredJava(context.Context, software.Type, string) (int, error) {
	return 21, nil
}
func (teamFakeSoftware) ForwardingFor(context.Context, software.Spec) (string, error) {
	return "", nil
}

func newTeamTestAPI(repo Repo, cl Cluster) *API {
	return &API{
		Repo:                   repo,
		Cluster:                cl,
		Internal:               okInternal{},
		RootDomain:             testRoot,
		LobbyImage:             "felis/lobby:demo",
		LobbyDisabledByDefault: true,
		Now:                    func() time.Time { return time.Unix(1_700_000_000, 0) },
		AccessLog:              discardLog,
	}
}

func TestFeaturesFailClosedDefaults(t *testing.T) {
	repo := newFakeRepo()
	a := newTeamTestAPI(repo, newFakeCluster())
	a.External = staticExternal{p: &Principal{UserID: "u-owner", Email: "owner@example.com", Role: "owner", ViaAdminAccess: true, EmailVerified: true}}

	w := do(a.ExternalHandler(), "GET", "/api/v1/settings/features", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var got platformFeaturesView
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Mode != ModeTeam || got.UseLobby || got.RequireAccountLink || got.CustomImages || got.AllowSubmissions || !got.LobbyAvailable {
		t.Fatalf("unexpected default features: %+v", got)
	}
	if got.Revision == "" {
		t.Fatal("expected non-empty revision")
	}

	w = do(a.ExternalHandler(), "GET", "/api/v1/auth/owner-status", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("owner-status=%d body=%s", w.Code, w.Body.String())
	}
	var st ownerStatusView
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Mode != ModeTeam {
		t.Fatalf("owner-status mode=%q, want team", st.Mode)
	}
}

func TestTeamModeBlocksPlayerPortalAndAdmitsCollaborator(t *testing.T) {
	repo := newFakeRepo()
	repo.settings[LocalAuthEnabledKey] = []byte("true")
	repo.linkCodes["ABCD-1234"] = fakeLinkCode{mcUUID: "00000000-0000-0000-0000-000000000001", expiresAt: time.Unix(1_700_000_600, 0)}

	a := newTeamTestAPI(repo, newFakeCluster())
	a.External = staticExternal{p: &Principal{UserID: "u-player", Email: "p@example.com", Role: "user", EmailVerified: true}}

	w := do(a.ExternalHandler(), "GET", "/api/v1/me", "", nil)
	if w.Code != http.StatusForbidden || decodeErr(t, w) != "team_mode_staff_only" {
		t.Fatalf("player /me in team mode: status=%d code=%s", w.Code, decodeErr(t, w))
	}

	w = do(a.ExternalHandler(), "POST", "/api/v1/auth/bind", `{"code":"ABCD-1234"}`, jsonHeader)
	if w.Code != http.StatusForbidden || decodeErr(t, w) != "team_mode_staff_only" {
		t.Fatalf("bind redeem in team mode: status=%d code=%s", w.Code, decodeErr(t, w))
	}
	if _, ok := repo.linkCodes["ABCD-1234"]; !ok {
		t.Fatal("bind code was consumed despite team-mode refusal")
	}

	repo.staff["p@example.com"] = &StaffUser{ID: "u-player", Username: "player", Email: "p@example.com", Role: "user", EmailVerified: true}
	repo.otps["otp1"] = &fakeEmailOTP{
		userID: "u-player", email: "p@example.com", purpose: otpPurposeLogin,
		codeHash: otpCodeHash("123456"), expiresAt: time.Unix(1_700_000_600, 0),
	}
	w = do(a.ExternalHandler(), "POST", "/api/v1/auth/email/verify", `{"email":"p@example.com","code":"123456"}`, jsonHeader)
	if w.Code != http.StatusForbidden || decodeErr(t, w) != "team_mode_staff_only" {
		t.Fatalf("email login in team mode: status=%d code=%s", w.Code, decodeErr(t, w))
	}
	if len(repo.sessions) != 0 {
		t.Fatalf("session minted for blocked user in team mode: %d", len(repo.sessions))
	}

	repo.byName["hub"] = &ServerRecord{Name: "hub", Subdomain: "hub"}
	repo.collaborators["hub"] = map[string]string{"u-player": CollabRoleOperator}
	w = do(a.ExternalHandler(), "GET", "/api/v1/me", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("collaborator /me in team mode: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestTeamModeGuardsCommunityEndpointsAndDefaults(t *testing.T) {
	repo := newFakeRepo()
	repo.byName["srv1"] = &ServerRecord{Name: "srv1", Subdomain: "srv1"}
	repo.owners["srv1"] = ServerOwnership{}
	repo.linked["u-owner"] = true
	repo.quota["u-owner"] = true
	cl := newFakeCluster()
	srv := ServerInfo{Name: "srv1", Subdomain: "srv1", Phase: "Stopped"}
	cl.byName["srv1"] = &srv
	cl.list = []ServerInfo{srv}

	a := newTeamTestAPI(repo, cl)
	a.Software = teamFakeSoftware{}
	a.RuntimeRegistry = "registry.felis.svc:5000"
	a.External = staticExternal{p: &Principal{UserID: "u-owner", Email: "owner@example.com", Role: "owner", ViaAdminAccess: true, EmailVerified: true}}

	for _, tc := range []struct {
		method, path, body string
	}{
		{"POST", "/api/v1/servers/srv1/claim", ""},
		{"GET", "/api/v1/me/server-quota", ""},
		{"POST", "/api/v1/me/servers", `{}`},
		{"GET", "/api/v1/servers/srv1/assignment?user_id=u-other", ""},
		{"POST", "/api/v1/servers/srv1/assignment", `{"user_id":"u-other"}`},
	} {
		w := do(a.ExternalHandler(), tc.method, tc.path, tc.body, jsonHeader)
		if w.Code != http.StatusForbidden || decodeErr(t, w) != "feature_disabled" {
			t.Fatalf("%s %s: status=%d code=%s", tc.method, tc.path, w.Code, decodeErr(t, w))
		}
	}

	w := do(a.InternalHandler(), "POST", "/api/v1/internal/servers/srv1/claim", `{"mc_uuid":"00000000-0000-0000-0000-000000000001"}`, jsonHeader)
	if w.Code != http.StatusForbidden || decodeErr(t, w) != "feature_disabled" {
		t.Fatalf("internal claim in team mode: status=%d code=%s", w.Code, decodeErr(t, w))
	}

	w = do(a.ExternalHandler(), "GET", "/api/v1/fleet", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("fleet status=%d", w.Code)
	}
	var fleet struct {
		Servers []fleetServerView `json:"servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &fleet); err != nil {
		t.Fatal(err)
	}
	if len(fleet.Servers) != 1 || fleet.Servers[0].Claimable {
		t.Fatalf("expected non-claimable fleet row in team mode: %+v", fleet.Servers)
	}

	w = do(a.ExternalHandler(), "POST", "/api/v1/servers", `{
		"name":"alpha","subdomain":"alpha","memory":"2Gi","storage":"10Gi","ownerId":"u-owner",
		"software":{"type":"paper","minecraftVersion":"1.21.4","build":"100","java":21},"acceptEula":true
	}`, jsonHeader)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("ownerId in team mode: status=%d body=%s", w.Code, w.Body.String())
	}

	w = do(a.ExternalHandler(), "POST", "/api/v1/servers", `{
		"name":"alpha","subdomain":"alpha","memory":"2Gi","storage":"10Gi",
		"software":{"type":"paper","minecraftVersion":"1.21.4","build":"100","java":21},"acceptEula":true
	}`, jsonHeader)
	if w.Code != http.StatusCreated {
		t.Fatalf("team server create: status=%d body=%s", w.Code, w.Body.String())
	}
	if got := cl.created["alpha"].AutostartPolicy; got != v1alpha1.AutostartPublic {
		t.Fatalf("team default autostartPolicy=%q, want public", got)
	}
}

func TestCustomImagesAndSubmissionsGatesPreserveExistingServers(t *testing.T) {
	repo := newFakeRepo()
	repo.byName["legacy"] = &ServerRecord{Name: "legacy", Subdomain: "legacy"}
	cl := newFakeCluster()
	cl.byName["legacy"] = &ServerInfo{
		Name: "legacy", Subdomain: "legacy", Image: "registry.felis.svc:5000/custom/modpack:v1",
		Memory: "4Gi", StorageSize: "10Gi", Phase: "Stopped",
	}

	b := &fakeBuilder{admitted: map[string]bool{"registry.felis.svc:5000/custom/modpack:v2": true}}
	subs := &fakeSubmissions{}
	a := newTeamTestAPI(repo, cl)
	a.DefaultMode = ModeCommunity
	a.Builder = b
	a.Submissions = subs
	a.External = staticExternal{p: &Principal{UserID: "u-owner", Email: "owner@example.com", Role: "owner", ViaAdminAccess: true, EmailVerified: true, ReauthAt: a.now()}}

	for _, tc := range []struct {
		method, path, body string
	}{
		{"POST", "/api/v1/images/build", `{"image_ref":"registry.felis.svc:5000/a:1","dockerfile":"FROM scratch"}`},
		{"POST", "/api/v1/images", `{"image_ref":"registry.felis.svc:5000/a:1"}`},
		{"DELETE", "/api/v1/images?ref=registry.felis.svc:5000/a:1", ""},
		{"POST", "/api/v1/me/submissions", `{"display_name":"pack"}`},
		{"POST", "/api/v1/submissions/sub-1/approve", `{}`},
		{"POST", "/api/v1/servers", `{"name":"img1","subdomain":"img1","image":"registry.felis.svc:5000/custom/modpack:v2","memory":"2Gi","storage":"10Gi"}`},
		{"PATCH", "/api/v1/servers/legacy", `{"image":"registry.felis.svc:5000/custom/modpack:v2","confirmImageChange":true}`},
	} {
		w := do(a.ExternalHandler(), tc.method, tc.path, tc.body, jsonHeader)
		if w.Code != http.StatusForbidden || decodeErr(t, w) != "feature_disabled" {
			t.Fatalf("%s %s with customImages=false: status=%d code=%s", tc.method, tc.path, w.Code, decodeErr(t, w))
		}
	}

	w := do(a.ExternalHandler(), "PATCH", "/api/v1/servers/legacy", `{"displayName":"Modpack Legacy"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("patching non-image field on existing custom-image server failed: status=%d body=%s", w.Code, w.Body.String())
	}

	w = do(a.ExternalHandler(), "GET", "/api/v1/settings/features", "", nil)
	var cur platformFeaturesView
	_ = json.Unmarshal(w.Body.Bytes(), &cur)

	putBody, _ := json.Marshal(setFeaturesRequest{
		Mode:             ModeCommunity,
		CustomImages:     true,
		AllowSubmissions: false,
		Revision:         cur.Revision,
	})
	w = do(a.ExternalHandler(), "PUT", "/api/v1/settings/features", string(putBody), jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("enable customImages: status=%d body=%s", w.Code, w.Body.String())
	}

	w = do(a.ExternalHandler(), "POST", "/api/v1/images", `{"image_ref":"registry.felis.svc:5000/a:1"}`, jsonHeader)
	if w.Code != http.StatusCreated {
		t.Fatalf("add image with customImages=true: status=%d body=%s", w.Code, w.Body.String())
	}
	w = do(a.ExternalHandler(), "POST", "/api/v1/me/submissions", `{"display_name":"pack"}`, jsonHeader)
	if w.Code != http.StatusForbidden || decodeErr(t, w) != "feature_disabled" {
		t.Fatalf("submission with allowSubmissions=false: status=%d code=%s", w.Code, decodeErr(t, w))
	}
}

func TestModeHotSwitchPreservesOwnershipAndSyncsCheckboxes(t *testing.T) {
	repo := newFakeRepo()
	repo.byName["srv1"] = &ServerRecord{Name: "srv1", Subdomain: "srv1", OwnerID: "u-alice"}
	repo.owners["srv1"] = ServerOwnership{OwnerID: "u-alice", Owner: "alice@example.com"}
	cl := newFakeCluster()
	cl.byName["srv1"] = &ServerInfo{Name: "srv1", Subdomain: "srv1", Phase: "Stopped"}

	a := newTeamTestAPI(repo, cl)
	a.External = staticExternal{p: &Principal{UserID: "u-owner", Email: "owner@example.com", Role: "owner", ViaAdminAccess: true, EmailVerified: true, ReauthAt: a.now()}}

	w := do(a.ExternalHandler(), "GET", "/api/v1/settings/features", "", nil)
	var cur platformFeaturesView
	_ = json.Unmarshal(w.Body.Bytes(), &cur)

	stale, _ := json.Marshal(setFeaturesRequest{Mode: ModeCommunity, Revision: "stale-rev"})
	w = do(a.ExternalHandler(), "PUT", "/api/v1/settings/features", string(stale), jsonHeader)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale revision: status=%d, want 409", w.Code)
	}

	tr := true
	body, _ := json.Marshal(setFeaturesRequest{
		Mode:                   ModeCommunity,
		UseLobby:               &tr,
		CustomImages:           true,
		AllowSubmissions:       true,
		SyncRequireAccountLink: &tr,
		Revision:               cur.Revision,
	})
	w = do(a.ExternalHandler(), "PUT", "/api/v1/settings/features", string(body), jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("switch to community: status=%d body=%s", w.Code, w.Body.String())
	}
	var updated platformFeaturesView
	_ = json.Unmarshal(w.Body.Bytes(), &updated)
	if updated.Mode != ModeCommunity || !updated.UseLobby || !updated.RequireAccountLink || !updated.CustomImages || !updated.AllowSubmissions {
		t.Fatalf("unexpected updated features: %+v", updated)
	}
	if _, ok := cl.byName["lobby"]; !ok {
		t.Fatal("expected lobby server to be provisioned when useLobby turned on")
	}

	fa := false
	back, _ := json.Marshal(setFeaturesRequest{
		Mode:                   ModeTeam,
		UseLobby:               &fa,
		CustomImages:           false,
		AllowSubmissions:       false,
		SyncRequireAccountLink: &fa,
		Revision:               updated.Revision,
	})
	w = do(a.ExternalHandler(), "PUT", "/api/v1/settings/features", string(back), jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("switch back to team: status=%d body=%s", w.Code, w.Body.String())
	}
	if repo.byName["srv1"].OwnerID != "u-alice" {
		t.Fatalf("owner_id was unexpectedly cleared on switch to team: %q", repo.byName["srv1"].OwnerID)
	}
	if cl.desired["lobby"] != v1alpha1.DesiredStopped {
		t.Fatalf("expected lobby stopped when useLobby turned off, got %q", cl.desired["lobby"])
	}
}
