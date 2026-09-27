package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/platform"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const bgControlNS = "felis-system"

func internalAPIObjs(clusterIP, token string) []client.Object {
	return []client.Object{
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: platform.APIInternalServiceName, Namespace: bgControlNS},
			Spec:       corev1.ServiceSpec{ClusterIP: clusterIP},
		},
		// The console presents the ops token; the proxy's felis-service-token sits
		// beside it and must not be the one picked up.
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "felis-ops-token", Namespace: bgControlNS},
			Data:       map[string][]byte{"token": []byte(token)},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "felis-service-token", Namespace: bgControlNS},
			Data:       map[string][]byte{"token": []byte("proxy-" + token)},
		},
	}
}

func fakeInternalAPIClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(haltScheme(t)).WithObjects(objs...).Build()
}

func TestResolveInternalAPI(t *testing.T) {
	t.Run("happy: ClusterIP + token -> baseURL, token", func(t *testing.T) {
		cl := fakeInternalAPIClient(t, internalAPIObjs("10.43.0.9", "s3cr3t")...)
		base, tok, err := resolveInternalAPI(context.Background(), cl, bgControlNS)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tok != "s3cr3t" {
			t.Fatalf("token = %q, want s3cr3t", tok)
		}
		// The URL must carry the resolved ClusterIP + internal port — not the cluster-DNS
		// name, which the on-node host cannot resolve.
		want := fmt.Sprintf("http://10.43.0.9:%d", platform.APIInternalPort)
		if base != want {
			t.Fatalf("baseURL = %q, want %q", base, want)
		}
	})

	t.Run("headless Service (no ClusterIP) -> error", func(t *testing.T) {
		cl := fakeInternalAPIClient(t, internalAPIObjs(corev1.ClusterIPNone, "s3cr3t")...)
		if _, _, err := resolveInternalAPI(context.Background(), cl, bgControlNS); err == nil {
			t.Fatal("want error for a Service with no ClusterIP")
		}
	})

	t.Run("empty token -> error", func(t *testing.T) {
		cl := fakeInternalAPIClient(t, internalAPIObjs("10.43.0.9", "")...)
		if _, _, err := resolveInternalAPI(context.Background(), cl, bgControlNS); err == nil {
			t.Fatal("want error for a Secret with no token")
		}
	})
}

func TestRequestBackup(t *testing.T) {
	hc := &http.Client{Timeout: 2 * time.Second}

	t.Run("202 -> backing_up, and the request carries Bearer + os_user", func(t *testing.T) {
		var gotAuth, gotOSUser, gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotPath = r.URL.Path
			var body struct {
				OSUser string `json:"os_user"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			gotOSUser = body.OSUser
			w.WriteHeader(http.StatusAccepted)
		}))
		defer srv.Close()

		out, err := requestBackup(context.Background(), hc, srv.URL, "tok123", "survival", "alice")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if out.name != "survival" || out.status != "backing_up" {
			t.Fatalf("outcome = %+v, want {survival backing_up}", out)
		}
		if gotAuth != "Bearer tok123" {
			t.Fatalf("Authorization = %q, want Bearer tok123", gotAuth)
		}
		if gotOSUser != "alice" {
			t.Fatalf("os_user in body = %q, want alice", gotOSUser)
		}
		if gotPath != "/api/v1/internal/servers/survival/backup" {
			t.Fatalf("path = %q, want the internal backup path", gotPath)
		}
	})

	// The status-code → friendly-message mapping is the peer's real logic; assert each
	// well-known code produces a distinct operator-facing message.
	cases := []struct {
		name   string
		code   int
		body   string // optional JSON error body
		expect string
	}{
		{"409 not_stopped", http.StatusConflict, "", "must be stopped"},
		{"409 no_world_volume surfaces the API's own text", http.StatusConflict,
			`{"error":{"code":"no_world_volume","message":"this server has no world volume yet — start it once to create it, then retry"}}`,
			"no world volume yet"},
		{"503 backup_unavailable", http.StatusServiceUnavailable, "", "not configured"},
		{"404 not found", http.StatusNotFound, "", "no such server"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				if tc.body != "" {
					_, _ = io.WriteString(w, tc.body)
				}
			}))
			defer srv.Close()
			_, err := requestBackup(context.Background(), hc, srv.URL, "tok", "survival", "alice")
			if err == nil || !strings.Contains(err.Error(), tc.expect) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.expect)
			}
		})
	}

	t.Run("transport failure -> unreachable message (break-glass needs the API alive)", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		srv.Close() // dial will fail
		_, err := requestBackup(context.Background(), hc, srv.URL, "tok", "survival", "alice")
		if err == nil || !strings.Contains(err.Error(), "unreachable") {
			t.Fatalf("err = %v, want an 'unreachable' transport error", err)
		}
		// backup-now ends its run on this error alone.
		if !errors.Is(err, errBackupAPIUnreachable) {
			t.Fatalf("err = %v, want it to wrap errBackupAPIUnreachable", err)
		}
	})
}

// The Sync picker must not offer system servers: the backup API validates names
// and resolves a servers-table row, so a lobby/login pick can only die in
// validation — a dead choice in an emergency console.
func TestBackupPickable(t *testing.T) {
	got := backupPickable([]haltableServer{
		{name: "lobby", phase: "Running", system: true},
		{name: "login", phase: "Running", system: true},
		{name: "test-one", phase: "Stopped"},
	})
	if len(got) != 1 || got[0].name != "test-one" || got[0].system {
		t.Fatalf("backupPickable = %+v, want only the user server", got)
	}

	// Survivors keep their input order (the picker's cursor math depends on it).
	got = backupPickable([]haltableServer{
		{name: "alpha"}, {name: "login", system: true}, {name: "beta"},
	})
	if len(got) != 2 || got[0].name != "alpha" || got[1].name != "beta" {
		t.Fatalf("backupPickable order = %+v, want [alpha beta]", got)
	}
}
