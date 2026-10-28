package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

var (
	tagsOwner    = &Principal{UserID: "owner1", Role: "user"}
	tagsAdmin    = &Principal{UserID: "admin1", Role: "admin", ViaAdminAccess: true}
	tagsStranger = &Principal{UserID: "other", Role: "user"}
)

// tagsAPI serves survival, owned by owner1 with the tag "old", and the lobby system service, to p.
func tagsAPI(p *Principal) (*API, *fakeRepo) {
	repo := newFakeRepo()
	repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1", Tags: []string{"old"}}
	cl := newFakeCluster()
	cl.byName["survival"] = &ServerInfo{Name: "survival", Phase: "Stopped"}
	cl.byName["lobby"] = &ServerInfo{Name: "lobby", ReaperExempt: true}
	a := newTestAPI(repo, cl)
	a.External = staticExternal{p: p}
	return a, repo
}

func putTags(a *API, server, body string) *httpResult {
	return result(do(a.ExternalHandler(), "PUT", "/api/v1/servers/"+server+"/tags", body, jsonHeader))
}

func TestSetServerTagsStoresTheCleanedSet(t *testing.T) {
	twenty := strings.Repeat("生", 20)
	for _, tc := range []struct {
		name string
		p    *Principal
		body string
		want []string
	}{
		{"owner replaces the set", tagsOwner, `{"tags":["PvP","survival"]}`, []string{"PvP", "survival"}},
		{"admin replaces the set", tagsAdmin, `{"tags":["PvP","survival"]}`, []string{"PvP", "survival"}},
		{"repeats differing in case keep the first spelling", tagsOwner,
			`{"tags":["PvP","survival","pvp","SURVIVAL"]}`, []string{"PvP", "survival"}},
		{"surrounding space is dropped", tagsOwner, `{"tags":["  PvP "]}`, []string{"PvP"}},
		{"an empty list clears the set", tagsOwner, `{"tags":[]}`, []string{}},
		{"20 characters and 6 tags are allowed", tagsOwner,
			`{"tags":["` + twenty + `","b","c","d","e","f"]}`, []string{twenty, "b", "c", "d", "e", "f"}},
		{"7 entries that repeat into 6 are allowed", tagsOwner,
			`{"tags":["a","b","c","d","e","f","A"]}`, []string{"a", "b", "c", "d", "e", "f"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo := tagsAPI(tc.p)
			res := putTags(a, "survival", tc.body)
			if res.code != http.StatusOK {
				t.Fatalf("code = %d (%s)", res.code, res.body)
			}
			var got serverTagsView
			if err := json.Unmarshal([]byte(res.body), &got); err != nil || got.Name != "survival" || !slices.Equal(got.Tags, tc.want) {
				t.Fatalf("body = %s (%v), want survival with %q", res.body, err, tc.want)
			}
			if stored := repo.byName["survival"].Tags; !slices.Equal(stored, tc.want) {
				t.Fatalf("stored = %q, want %q", stored, tc.want)
			}
		})
	}
}

func TestSetServerTagsIsAudited(t *testing.T) {
	a, repo := tagsAPI(tagsOwner)
	if res := putTags(a, "survival", `{"tags":["PvP","pvp","Survival"]}`); res.code != http.StatusOK {
		t.Fatalf("code = %d (%s)", res.code, res.body)
	}
	if len(repo.audits) != 1 {
		t.Fatalf("audits = %+v", repo.audits)
	}
	e := repo.audits[0]
	if e.Action != "server.tags" || e.ServerName != "survival" || e.ActorUserID != "owner1" || string(e.Payload) != `{"tags":["PvP","Survival"]}` {
		t.Fatalf("audit = %+v (payload %s)", e, e.Payload)
	}
}

func TestSetServerTagsRefusals(t *testing.T) {
	long := strings.Repeat("a", 21)
	for _, tc := range []struct {
		name   string
		p      *Principal
		server string
		body   string
		code   int
		err    string
	}{
		{"stranger", tagsStranger, "survival", `{"tags":["x"]}`, http.StatusForbidden, "forbidden"},
		{"unknown server", tagsAdmin, "creative", `{"tags":["x"]}`, http.StatusNotFound, "not_found"},
		{"system service for a player", tagsOwner, "lobby", `{"tags":["x"]}`, http.StatusBadRequest, "bad_name"},
		{"system service for an admin", tagsAdmin, "lobby", `{"tags":["x"]}`, http.StatusConflict, "system_server"},
		{"21 characters", tagsOwner, "survival", `{"tags":["` + long + `"]}`, http.StatusBadRequest, "bad_tags"},
		{"empty after trimming", tagsOwner, "survival", `{"tags":["  "]}`, http.StatusBadRequest, "bad_tags"},
		{"a comma", tagsOwner, "survival", `{"tags":["a,b"]}`, http.StatusBadRequest, "bad_tags"},
		{"a control character", tagsOwner, "survival", `{"tags":["a\u0007b"]}`, http.StatusBadRequest, "bad_tags"},
		{"7 distinct tags", tagsOwner, "survival", `{"tags":["a","b","c","d","e","f","g"]}`, http.StatusBadRequest, "bad_tags"},
		{"no tags field", tagsOwner, "survival", `{}`, http.StatusBadRequest, "bad_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo := tagsAPI(tc.p)
			res := putTags(a, tc.server, tc.body)
			if res.code != tc.code || res.errCode() != tc.err {
				t.Fatalf("code = %d %q, want %d %q (%s)", res.code, res.errCode(), tc.code, tc.err, res.body)
			}
			if stored := repo.byName["survival"].Tags; !slices.Equal(stored, []string{"old"}) {
				t.Fatalf("a refused request changed the tags to %q", stored)
			}
			if len(repo.audits) != 0 {
				t.Fatalf("a refused request was audited: %+v", repo.audits)
			}
		})
	}
}

func TestServerStatusShowsTagsOnlyToOwnerAndStaff(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    *Principal
		want bool
	}{
		{"owner", tagsOwner, true},
		{"admin", tagsAdmin, true},
		{"stranger", tagsStranger, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := tagsAPI(tc.p)
			res := result(do(a.ExternalHandler(), "GET", "/api/v1/servers/survival/status", "", nil))
			if res.code != http.StatusOK {
				t.Fatalf("code = %d (%s)", res.code, res.body)
			}
			if shown := strings.Contains(res.body, `"tags":["old"]`); shown != tc.want {
				t.Fatalf("tags shown = %v, want %v (%s)", shown, tc.want, res.body)
			}
		})
	}
}
