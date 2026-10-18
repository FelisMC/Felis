package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"felis.lolicon.best/internal/heartbeatsettings"
)

const heartbeatPath = "/api/v1/settings/heartbeat"

func readHeartbeat(t *testing.T, a *API) HeartbeatView {
	t.Helper()
	w := do(a.ExternalHandler(), "GET", heartbeatPath, "", nil)
	var v HeartbeatView
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		t.Fatalf("read = %d %s", w.Code, w.Body.String())
	}
	return v
}

func TestHeartbeatSettingKeepsItsKeyPrivate(t *testing.T) {
	repo := newFakeRepo()
	a := newTestAPI(repo, newFakeCluster())
	a.External = staticExternal{p: &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true}}
	v := readHeartbeat(t, a)
	if v.Set || v.Display != "" {
		t.Fatalf("unset = %+v", v)
	}
	put := func(url, revision string) int {
		body, _ := json.Marshal(map[string]string{"url": url, "revision": revision})
		return do(a.ExternalHandler(), "PUT", heartbeatPath, string(body), jsonHeader).Code
	}
	if code := put("ftp://hc-ping.com/key", v.Revision); code != http.StatusBadRequest {
		t.Errorf("a bad URL = %d", code)
	}
	if code := put("https://hc-ping.com/secret-key", "stale"); code != http.StatusConflict {
		t.Errorf("a stale revision = %d", code)
	}
	if code := put(" https://hc-ping.com/secret-key ", v.Revision); code != http.StatusOK {
		t.Fatalf("save = %d", code)
	}
	w := do(a.ExternalHandler(), "GET", heartbeatPath, "", nil)
	if strings.Contains(w.Body.String(), "secret-key") {
		t.Fatalf("the view shows the check's key: %s", w.Body.String())
	}
	saved := readHeartbeat(t, a)
	if !saved.Set || saved.Display != "https://hc-ping.com/..." || saved.Revision == v.Revision {
		t.Fatalf("saved = %+v", saved)
	}
	if s, _ := heartbeatsettings.Parse(repo.settings[heartbeatsettings.Key]); s == nil || s.URL != "https://hc-ping.com/secret-key" {
		t.Fatalf("stored = %s", repo.settings[heartbeatsettings.Key])
	}
	// Another key on the same host is a different revision.
	if code := put("https://hc-ping.com/other-key", saved.Revision); code != http.StatusOK {
		t.Fatalf("change = %d", code)
	}
	if code := put("https://hc-ping.com/third-key", saved.Revision); code != http.StatusConflict {
		t.Errorf("an edit over a change made meanwhile = %d", code)
	}
	if w := do(a.ExternalHandler(), "DELETE", heartbeatPath, "", nil); w.Code != http.StatusOK {
		t.Fatalf("clear = %d", w.Code)
	}
	if v := readHeartbeat(t, a); v.Set {
		t.Fatalf("after clearing = %+v", v)
	}
	if !hasAudit(repo, "platform.heartbeat") || !hasAudit(repo, "platform.heartbeat.clear") {
		t.Error("the changes were not audited")
	}

	for _, role := range []string{"user", "admin"} {
		a.External = staticExternal{p: &Principal{UserID: "x", Role: role, ViaAdminAccess: role == "admin"}}
		for _, method := range []string{"GET", "PUT", "DELETE"} {
			if w := do(a.ExternalHandler(), method, heartbeatPath, `{}`, jsonHeader); w.Code != http.StatusForbidden {
				t.Errorf("%s %s = %d", role, method, w.Code)
			}
		}
	}
}

func TestHeartbeatSettingAsksForAFreshProof(t *testing.T) {
	repo := newFakeRepo()
	a := newTestAPI(repo, newFakeCluster())
	p := &Principal{UserID: "owner", Role: "owner", ViaAdminAccess: true, ViaSession: true, EmailVerified: true}
	repo.passkeyCreds["owner-key"] = PasskeyCredential{ID: "owner-key", UserID: p.UserID, UserVerified: true}
	a.External = staticExternal{p: p}
	v := readHeartbeat(t, a)
	body, _ := json.Marshal(map[string]string{"url": "https://hc-ping.com/k", "revision": v.Revision})
	if w := do(a.ExternalHandler(), "PUT", heartbeatPath, string(body), jsonHeader); w.Code != http.StatusForbidden || decodeErr(t, w) != "reauth_required" {
		t.Errorf("save = %d %s", w.Code, w.Body.String())
	}
	if w := do(a.ExternalHandler(), "DELETE", heartbeatPath, "", nil); w.Code != http.StatusForbidden {
		t.Errorf("clear = %d", w.Code)
	}
	if len(repo.settings) != 0 {
		t.Fatal("an unproven request wrote the setting")
	}
}
