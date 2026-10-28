package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestFleetUsageIsForStaffAndCountsLoginFailures(t *testing.T) {
	repo := newFakeRepo()
	repo.audits = []AuditEntry{{Action: "auth.passkey_login.failed"}, {Action: "auth.passkey_login.failed"}, {Action: "auth.logout"}}
	a := newTestAPI(repo, newFakeCluster())
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }

	a.External = staticExternal{p: fileOwner}
	if w := do(a.ExternalHandler(), "GET", "/api/v1/fleet/usage", "", nil); w.Code != http.StatusForbidden {
		t.Fatalf("user = %d %s", w.Code, w.Body.String())
	}

	a.External = staticExternal{p: fileAdmin}
	w := do(a.ExternalHandler(), "GET", "/api/v1/fleet/usage", "", nil)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil || w.Code != http.StatusOK {
		t.Fatalf("admin = %d %s", w.Code, w.Body.String())
	}
	if string(raw["current"]) != "null" || string(raw["history"]) != "[]" || string(raw["nodes"]) != "[]" || string(raw["loginFailures24h"]) != "2" {
		t.Fatalf("without a sampler: %s", w.Body.String())
	}

	a.Usage = NewUsageSampler(func(context.Context) (map[string]UsagePoint, error) {
		return map[string]UsagePoint{"survival-0": {At: now, CPUMilli: 250, MemoryBytes: 1 << 30}}, nil
	})
	servers := []ServerInfo{{Name: "survival", Phase: "Running", DesiredState: "Running", NodeName: "worker-a"}}
	if err := a.Usage.Collect(context.Background(), servers, now); err != nil {
		t.Fatal(err)
	}
	var view PlatformUsage
	w = do(a.ExternalHandler(), "GET", "/api/v1/fleet/usage", "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil || w.Code != http.StatusOK {
		t.Fatalf("admin = %d %s", w.Code, w.Body.String())
	}
	if view.Current == nil || view.Current.Running != 1 || view.Current.CPUMilli != 250 || len(view.History) != 1 ||
		len(view.Nodes) != 1 || view.Nodes[0].Name != "worker-a" || view.LoginFailures24h != 2 {
		t.Fatalf("with a sampler: %s", w.Body.String())
	}
}
