package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestStaffProfileDesignation(t *testing.T) {
	const native = "123456781234423482341234567890ab"
	stubMojangNames(t)
	upstreamCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/profiles/minecraft":
			var names []string
			if err := json.NewDecoder(r.Body).Decode(&names); err != nil || len(names) != 1 || names[0] != "LemonMiaow" {
				t.Errorf("lookup names = %v err = %v", names, err)
			}
			_, _ = w.Write([]byte(`[{"id":"` + native + `","name":"LemonMiaow"}]`))
		case strings.HasPrefix(r.URL.Path, "/sessionserver/session/minecraft/profile/"):
			_, _ = w.Write([]byte(`{"id":"` + native + `","name":"LemonMiaow"}`))
		case strings.HasSuffix(r.URL.Path, "/hasJoined"):
			_, _ = w.Write([]byte(`{"id":"` + native + `","name":"LemonMiaow"}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	repo := newFakeRepo()
	repo.seedUser(UserView{ID: "owner", Username: "owner", Role: "owner"})
	a := newTestAPI(repo, newFakeCluster())
	p := &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true}
	a.External = staticExternal{p: p}
	src := AuthSource{Tag: "littleskin", Prefix: "LS", URL: server.URL + "/sessionserver/session/minecraft/hasJoined"}
	a.AuthSources = []AuthSource{src, {Tag: "custom", URL: server.URL + "/custom-check"}}
	h := a.ExternalHandler()
	lookup := func(source, profile string) *httptest.ResponseRecorder {
		return do(h, "GET", "/api/v1/account/link/profile?"+url.Values{"source": {source}, "profile": {profile}}.Encode(), "", nil)
	}
	w := do(h, "GET", "/api/v1/account/link/sources", "", nil)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), server.URL) {
		t.Fatalf("sources = %d %s", w.Code, w.Body.String())
	}
	w = lookup("littleskin", "LemonMiaow")
	if w.Code != http.StatusOK {
		t.Fatalf("lookup = %d %s", w.Code, w.Body.String())
	}
	var profile linkedProfile
	if err := json.Unmarshal(w.Body.Bytes(), &profile); err != nil {
		t.Fatal(err)
	}
	canonical, _ := canonicalProfileUUID(src, native)
	if profile.MCUUID != canonical.String() || profile.AuthSource != "thirdparty" || len(repo.links) != 0 {
		t.Fatalf("preview = %+v links = %v", profile, repo.links)
	}
	game := httptest.NewRecorder()
	a.handleHasJoined(game, httptest.NewRequest("GET", "/session/minecraft/hasJoined?username=LemonMiaow&serverId=abc123", nil))
	var authenticated sessionProfile
	if err := json.Unmarshal(game.Body.Bytes(), &authenticated); err != nil || game.Code != http.StatusOK || authenticated.ID != strings.ReplaceAll(profile.MCUUID, "-", "") {
		t.Fatalf("preview differs from game identity: %d %s, err = %v", game.Code, game.Body.String(), err)
	}
	body := `{"source":"littleskin","profile_uuid":"` + native + `"}`
	w = do(h, "POST", "/api/v1/account/link/profile", body, jsonHeader)
	if w.Code != http.StatusOK || repo.links[canonical.String()] != "owner" {
		t.Fatalf("bind = %d %s links = %v", w.Code, w.Body.String(), repo.links)
	}
	// Server-side designation is idempotent and never consumes game link codes.
	w = do(h, "POST", "/api/v1/account/link/profile", body, jsonHeader)
	if w.Code != http.StatusOK || len(repo.links) != 1 {
		t.Fatalf("repeat = %d %s", w.Code, w.Body.String())
	}
	repo.links[canonical.String()] = "another-user"
	w = do(h, "POST", "/api/v1/account/link/profile", body, jsonHeader)
	if w.Code != http.StatusConflict || repo.links[canonical.String()] != "another-user" {
		t.Fatal("designation overwrote another user")
	}
	w = lookup("custom", "LemonMiaow")
	if w.Code != http.StatusBadRequest || decodeErr(t, w) != "auth_source_lookup_unsupported" {
		t.Fatalf("custom = %d %s", w.Code, w.Body.String())
	}
	before := upstreamCalls
	w = lookup("unknown", "LemonMiaow")
	if w.Code != http.StatusBadRequest || upstreamCalls != before {
		t.Fatal("unknown source queried an upstream")
	}
	w = do(h, "POST", "/api/v1/account/link/profile", `{"source":"littleskin","profile_uuid":"`+native+`","user_id":"victim"}`, jsonHeader)
	if w.Code != http.StatusBadRequest {
		t.Fatal("caller can select another account")
	}
	// A local staff session must prove its factor before designating a role.
	p.ViaSession = true
	repo.passkeyCreds["owner-key"] = PasskeyCredential{ID: "owner-key", UserID: p.UserID, UserVerified: true}
	before = upstreamCalls
	w = do(h, "POST", "/api/v1/account/link/profile", body, jsonHeader)
	if w.Code != http.StatusForbidden || decodeErr(t, w) != "reauth_required" || upstreamCalls != before {
		t.Fatalf("stale staff session = %d %s", w.Code, w.Body.String())
	}
	p.ReauthAt = a.now()
	repo.links[canonical.String()] = "owner"
	w = do(h, "POST", "/api/v1/account/link/profile", body, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("proven staff session = %d %s", w.Code, w.Body.String())
	}
	// A player's session never reaches either lookup or designation.
	p.Role = "user"
	before = upstreamCalls
	for _, w := range []*httptest.ResponseRecorder{lookup("littleskin", "LemonMiaow"), do(h, "POST", "/api/v1/account/link/profile", body, jsonHeader)} {
		if w.Code != http.StatusForbidden {
			t.Fatalf("player route = %d %s", w.Code, w.Body.String())
		}
	}
	if upstreamCalls != before {
		t.Fatal("player reached role lookup")
	}
}

func TestProfileLookupRejectsInvalidResponses(t *testing.T) {
	const native = "123456781234423482341234567890ab"
	for _, tc := range []struct {
		name, body   string
		status, want int
	}{
		{"missing", "", 204, 404},
		{"unavailable", "", 503, 503},
		{"invalid JSON", "broken", 200, 502},
		{"different UUID", `{"id":"223456781234423482341234567890ab","name":"LemonMiaow"}`, 200, 502},
		{"invalid name", `{"id":"` + native + `","name":"invalid name"}`, 200, 502},
		{"redirect", "", 302, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			a := newTestAPI(newFakeRepo(), newFakeCluster())
			a.AuthSources = []AuthSource{{Tag: "test", APIURL: server.URL}}
			_, err := a.lookupProfile(t.Context(), "test", native)
			var apiErr *apiError
			if !errors.As(err, &apiErr) || apiErr.status != tc.want {
				t.Fatalf("lookup err = %v, want %d", err, tc.want)
			}
		})
	}
}

func TestOfficialProfileUUIDIsPreserved(t *testing.T) {
	const native = "123456781234423482341234567890ab"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + native + `","name":"LemonMiaow"}`))
	}))
	defer server.Close()
	a := newTestAPI(newFakeRepo(), newFakeCluster())
	a.AuthSources = []AuthSource{{Tag: "mojang", Identity: true, URL: server.URL + "/session/minecraft/hasJoined"}}
	profile, err := a.lookupProfile(t.Context(), "mojang", native)
	if err != nil || profile.MCUUID != uuid.MustParse(native).String() || profile.AuthSource != "mojang" {
		t.Fatalf("profile = %+v err = %v", profile, err)
	}
}
