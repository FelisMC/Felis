package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/placement"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func diskQuotaCluster(t *testing.T) (*K8sCluster, client.Client, *v1alpha1.MinecraftServer, *corev1.PersistentVolumeClaim) {
	t.Helper()
	server := stoppedServer()
	server.UID = "disk-server"
	server.Spec.Storage.Size = "100"
	server.Annotations = map[string]string{"unrelated": "kept"}
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: server.WorldPVC(), Namespace: server.Namespace, UID: "disk-claim"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	k, c := lockCluster(t, server, claim)
	return k, c, server, claim
}

func setAPIDiskSample(t *testing.T, server *v1alpha1.MinecraftServer, claim *corev1.PersistentVolumeClaim, used int64, at time.Time) {
	t.Helper()
	raw, err := json.Marshal(v1alpha1.DiskUsageSample{ServerUID: server.UID, PVCName: claim.Name, PVCUID: claim.UID, UsedBytes: used, SampledAt: at})
	if err != nil {
		t.Fatal(err)
	}
	if server.Annotations == nil {
		server.Annotations = map[string]string{}
	}
	server.Annotations[v1alpha1.AnnotationDiskUsage] = string(raw)
}

func TestDiskUsagePublicationPreservesSpecStatusAndMonotonicTime(t *testing.T) {
	k, c, original, _ := diskQuotaCluster(t)
	ctx := context.Background()
	target, err := k.DiskUsageTarget(ctx, original.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.PublishDiskUsage(ctx, target, 0, lockNow.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	var server v1alpha1.MinecraftServer
	if err := c.Get(ctx, client.ObjectKeyFromObject(original), &server); err != nil {
		t.Fatal(err)
	}
	sample, ok := v1alpha1.ServerDiskSample(&server)
	if !ok || sample.UsedBytes != 0 || !sample.SampledAt.Equal(lockNow.Add(-time.Minute)) ||
		!reflect.DeepEqual(server.Spec, original.Spec) || !reflect.DeepEqual(server.Status, original.Status) || server.Annotations["unrelated"] != "kept" {
		t.Fatal("successful zero sample was lost or changed spec/status/unrelated annotations")
	}
	if err := k.PublishDiskUsage(ctx, target, 90, lockNow); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(original), &server); err != nil {
		t.Fatal(err)
	}
	last := server.Annotations[v1alpha1.AnnotationDiskUsage]
	for _, tc := range []struct {
		used int64
		at   time.Time
	}{
		{1, lockNow.Add(-time.Second)}, {1, lockNow}, {-1, lockNow}, {1, time.Time{}}, {1, lockNow.Add(time.Second)},
	} {
		if err := k.PublishDiskUsage(ctx, target, tc.used, tc.at); err == nil {
			t.Fatal("an invalid or regressing sample was published")
		}
		if err := c.Get(ctx, client.ObjectKeyFromObject(original), &server); err != nil {
			t.Fatal(err)
		}
		if server.Annotations[v1alpha1.AnnotationDiskUsage] != last {
			t.Fatal("failed publication replaced the last successful sample")
		}
	}
}

func TestDiskUsagePublicationRetriesWithoutOverwritingOperatorStatus(t *testing.T) {
	k, c, original, _ := diskQuotaCluster(t)
	ctx := context.Background()
	target, err := k.DiskUsageTarget(ctx, original.Name)
	if err != nil {
		t.Fatal(err)
	}
	patches := 0
	k.c = interceptor.NewClient(c.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			patches++
			if patches == 1 {
				var current v1alpha1.MinecraftServer
				if err := cl.Get(ctx, client.ObjectKeyFromObject(original), &current); err != nil {
					return err
				}
				current.Status.Players.Online = 7
				if err := cl.Status().Update(ctx, &current); err != nil {
					return err
				}
			}
			return cl.Patch(ctx, obj, patch, opts...)
		},
	})
	if err := k.PublishDiskUsage(ctx, target, 75, lockNow); err != nil {
		t.Fatal(err)
	}
	var got v1alpha1.MinecraftServer
	if err := c.Get(ctx, client.ObjectKeyFromObject(original), &got); err != nil {
		t.Fatal(err)
	}
	if patches < 2 || got.Status.Players.Online != 7 || got.Annotations[v1alpha1.AnnotationDiskUsage] == "" {
		t.Fatal("publication did not retry the checked resourceVersion or overwrote operator status")
	}
}

func TestDiskUsagePublicationRecoversFromForeignOrFutureAnnotations(t *testing.T) {
	for _, mode := range []string{"foreign server", "foreign claim name", "foreign claim UID", "future"} {
		t.Run(mode, func(t *testing.T) {
			k, c, server, claim := diskQuotaCluster(t)
			ctx := context.Background()
			if err := c.Get(ctx, client.ObjectKeyFromObject(server), server); err != nil {
				t.Fatal(err)
			}
			old := v1alpha1.DiskUsageSample{ServerUID: server.UID, PVCName: claim.Name, PVCUID: claim.UID, UsedBytes: 100, SampledAt: lockNow.Add(time.Hour)}
			switch mode {
			case "foreign server":
				old.ServerUID = "old-server"
				old.SampledAt = lockNow
			case "foreign claim name":
				old.PVCName = "old-world"
				old.SampledAt = lockNow
			case "foreign claim UID":
				old.PVCUID = "old-claim"
				old.SampledAt = lockNow
			}
			raw, err := json.Marshal(old)
			if err != nil {
				t.Fatal(err)
			}
			server.Annotations[v1alpha1.AnnotationDiskUsage] = string(raw)
			if err := c.Update(ctx, server); err != nil {
				t.Fatal(err)
			}
			server.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionDiskSpaceAvailable, Status: metav1.ConditionFalse, Reason: v1alpha1.StopDiskFull}}
			if err := c.Status().Update(ctx, server); err != nil {
				t.Fatal(err)
			}
			target, err := k.DiskUsageTarget(ctx, server.Name)
			if err != nil {
				t.Fatal(err)
			}
			if err := k.PublishDiskUsage(ctx, target, 10, lockNow); err != nil {
				t.Fatalf("unusable annotation prevented recovery: %v", err)
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(server), server); err != nil {
				t.Fatal(err)
			}
			sample, ok := v1alpha1.ServerDiskSample(server)
			if !ok || sample.PVCUID != claim.UID || sample.UsedBytes != 10 || !sample.SampledAt.Equal(lockNow) {
				t.Fatal("current successful sample did not replace unusable history")
			}
			if err := k.checkDiskQuota(ctx, server); err != nil {
				t.Fatal("a fresh current-claim sample could not release the start gate")
			}
			if _, blocked := serverDiskUsage(server, lockNow); !blocked {
				t.Fatal("publishing a sample wrote or bypassed the operator-owned full condition")
			}
		})
	}
}

func TestDiskUsagePublicationRejectsChangedTargetsAndMaintenance(t *testing.T) {
	for _, mode := range []string{"claim replacement", "server replacement", "node change", "maintenance"} {
		t.Run(mode, func(t *testing.T) {
			k, c, server, claim := diskQuotaCluster(t)
			ctx := context.Background()
			node := "node-a"
			k.DiskWorldResolver = func(context.Context, string) (placement.World, error) {
				return placement.World{Claim: claim.Name, Node: node}, nil
			}
			target, err := k.DiskUsageTarget(ctx, server.Name)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "claim replacement":
				if err := c.Delete(ctx, claim); err != nil {
					t.Fatal(err)
				}
				claim.ResourceVersion, claim.UID = "", "replacement-claim"
				if err := c.Create(ctx, claim); err != nil {
					t.Fatal(err)
				}
			case "server replacement":
				if err := c.Delete(ctx, server); err != nil {
					t.Fatal(err)
				}
				server.ResourceVersion, server.UID = "", "replacement-server"
				if err := c.Create(ctx, server); err != nil {
					t.Fatal(err)
				}
			case "node change":
				node = "node-b"
			case "maintenance":
				if err := c.Get(ctx, client.ObjectKeyFromObject(server), server); err != nil {
					t.Fatal(err)
				}
				server.Annotations[maintenance.Annotation] = maintenance.LockValue(maintenance.KindMigration, lockNow)
				if err := c.Update(ctx, server); err != nil {
					t.Fatal(err)
				}
			}
			if err := k.PublishDiskUsage(ctx, target, 1, lockNow); err == nil {
				t.Fatal("a changed or busy target accepted the old scan")
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(server), server); err != nil {
				t.Fatal(err)
			}
			if server.Annotations[v1alpha1.AnnotationDiskUsage] != "" {
				t.Fatal("a rejected sample was persisted")
			}
		})
	}
}

func TestDiskUsageTargetsRequireExistingBoundClaims(t *testing.T) {
	for _, mode := range []string{"pending", "missing", "resolver mismatch", "maintenance"} {
		t.Run(mode, func(t *testing.T) {
			k, c, server, claim := diskQuotaCluster(t)
			ctx := context.Background()
			switch mode {
			case "pending":
				if err := c.Get(ctx, client.ObjectKeyFromObject(claim), claim); err != nil {
					t.Fatal(err)
				}
				claim.Status.Phase = corev1.ClaimPending
				if err := c.Status().Update(ctx, claim); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := c.Delete(ctx, claim); err != nil {
					t.Fatal(err)
				}
			case "resolver mismatch":
				k.DiskWorldResolver = func(context.Context, string) (placement.World, error) {
					return placement.World{Claim: "wrong-world"}, nil
				}
			case "maintenance":
				if err := c.Get(ctx, client.ObjectKeyFromObject(server), server); err != nil {
					t.Fatal(err)
				}
				server.Annotations[maintenance.Annotation] = maintenance.LockValue(maintenance.KindRestore, lockNow)
				if err := c.Update(ctx, server); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := k.DiskUsageTarget(ctx, server.Name); err == nil {
				t.Fatal("an unavailable world was admitted to scanning")
			}
			var claims corev1.PersistentVolumeClaimList
			if err := c.List(ctx, &claims); err != nil {
				t.Fatal(err)
			}
			if mode == "missing" && len(claims.Items) != 0 {
				t.Fatal("disk sampling created a missing claim")
			}
		})
	}
}

func TestDiskQuotaBlocksAllStartPathsBeforePendingRequestCoalescing(t *testing.T) {
	for _, action := range []string{"wake", "retry", "restart"} {
		for _, sampleState := range []string{"fresh full", "fresh full no latch", "stale full", "stale full no latch", "damaged", "old claim low", "fresh low"} {
			t.Run(action+"/"+sampleState, func(t *testing.T) {
				k, c, server, claim := diskQuotaCluster(t)
				used, at := int64(100), lockNow
				if strings.HasPrefix(sampleState, "stale full") {
					at = lockNow.Add(-4 * time.Minute)
				}
				if sampleState == "old claim low" || sampleState == "fresh low" {
					used = 10
				}
				if sampleState == "old claim low" {
					if err := c.Delete(context.Background(), claim); err != nil {
						t.Fatal(err)
					}
					claim.ResourceVersion, claim.UID = "", "new-claim"
					if err := c.Create(context.Background(), claim); err != nil {
						t.Fatal(err)
					}
				}
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(server), server); err != nil {
					t.Fatal(err)
				}
				server.Spec.DesiredState = v1alpha1.DesiredRunning
				server.Annotations[v1alpha1.AnnotationRestart] = `{"request":"pending","pod_uid":"old-pod"}`
				setAPIDiskSample(t, server, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: server.WorldPVC(), UID: "disk-claim"}}, used, at)
				if sampleState == "damaged" {
					server.Annotations[v1alpha1.AnnotationDiskUsage] = "broken"
				}
				if err := c.Update(context.Background(), server); err != nil {
					t.Fatal(err)
				}
				server.Status.Phase = v1alpha1.PhaseRunning
				server.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionDiskSpaceAvailable, Status: metav1.ConditionFalse, Reason: v1alpha1.StopDiskFull}}
				if strings.HasSuffix(sampleState, "no latch") {
					server.Status.Conditions = nil
				}
				if err := c.Status().Update(context.Background(), server); err != nil {
					t.Fatal(err)
				}
				var err error
				switch action {
				case "wake":
					err = k.SetDesiredState(context.Background(), server.Name, v1alpha1.DesiredRunning)
				case "retry":
					err = k.RetryStart(context.Background(), server.Name)
				case "restart":
					err = k.RestartServer(context.Background(), server.Name)
				}
				if sampleState == "fresh low" {
					if err != nil {
						t.Fatalf("fresh current claim remained blocked: %v", err)
					}
				} else {
					var ae *apiError
					if !errors.As(err, &ae) || ae.status != http.StatusConflict || ae.code != "disk_full" {
						t.Fatalf("gate error=%v", err)
					}
				}
				if err := k.SetDesiredState(context.Background(), server.Name, v1alpha1.DesiredStopped); err != nil {
					t.Fatal("disk quota blocked stopping")
				}
				if err := k.CheckFileAccess(context.Background(), server.Name); err != nil {
					t.Fatal("disk quota blocked file cleanup")
				}
			})
		}
	}
}

func TestDiskProjectionRetainsFullLatchAndMarksUnconfirmedLowStale(t *testing.T) {
	_, _, server, claim := diskQuotaCluster(t)
	server.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionDiskSpaceAvailable, Status: metav1.ConditionFalse, Reason: v1alpha1.StopDiskFull}}
	setAPIDiskSample(t, server, claim, 10, lockNow)
	view := serverInfo(server, lockNow)
	if !view.DiskFull || view.Disk == nil || view.Disk.UsedBytes != 10 || !view.Disk.Stale {
		t.Fatal("an unverified low sample released the full-disk projection")
	}
	public := publicServerInfo(view)
	if !public.DiskFull || public.Disk != nil {
		t.Fatal("public status leaked disk usage or hid the startup block")
	}
	delete(server.Annotations, v1alpha1.AnnotationDiskUsage)
	if view := serverInfo(server, lockNow); !view.DiskFull || view.Disk != nil {
		t.Fatal("unknown usage became zero or released the full-disk projection")
	}
	server.Status.Conditions = nil
	setAPIDiskSample(t, server, claim, 100, lockNow)
	if view := serverInfo(server, lockNow); !view.DiskFull || view.Disk == nil || view.Disk.Stale {
		t.Fatal("fresh full usage did not reach the projection before operator reconciliation")
	}
	if view := serverInfo(server, lockNow.Add(4*time.Minute)); view.Disk == nil || !view.Disk.Stale || !view.DiskFull {
		t.Fatal("an old full sample became unknown or lost its startup block")
	}
}

func TestDiskQuotaStatusAndListsExposeUsageOnlyToManagers(t *testing.T) {
	a, repo, cl, _ := newPatchAPI()
	repo.byName["survival"].OwnerID = "owner1"
	cl.byName["survival"].Disk = &ServerDiskUsage{UsedBytes: 100, LimitBytes: 100, SampledAt: lockNow, Stale: true}
	cl.byName["survival"].DiskFull = true
	cl.list = []ServerInfo{*cl.byName["survival"]}
	for _, owned := range []bool{false, true} {
		id := "other"
		if owned {
			id = "owner1"
		}
		a.External = staticExternal{p: &Principal{UserID: id, Role: "user"}}
		repo.mine[id] = []MyServerView{{Name: "survival", Owned: owned}}
		for _, path := range []string{"/api/v1/servers/survival/status", "/api/v1/me/servers"} {
			w := do(a.ExternalHandler(), "GET", path, "", nil)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"diskFull":true`) || strings.Contains(w.Body.String(), `"usedBytes"`) != owned {
				t.Fatalf("manager=%v path=%s status=%d body=%s", owned, path, w.Code, w.Body.String())
			}
		}
	}
}
