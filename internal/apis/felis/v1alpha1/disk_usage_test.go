package v1alpha1

import (
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func diskSampleServer(t *testing.T, used int64, at time.Time) *MinecraftServer {
	t.Helper()
	server := &MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "survival", UID: "server-1"}, Spec: MinecraftServerSpec{Storage: StorageSpec{Size: "100"}}}
	sample := DiskUsageSample{ServerUID: server.UID, PVCName: server.WorldPVC(), PVCUID: "claim-1", UsedBytes: used, SampledAt: at}
	raw, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	server.Annotations = map[string]string{AnnotationDiskUsage: string(raw)}
	return server
}

func TestDiskQuotaThresholdsAndFreshness(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		used int64
		age  time.Duration
		want string
	}{
		{0, 0, DiskHealthy}, {89, 0, DiskHealthy}, {90, 0, DiskLow}, {99, 0, DiskLow},
		{100, 0, StopDiskFull}, {101, 0, StopDiskFull}, {100, 180 * time.Second, StopDiskFull},
		{100, 181 * time.Second, StopDiskFull}, {100, -time.Second, ""},
	} {
		server := diskSampleServer(t, tc.used, now.Add(-tc.age))
		if got := DiskQuotaReason(server, "claim-1", now); got != tc.want {
			t.Errorf("used=%d age=%s: %q, want %q", tc.used, tc.age, got, tc.want)
		}
	}
	server := diskSampleServer(t, 90, now)
	server.Spec.Storage.Size = "101"
	if got := DiskQuotaReason(server, "claim-1", now); got != DiskHealthy {
		t.Fatal("the 90% threshold rounded down and warned early")
	}
}

func TestDiskFullReleasesOnlyOnFreshCurrentClaimSample(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, mode := range []string{"missing", "damaged", "missing used", "null used", "stale low", "future low", "wrong claim UID", "wrong server UID", "changed claim name", "fresh low"} {
		t.Run(mode, func(t *testing.T) {
			server := diskSampleServer(t, 89, now)
			server.Status.Conditions = []metav1.Condition{{Type: ConditionDiskSpaceAvailable, Status: metav1.ConditionFalse, Reason: StopDiskFull}}
			uid := types.UID("claim-1")
			switch mode {
			case "missing":
				delete(server.Annotations, AnnotationDiskUsage)
			case "damaged":
				server.Annotations[AnnotationDiskUsage] = "{broken"
			case "missing used", "null used":
				var wire map[string]any
				if err := json.Unmarshal([]byte(server.Annotations[AnnotationDiskUsage]), &wire); err != nil {
					t.Fatal(err)
				}
				if mode == "missing used" {
					delete(wire, "usedBytes")
				} else {
					wire["usedBytes"] = nil
				}
				raw, err := json.Marshal(wire)
				if err != nil {
					t.Fatal(err)
				}
				server.Annotations[AnnotationDiskUsage] = string(raw)
			case "stale low":
				server = diskSampleServer(t, 89, now.Add(-4*time.Minute))
				server.Status.Conditions = []metav1.Condition{{Type: ConditionDiskSpaceAvailable, Status: metav1.ConditionFalse, Reason: StopDiskFull}}
			case "future low":
				server = diskSampleServer(t, 89, now.Add(time.Second))
				server.Status.Conditions = []metav1.Condition{{Type: ConditionDiskSpaceAvailable, Status: metav1.ConditionFalse, Reason: StopDiskFull}}
			case "wrong claim UID":
				uid = "replacement-claim"
			case "wrong server UID":
				server.UID = "replacement-server"
			case "changed claim name":
				server.Spec.Storage.ClaimName = "migrated-world"
			}
			want := StopDiskFull
			if mode == "fresh low" {
				want = DiskHealthy
			}
			if got := DiskQuotaReason(server, uid, now); got != want {
				t.Fatalf("reason=%q want=%q", got, want)
			}
		})
	}
}

func TestDiskUsageRejectsInvalidSamplesAndLimits(t *testing.T) {
	for _, raw := range []string{"", "null", "{}", `{"usedBytes":-1}`, "{broken"} {
		if _, ok := ParseDiskUsage(raw); ok {
			t.Fatalf("invalid sample %q was accepted", raw)
		}
	}
	server := &MinecraftServer{}
	if DiskQuotaBytes(server) != 8<<30 {
		t.Fatal("default quota differs from the world PVC default")
	}
	for _, size := range []string{"0", "-1Gi", "garbage", "9223372036854775808"} {
		server.Spec.Storage.Size = size
		if DiskQuotaBytes(server) != 0 {
			t.Fatalf("invalid quota %q was accepted", size)
		}
	}
}
