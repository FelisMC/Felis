package api

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestExtraPortsPatchPermissionsAndRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, path, server string
		principal          *Principal
		want               int
	}{
		{"owner", "/api/v1/me/servers/survival", "survival", &Principal{UserID: "owner1", Role: "user"}, http.StatusOK},
		{"administrator", "/api/v1/servers/survival", "survival", &Principal{UserID: "staff", Role: "admin", ViaAdminAccess: true}, http.StatusOK},
		{"system administrator", "/api/v1/servers/login", "login", &Principal{UserID: "staff", Role: "admin", ViaAdminAccess: true}, http.StatusOK},
		{"other owner", "/api/v1/me/servers/survival", "survival", &Principal{UserID: "other", Role: "user"}, http.StatusForbidden},
		{"owner on admin route", "/api/v1/servers/survival", "survival", &Principal{UserID: "owner1", Role: "user"}, http.StatusForbidden},
		{"anonymous", "/api/v1/me/servers/survival", "survival", nil, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, repo, cl, _ := newPatchAPI()
			repo.byName["survival"].OwnerID = "owner1"
			a.External = staticExternal{p: tc.principal}
			if tc.server == "login" {
				cl.byName["login"] = &ServerInfo{Name: "login", Phase: "Stopped"}
			}
			body := `{"extraPorts":[{"protocol":"UDP","port":24454},{"protocol":"TCP","port":8123}]}`
			w := do(a.ExternalHandler(), "PATCH", tc.path, body, jsonHeader)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.want != http.StatusOK {
				if len(cl.patched) != 0 || len(repo.audits) != 0 {
					t.Fatal("unauthorized port change had side effects")
				}
				return
			}
			want := []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolUDP, Port: 24454}, {Protocol: corev1.ProtocolTCP, Port: 8123}}
			if got := cl.patched[tc.server].ExtraPorts; got == nil || !reflect.DeepEqual(*got, want) {
				t.Fatal("patch did not carry the requested ports")
			}
			if len(cl.ensured) != 0 || len(cl.acquired) != 0 || len(cl.retried) != 0 {
				t.Fatal("pure port configuration started workload or acquired the world")
			}
			if len(repo.audits) != 1 || string(repo.audits[0].Payload) != `{"fields":["extraPorts"]}` || strings.Contains(w.Body.String(), "24454") {
				t.Fatal("port values leaked into the audit or patch acknowledgement")
			}
			w = do(a.ExternalHandler(), "GET", "/api/v1/servers/"+tc.server+"/status", "", nil)
			if w.Code != http.StatusOK {
				t.Fatal(w.Body.String())
			}
			var info ServerInfo
			if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
				t.Fatal(err)
			}
			if !info.ExtraPortsPending || !reflect.DeepEqual(info.ExtraPorts, []ServerExtraPort{{Protocol: "UDP", Port: 24454}, {Protocol: "TCP", Port: 8123}}) {
				t.Fatal("requested ports were omitted or reported active before allocation")
			}
		})
	}
}

func TestExtraPortsPatchStrictJSONAndAtomicRejection(t *testing.T) {
	for _, owner := range []bool{false, true} {
		for _, tc := range []struct{ body, code string }{
			{`{"extraPorts":[{"protocol":"UDP","port":24454,"nodePort":30100}]}`, "bad_request"},
			{`{"extraPorts":[{"protocol":"UDP","port":24454,"address":"example.com"}]}`, "bad_request"},
			{`{"extraPorts":{}}`, "bad_request"},
			{`{"extraPorts":[{"protocol":1,"port":24454}]}`, "bad_request"},
			{`{"extraPorts":[{"protocol":"UDP","port":"24454"}]}`, "bad_request"},
			{`{"extraPorts":[null]}`, "bad_extra_ports"},
			{`{"extraPorts":[{"port":24454}]}`, "bad_extra_ports"},
			{`{"extraPorts":[{"protocol":"UDP","port":null}]}`, "bad_extra_ports"},
			{`{"extraPorts":[{"protocol":"tcp","port":8123}]}`, "bad_extra_ports"},
			{`{"extraPorts":[{"protocol":"UDP","port":65536}]}`, "bad_extra_ports"},
			{`{"displayName":"Must not apply","args":["--changed"],"extraPorts":[{"protocol":"TCP","port":25575}]}`, "bad_extra_ports"},
		} {
			t.Run(map[bool]string{false: "admin/", true: "owner/"}[owner]+tc.body, func(t *testing.T) {
				a, repo, cl := newStartupSettingsAPI()
				path := "/api/v1/servers/survival"
				if owner {
					a.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
					path = "/api/v1/me/servers/survival"
				}
				spec := cl.startupSpecs["survival"]
				before := spec.DeepCopy()
				w := do(a.ExternalHandler(), "PATCH", path, tc.body, jsonHeader)
				if w.Code != http.StatusBadRequest || decodeErr(t, w) != tc.code {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				if len(cl.patched) != 0 || len(repo.audits) != 0 || !reflect.DeepEqual(cl.startupSpecs["survival"], *before) || cl.byName["survival"].DisplayName == "Must not apply" {
					t.Fatal("invalid nested port data partially applied a mixed patch")
				}
			})
		}
	}
}

func TestExtraPortsOmittedNullAndEmpty(t *testing.T) {
	for _, tc := range []struct {
		body  string
		clear bool
		want  int
	}{
		{`{"displayName":"Renamed"}`, false, http.StatusOK},
		{`{"displayName":"Renamed","extraPorts":null}`, false, http.StatusOK},
		{`{"extraPorts":[]}`, true, http.StatusOK},
		{`{"extraPorts":null}`, false, http.StatusBadRequest},
	} {
		t.Run(tc.body, func(t *testing.T) {
			a, _, cl, _ := newPatchAPI()
			ports := []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolUDP, Port: 24454}}
			cl.startupSpecs["survival"] = v1alpha1.MinecraftServerSpec{ExtraPorts: ports}
			w := patchSurvival(a, tc.body)
			if w.Code != tc.want {
				t.Fatal(w.Body.String())
			}
			if got := cl.startupSpecs["survival"].ExtraPorts; (tc.clear && len(got) != 0) || (!tc.clear && !reflect.DeepEqual(got, ports)) {
				t.Fatal("omitted/null/empty extraPorts semantics were lost")
			}
		})
	}
}

func TestExtraPortsStatusAndListsArePrivateToManagers(t *testing.T) {
	for _, tc := range []struct {
		path, user            string
		admin, owned, visible bool
	}{
		{"/api/v1/servers/survival/status", "owner1", false, true, true},
		{"/api/v1/servers/survival/status", "staff", true, false, true},
		{"/api/v1/servers/survival/status", "other", false, false, false},
		{"/api/v1/me/servers", "owner1", false, true, true},
		{"/api/v1/me/servers", "other", false, false, false},
	} {
		t.Run(tc.path+"/"+tc.user, func(t *testing.T) {
			a, repo, cl, _ := newPatchAPI()
			repo.byName["survival"].OwnerID = "owner1"
			cl.byName["survival"].UID = "private-cr-uid"
			cl.byName["survival"].ExtraPorts = []ServerExtraPort{{Protocol: "UDP", Port: 24454, NodePort: 30100, Address: "203.0.113.9:30100"}}
			cl.byName["survival"].ExtraPortsPending = true
			cl.list = []ServerInfo{*cl.byName["survival"]}
			repo.mine[tc.user] = []MyServerView{{Name: "survival", Owned: tc.owned}}
			a.External = staticExternal{p: &Principal{UserID: tc.user, Role: map[bool]string{false: "user", true: "admin"}[tc.admin], ViaAdminAccess: tc.admin}}
			w := do(a.ExternalHandler(), "GET", tc.path, "", nil)
			if w.Code != http.StatusOK {
				t.Fatal(w.Body.String())
			}
			if strings.Contains(w.Body.String(), `"extraPorts"`) != tc.visible || strings.Contains(w.Body.String(), `"extraPortsPending"`) != tc.visible || strings.Contains(w.Body.String(), "private-cr-uid") {
				t.Fatal("port details or CR identity crossed the management boundary")
			}
		})
	}
}

type portsReassignedRepo struct{ *fakeRepo }

func (p portsReassignedRepo) PatchServerPorts(ctx context.Context, name, ownerID string, apply func() error) error {
	p.byName[name].OwnerID = "new-owner"
	return p.fakeRepo.PatchServerPorts(ctx, name, ownerID, apply)
}

func TestMixedExtraPortsPatchRechecksOwnerAfterQuotaReservation(t *testing.T) {
	a, repo, cl, _ := newPatchAPI()
	repo.byName["survival"].OwnerID = "owner1"
	repo.quota["owner1"] = true
	cl.byName["survival"].Resources = corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi"), corev1.ResourceCPU: resource.MustParse("500m")}}
	a.Repo = portsReassignedRepo{repo}
	a.External = staticExternal{p: &Principal{UserID: "owner1", Role: "user"}}
	w := do(a.ExternalHandler(), "PATCH", "/api/v1/me/servers/survival", `{"memory":"1Gi","extraPorts":[{"protocol":"UDP","port":24454}]}`, jsonHeader)
	if w.Code != http.StatusForbidden || len(cl.patched) != 0 || len(repo.audits) != 0 || len(repo.quotaChecked) == 0 {
		t.Fatalf("stale owner changed the cluster after quota work: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestDeleteUserWaitsForPortCleanupBeforeReleasingOwnership(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "pending"}[fail], func(t *testing.T) {
			repo, cl := newFakeRepo(), newFakeCluster()
			repo.seedUser(UserView{ID: "owner1", Username: "old-owner", Role: "user"})
			repo.byName["survival"] = &ServerRecord{Name: "survival", OwnerID: "owner1"}
			repo.byName["other"] = &ServerRecord{Name: "other", OwnerID: "new-owner"}
			cl.byName["survival"] = &ServerInfo{Name: "survival", UID: "ports-cr"}
			if fail {
				cl.clearPortsErr = newError(http.StatusConflict, "extra_ports_pending", "cleanup pending")
			}
			a := newTestAPI(repo, cl)
			a.External = staticExternal{p: &Principal{UserID: "platform-owner", Role: "owner", ViaAdminAccess: true}}
			w := do(a.ExternalHandler(), "DELETE", "/api/v1/users/owner1", "", nil)
			want := http.StatusOK
			if fail {
				want = http.StatusConflict
			}
			if w.Code != want || !reflect.DeepEqual(cl.clearedPorts, []string{"survival/ports-cr"}) || repo.deletedIDs["owner1"] == fail {
				t.Fatalf("cleanup/deletion status=%d calls=%v body=%s", w.Code, cl.clearedPorts, w.Body.String())
			}
			if (repo.byName["survival"].OwnerID == "") == fail || repo.byName["other"].OwnerID != "new-owner" {
				t.Fatal("cleanup failure released ownership or touched another user's server")
			}
		})
	}
}
