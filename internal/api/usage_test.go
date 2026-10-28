package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestUsageHistory(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	point := UsagePoint{At: at}
	var failure error
	u := NewUsageSampler(func(context.Context) (map[string]UsagePoint, error) {
		return map[string]UsagePoint{"survival-0": point, "files-survival-0": {CPUMilli: 9999}}, failure
	})
	servers := []ServerInfo{{Name: "survival", Phase: "Running", DesiredState: "Running"}}
	for i := 0; i < 245; i++ {
		point = UsagePoint{At: at.Add(time.Duration(i) * 15 * time.Second), CPUMilli: int64(i), MemoryBytes: 64 << 20}
		if err := u.Collect(ctx, servers, point.At); err != nil {
			t.Fatal(err)
		}
	}
	view := u.Snapshot("survival", point.At)
	if len(view.History) != 240 || view.History[0].CPUMilli != 5 || view.History[239].CPUMilli != 244 || view.Current.CPUMilli != 244 {
		t.Fatalf("bounded chronological history: len=%d first=%v last=%v", len(view.History), view.History[0], view.Current)
	}
	view.History[0].CPUMilli = -1
	if err := u.Collect(ctx, servers, point.At); err != nil {
		t.Fatal(err)
	}
	if got := u.Snapshot("survival", point.At); len(got.History) != 240 || got.History[0].CPUMilli != 5 {
		t.Fatal("duplicate sample or caller mutation changed history")
	}
	if u.Snapshot("survival", point.At.Add(46*time.Second)).Current != nil {
		t.Fatal("stale usage reported as current")
	}
	if got := u.Snapshot("survival", point.At.Add(time.Hour+time.Second)); got.Current != nil || len(got.History) != 0 {
		t.Fatal("history outlived one hour")
	}
	failure = errors.New("metrics unavailable")
	if err := u.Collect(ctx, servers, point.At); !errors.Is(err, failure) {
		t.Fatal("metrics failure was hidden", err)
	}
	if got := u.Snapshot("survival", point.At); got.Current != nil || len(got.History) != 240 {
		t.Fatal("failure must hide current and retain history")
	}
	failure = nil
	servers[0].Phase = "Stopped"
	if err := u.Collect(ctx, servers, point.At); err != nil {
		t.Fatal(err)
	}
	if got := u.Snapshot("survival", point.At); got.Current != nil || len(got.History) != 240 {
		t.Fatal("stopped server lost history or retained current")
	}
	if err := u.Collect(ctx, nil, point.At); err != nil {
		t.Fatal(err)
	}
	if len(u.Snapshot("survival", point.At).History) != 0 {
		t.Fatal("deleted server retained its history")
	}
}

func TestUsageSamplerConcurrentReaders(t *testing.T) {
	point := UsagePoint{At: time.Now(), CPUMilli: 0, MemoryBytes: 1}
	u := NewUsageSampler(func(context.Context) (map[string]UsagePoint, error) {
		return map[string]UsagePoint{"survival-0": point}, nil
	})
	var group sync.WaitGroup
	for range 10 {
		group.Go(func() {
			for range 20 {
				if err := u.Collect(context.Background(), []ServerInfo{{Name: "survival", Phase: "Running", DesiredState: "Running"}}, point.At); err != nil {
					t.Error(err)
				}
				u.Snapshot("survival", point.At)
			}
		})
	}
	group.Wait()
	if got := u.Snapshot("survival", point.At); got.Current == nil || got.Current.CPUMilli != 0 || len(got.History) != 1 {
		t.Fatal("idle usage or concurrent sampling was lost")
	}
}

func TestReadNamespacedPodUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/apis/metrics.k8s.io/v1beta1/namespaces/minecraft/pods" {
			t.Errorf("unexpected metrics request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"metrics.k8s.io/v1beta1","kind":"PodMetricsList","items":[{"metadata":{"name":"survival-0","namespace":"minecraft"},"timestamp":"2026-10-23T00:00:00Z","window":"15s","containers":[{"name":"minecraft","usage":{"cpu":"25000000n","memory":"32Mi"}},{"name":"sidecar","usage":{"cpu":"75m","memory":"64Mi"}}]}]}`))
	}))
	defer server.Close()
	gv := schema.GroupVersion{Group: "metrics.k8s.io", Version: "v1beta1"}
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{gv})
	mapper.AddSpecific(gv.WithKind("PodMetrics"), gv.WithResource("pods"), gv.WithResource("pod"), meta.RESTScopeNamespace)
	cl, err := client.New(&rest.Config{Host: server.URL}, client.Options{Scheme: runtime.NewScheme(), Mapper: mapper})
	if err != nil {
		t.Fatal(err)
	}
	points, err := NewK8sCluster(cl, "minecraft").ReadPodUsage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	point := points["survival-0"]
	if len(points) != 1 || point.CPUMilli != 100 || point.MemoryBytes != 96*1024*1024 || point.At.Format(time.RFC3339) != "2026-10-23T00:00:00Z" {
		t.Fatalf("resource quantities: %+v", points)
	}
}

func TestUsagePermissionsAndStatusPrivacy(t *testing.T) {
	a, repo, cl, _ := mkFiles(t)
	now := time.Now().UTC()
	a.Now = func() time.Time { return now }
	cl.byName["survival"].Phase, cl.byName["survival"].DesiredState = "Running", "Running"
	a.Usage = NewUsageSampler(func(context.Context) (map[string]UsagePoint, error) {
		return map[string]UsagePoint{"survival-0": {At: now, CPUMilli: 250, MemoryBytes: 1 << 30}}, nil
	})
	if err := a.Usage.Collect(context.Background(), []ServerInfo{*cl.byName["survival"]}, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		label     string
		principal *Principal
		allowed   bool
	}{
		{"owner", fileOwner, true}, {"admin", fileAdmin, true}, {"stranger", fileStranger, false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			a.External = staticExternal{p: tc.principal}
			w := do(a.ExternalHandler(), "GET", "/api/v1/servers/survival/usage", "", nil)
			if !tc.allowed {
				if w.Code != http.StatusForbidden {
					t.Fatalf("usage permission: %d %s", w.Code, w.Body.String())
				}
			} else {
				var view ServerUsage
				if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
					t.Fatal(err)
				}
				if w.Code != 200 || view.Current == nil || view.Current.CPUMilli != 250 || len(view.History) != 1 {
					t.Fatalf("usage: %d %s", w.Code, w.Body.String())
				}
			}
			w = do(a.ExternalHandler(), "GET", "/api/v1/servers/survival/status", "", nil)
			var status map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
				t.Fatal(err)
			}
			_, visible := status["cpuMilli"]
			if visible != tc.allowed {
				t.Fatalf("status privacy: %s", w.Body.String())
			}
		})
	}
	a.External = staticExternal{p: fileOwner}
	repo.mine[fileOwner.UserID] = []MyServerView{{Name: "survival", Owned: true}, {Name: "public", Owned: false}}
	cl.byName["public"] = &ServerInfo{Name: "public", Phase: "Running", DesiredState: "Running", CPU: "2", Memory: "2Gi"}
	cl.byName["survival"].CPU, cl.byName["survival"].Memory = "2", "2Gi"
	cl.list = []ServerInfo{*cl.byName["survival"], *cl.byName["public"]}
	a.Usage.read = func(context.Context) (map[string]UsagePoint, error) {
		return map[string]UsagePoint{"survival-0": {At: now, CPUMilli: 250}, "public-0": {At: now, CPUMilli: 500}}, nil
	}
	if err := a.Usage.Collect(context.Background(), []ServerInfo{*cl.byName["survival"], *cl.byName["public"]}, now); err != nil {
		t.Fatal(err)
	}
	listed := do(a.ExternalHandler(), "GET", "/api/v1/me/servers", "", nil)
	var mine struct {
		Servers []MyServerView `json:"servers"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &mine); err != nil {
		t.Fatal(err)
	}
	if len(mine.Servers) != 2 || mine.Servers[0].CPUMilli == nil || mine.Servers[0].CPU != "2" || mine.Servers[1].CPUMilli != nil || mine.Servers[1].CPU != "" || mine.Servers[1].Memory != "" {
		t.Fatalf("list metrics privacy: %s", listed.Body.String())
	}
	cl.byName["survival"].DesiredState = "Stopped"
	w := do(a.ExternalHandler(), "GET", "/api/v1/servers/survival/usage", "", nil)
	var stopped ServerUsage
	if err := json.Unmarshal(w.Body.Bytes(), &stopped); err != nil {
		t.Fatal(err)
	}
	if stopped.Current != nil || len(stopped.History) != 1 {
		t.Fatal("stopping server still reports current usage or lost history")
	}
}

func TestPlatformUsageSumsRunningServersPerNode(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	u := NewUsageSampler(func(context.Context) (map[string]UsagePoint, error) {
		return map[string]UsagePoint{
			"a-0":       {At: now, CPUMilli: 250, MemoryBytes: 1 << 30},
			"b-0":       {At: now, CPUMilli: 500, MemoryBytes: 2 << 30},
			"stopped-0": {At: now, CPUMilli: 9999, MemoryBytes: 1 << 40},
		}, nil
	})
	servers := []ServerInfo{
		{Name: "b", Phase: "Running", DesiredState: "Running", NodeName: "worker-b"},
		{Name: "a", Phase: "Running", DesiredState: "Running", NodeName: "worker-b"},
		{Name: "starting", Phase: "Running", DesiredState: "Running", NodeName: "worker-a"},
		{Name: "stopped", Phase: "Stopped", DesiredState: "Stopped", NodeName: "worker-c"},
	}
	if err := u.Collect(context.Background(), servers, now); err != nil {
		t.Fatal(err)
	}
	view := u.PlatformSnapshot(now)
	if view.Current == nil || view.Current.Running != 3 || view.Current.CPUMilli != 750 || view.Current.MemoryBytes != 3<<30 || !view.Current.At.Equal(now) {
		t.Fatalf("platform point = %+v, want 3 running servers with the two measured ones summed", view.Current)
	}
	want := []NodeUsage{
		{Name: "worker-a", Running: 1},
		{Name: "worker-b", CPUMilli: 750, MemoryBytes: 3 << 30, Running: 2},
	}
	if !slices.Equal(view.Nodes, want) {
		t.Fatalf("nodes = %+v, want %+v", view.Nodes, want)
	}
}

func TestPlatformUsageCountsServersWithoutANodeOnlyInTheTotal(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	u := NewUsageSampler(func(context.Context) (map[string]UsagePoint, error) {
		return map[string]UsagePoint{"loose-0": {At: now, CPUMilli: 100, MemoryBytes: 1 << 20}}, nil
	})
	if err := u.Collect(context.Background(), []ServerInfo{{Name: "loose", Phase: "Running", DesiredState: "Running"}}, now); err != nil {
		t.Fatal(err)
	}
	view := u.PlatformSnapshot(now)
	if view.Current == nil || view.Current.Running != 1 || view.Current.CPUMilli != 100 || len(view.Nodes) != 0 {
		t.Fatalf("view = %+v current %+v", view, view.Current)
	}
}

func TestPlatformUsageHistoryAndFreshness(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	var failure error
	u := NewUsageSampler(func(context.Context) (map[string]UsagePoint, error) {
		return map[string]UsagePoint{"a-0": {CPUMilli: 100, MemoryBytes: 1 << 20}}, failure
	})
	servers := []ServerInfo{{Name: "a", Phase: "Running", DesiredState: "Running", NodeName: "worker-a"}}
	for i := range 3 {
		if err := u.Collect(ctx, servers, at.Add(time.Duration(i)*UsageInterval)); err != nil {
			t.Fatal(err)
		}
	}
	failure = errors.New("metrics unavailable")
	if err := u.Collect(ctx, servers, at.Add(3*UsageInterval)); !errors.Is(err, failure) {
		t.Fatal("metrics failure was hidden", err)
	}
	if got := u.PlatformSnapshot(at.Add(3 * UsageInterval)); len(got.History) != 3 {
		t.Fatalf("a failed read left %d points, want the 3 before it", len(got.History))
	}

	stale := u.PlatformSnapshot(at.Add(5*UsageInterval + time.Second))
	if stale.Current != nil || stale.Nodes == nil || len(stale.Nodes) != 0 || len(stale.History) != 3 {
		t.Fatalf("stale view = current %+v nodes %v history %d, want no current, empty nodes, 3 points", stale.Current, stale.Nodes, len(stale.History))
	}

	failure = nil
	servers[0].Phase = "Stopped"
	last := at.Add(6 * UsageInterval)
	if err := u.Collect(ctx, servers, last); err != nil {
		t.Fatal(err)
	}
	got := u.PlatformSnapshot(last)
	if len(got.History) != 4 || got.Current == nil || got.Current.Running != 0 || len(got.Nodes) != 0 {
		t.Fatalf("view with nothing running = current %+v nodes %v history %d, want a zero point and no nodes", got.Current, got.Nodes, len(got.History))
	}
	for i := 1; i < len(got.History); i++ {
		if !got.History[i].At.After(got.History[i-1].At) {
			t.Fatal("history is not in time order")
		}
	}
	if later := u.PlatformSnapshot(last.Add(time.Hour)); len(later.History) != 1 {
		t.Fatalf("history kept %d points beyond an hour, want only the newest", len(later.History))
	}
}
