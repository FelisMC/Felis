package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestSystemServerManagement(t *testing.T) {
	for _, name := range []string{"login", "lobby"} {
		for _, principal := range []*Principal{
			{UserID: "staff", Role: "admin", ViaAdminAccess: true},
			{UserID: "staff", Role: "admin"},
			{UserID: "player", Role: "user"},
		} {
			t.Run(fmt.Sprintf("%s/%s/operator=%t", name, principal.Role, principal.ViaAdminAccess), func(t *testing.T) {
				api, repo, cl, files := mkFiles(t)
				api.External = staticExternal{p: principal}
				cl.byName[name] = &ServerInfo{Name: name, Subdomain: name, Phase: "Stopped", DesiredState: "Stopped", ReaperExempt: true}
				for _, route := range []struct {
					method, suffix, body string
					success              int
				}{
					{"GET", "/status", "", 200},
					{"GET", "/files", "", 200},
					{"GET", "/file?path=felis-experience.json", "", 200},
					{"PUT", "/file?path=felis-experience.json", `{"content":"aGk=","content_sha256":"` + hiSum + `"}`, 200},
					{"PATCH", "", `{"displayName":"Custom Space"}`, 200},
					{"POST", "/stop", "", 202},
				} {
					before := files.calls
					result := do(api.ExternalHandler(), route.method, "/api/v1/servers/"+name+route.suffix, route.body, jsonHeader)
					if principal.IsAdmin() {
						if result.Code != route.success {
							t.Fatalf("%s %s: %d %s", route.method, route.suffix, result.Code, result.Body.String())
						}
					} else {
						if result.Code < 400 || files.calls != before {
							t.Fatalf("player management admitted: %d %s", result.Code, result.Body.String())
						}
					}
				}
				result := do(api.ExternalHandler(), "POST", "/api/v1/servers/"+name+"/claim", "", nil)
				if result.Code != http.StatusBadRequest {
					t.Fatalf("system service claim: %d", result.Code)
				}
				if _, exists := repo.byName[name]; exists {
					t.Fatal("management created a claimable business row")
				}
			})
		}
	}
}

func TestSystemServerResourcesAndInvariants(t *testing.T) {
	api, repo, cl, _ := newPatchAPI()
	cl.byName["lobby"] = &ServerInfo{Name: "lobby", ReaperExempt: true, Resources: corev1.ResourceRequirements{
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
	}}
	result := do(api.ExternalHandler(), "PATCH", "/api/v1/servers/lobby", `{"memory":"2Gi"}`, jsonHeader)
	if result.Code != http.StatusOK || cl.patched["lobby"].JavaMemory == nil {
		t.Fatalf("resource patch: %d %s", result.Code, result.Body.String())
	}
	if _, exists := repo.byName["lobby"]; exists {
		t.Fatal("system resources added an ownership row")
	}
	for _, body := range []string{`{"autostartPolicy":"ownerOnly"}`, `{"idleStopSeconds":600}`, `{"idleAction":"stop"}`} {
		result := do(api.ExternalHandler(), "PATCH", "/api/v1/servers/lobby", body, jsonHeader)
		if result.Code != http.StatusBadRequest {
			t.Fatalf("system invariant changed: %d %s", result.Code, result.Body.String())
		}
	}
	delete(cl.byName, "lobby")
	result = do(api.ExternalHandler(), "POST", "/api/v1/servers/lobby/stop", "", nil)
	if result.Code != http.StatusNotFound {
		t.Fatalf("missing cluster service: %d %s", result.Code, result.Body.String())
	}
}

func TestSystemLobbyBuilderAccess(t *testing.T) {
	api, repo, cl, console := mkAccess(t)
	api.External = staticExternal{p: &Principal{UserID: "staff", Role: "admin", ViaAdminAccess: true}}
	cl.byName["lobby"] = &ServerInfo{Name: "lobby", Phase: "Running", Ready: true}
	result := do(api.ExternalHandler(), "POST", "/api/v1/servers/lobby/access/permission",
		`{"action":"set","player":"Steve","node":"felis.lobby.build","value":true}`, jsonHeader)
	if result.Code != http.StatusOK || console.gotCommand != "lp user Steve permission set felis.lobby.build true" {
		t.Fatalf("builder grant: %d %s; command %q", result.Code, result.Body.String(), console.gotCommand)
	}
	if _, exists := repo.byName["lobby"]; exists {
		t.Fatal("builder access created an ownership row")
	}
}

func TestSystemConsoleStreamRechecksWithoutOwnershipRow(t *testing.T) {
	shrinkStreamTimers(t, 10*time.Millisecond, 80*time.Millisecond)
	a, _, cl, _ := mkAccess(t)
	a.External = staticExternal{p: &Principal{UserID: "staff", Role: "admin", ViaAdminAccess: true}}
	cl.byName["lobby"] = &ServerInfo{Name: "lobby", Phase: "Running", Ready: true}
	a.Logs = &fakeLogStreamer{srcFromCtx: func(ctx context.Context) io.ReadCloser {
		return &ctxBlockingReadCloser{ctx: ctx, first: []byte("boot\n"), firstRead: make(chan struct{}), closed: make(chan struct{})}
	}}
	begun := time.Now()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- do(a.ExternalHandler(), "GET", "/api/v1/servers/lobby/console", "", nil) }()
	select {
	case result := <-done:
		if result.Code != http.StatusOK || strings.Contains(result.Body.String(), "event: revoked") || time.Since(begun) < 80*time.Millisecond {
			t.Fatalf("system logs ended before their lifetime: %d %s", result.Code, result.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("system log stream outlived its lifetime")
	}
}

func TestLoginExecutableAndFilesAreOwnerOnly(t *testing.T) {
	admin := &Principal{UserID: "staff", Role: "admin", ViaAdminAccess: true}
	owner := &Principal{UserID: "boss", Role: "owner", ViaAdminAccess: true}

	for _, body := range []string{
		`{"java_flags":["-javaagent:/data/evil.jar"]}`,
		`{"args":["--extra"]}`,
		`{"env":[{"name":"X","value":"1"}]}`,
		`{"stop_commands":["stop"]}`,
		`{"image":"example.org/custom:latest"}`,
		`{"software":{"type":"custom","jar":"evil.jar"}}`,
	} {
		api, _, cl, _ := newPatchAPI()
		api.External = staticExternal{p: admin}
		cl.byName["login"] = &ServerInfo{Name: "login", Phase: "Stopped", DesiredState: "Stopped", ReaperExempt: true}
		w := do(api.ExternalHandler(), "PATCH", "/api/v1/servers/login", body, jsonHeader)
		if w.Code != http.StatusForbidden {
			t.Fatalf("admin PATCH login %s = %d (%s), want 403", body, w.Code, w.Body.String())
		}
		if _, ok := cl.patched["login"]; ok {
			t.Fatalf("admin PATCH login %s mutated cluster spec", body)
		}
	}

	api, _, cl, files := mkFiles(t)
	cl.byName["login"] = &ServerInfo{Name: "login", Phase: "Stopped", DesiredState: "Stopped", ReaperExempt: true}
	api.External = staticExternal{p: admin}

	for _, tc := range []struct {
		method, path, body string
	}{
		{"PUT", "/api/v1/servers/login/file?path=plugins/evil.jar", `{"content":"aGk=","content_sha256":"` + hiSum + `"}`},
		{"DELETE", "/api/v1/servers/login/file?path=felis-experience.json", ""},
		{"POST", "/api/v1/servers/login/files/mkdir?path=plugins", ""},
		{"POST", "/api/v1/servers/login/files/rename", `{"from":"a","to":"b"}`},
		{"PUT", "/api/v1/servers/login/files/upload?path=plugins/evil.jar", "payload"},
		{"POST", "/api/v1/servers/login/files/unzip?path=evil.zip", ""},
		{"POST", "/api/v1/servers/login/restore-backup", `{}`},
	} {
		before := files.calls
		w := do(api.ExternalHandler(), tc.method, tc.path, tc.body, jsonHeader)
		if w.Code != http.StatusForbidden {
			t.Fatalf("admin %s %s = %d (%s), want 403", tc.method, tc.path, w.Code, w.Body.String())
		}
		if files.calls != before {
			t.Fatalf("admin %s %s reached file executor", tc.method, tc.path)
		}
	}

	api.External = staticExternal{p: owner}
	w := do(api.ExternalHandler(), "PUT", "/api/v1/servers/login/file?path=plugins/ok.txt", `{"content":"aGk=","content_sha256":"`+hiSum+`"}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("owner PUT login plugin file = %d (%s), want 200", w.Code, w.Body.String())
	}
	w = do(api.ExternalHandler(), "PATCH", "/api/v1/servers/login", `{"java_flags":["-XX:+UseG1GC"]}`, jsonHeader)
	if w.Code != http.StatusOK {
		t.Fatalf("owner PATCH login java_flags = %d (%s), want 200", w.Code, w.Body.String())
	}
}
