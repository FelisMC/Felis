package operator_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/operator"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func newDiskHost(t *testing.T) (*runHost, *corev1.PersistentVolumeClaim) {
	t.Helper()
	h := newRunHost(t, func(s *v1alpha1.MinecraftServer) {
		s.UID = "disk-server"
		s.Spec.Storage.Size = "100"
		s.Spec.Lifecycle.StopCommands = []string{"say quota stop"}
	})
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: getServer(t, h.c, "survival").WorldPVC(), Namespace: "minecraft", UID: "disk-claim"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	if err := h.c.Create(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	return h, claim
}

func recordDiskSample(t *testing.T, h *runHost, claim *corev1.PersistentVolumeClaim, used int64) {
	t.Helper()
	server := getServer(t, h.c, "survival")
	raw, err := json.Marshal(v1alpha1.DiskUsageSample{ServerUID: server.UID, PVCName: claim.Name, PVCUID: claim.UID, UsedBytes: used, SampledAt: h.clock})
	if err != nil {
		t.Fatal(err)
	}
	if server.Annotations == nil {
		server.Annotations = map[string]string{}
	}
	server.Annotations[v1alpha1.AnnotationDiskUsage] = string(raw)
	if err := h.c.Update(context.Background(), server); err != nil {
		t.Fatal(err)
	}
}

func countDiskEvents(server *v1alpha1.MinecraftServer, kind string) int {
	count := 0
	for _, ev := range server.Status.Events {
		if ev.Type == kind {
			count++
		}
	}
	return count
}

func TestDiskWarningOccursOncePerThresholdCrossing(t *testing.T) {
	h, claim := newDiskHost(t)
	for _, used := range []int64{90, 95, 99} {
		h.clock = h.clock.Add(time.Minute)
		recordDiskSample(t, h, claim, used)
		server := h.pass()
		if server.Spec.DesiredState != v1alpha1.DesiredRunning || countDiskEvents(server, v1alpha1.EventDiskWarning) != 1 {
			t.Fatal("a repeated low-space sample stopped the server or repeated its warning")
		}
	}
	h.clock = h.clock.Add(time.Minute)
	recordDiskSample(t, h, claim, 89)
	h.pass()
	h.clock = h.clock.Add(time.Minute)
	recordDiskSample(t, h, claim, 90)
	if countDiskEvents(h.pass(), v1alpha1.EventDiskWarning) != 2 {
		t.Fatal("a new threshold crossing did not warn again")
	}
}

func TestDiskFullUsesNormalStopAndSupersedesPendingRestart(t *testing.T) {
	h, claim := newDiskHost(t)
	server := getServer(t, h.c, "survival")
	server.Annotations = map[string]string{v1alpha1.AnnotationRestart: `{"request":"pending","pod_uid":"pod-1"}`}
	if err := h.c.Update(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	var steps []string
	h.r.Prober = fakeProber{players: operator.PlayerCount{Known: true}, steps: &steps}
	recordDiskSample(t, h, claim, 100)
	server = h.pass()
	if server.Spec.DesiredState != v1alpha1.DesiredStopped || server.Status.AutoStop == nil || server.Status.AutoStop.Reason != v1alpha1.StopDiskFull || countDiskEvents(server, v1alpha1.EventDiskFull) != 1 || !podPresent(t, h.c) {
		t.Fatal("full disk did not preserve the Pod and request a normal stop before restart handling")
	}
	server = h.pass()
	if len(steps) != 2 || steps[0] != "save" || steps[1] != "commands" || *getSTS(t, h.c, "survival").Spec.Replicas != 0 || server.Annotations[v1alpha1.AnnotationRestart] != "" {
		t.Fatalf("normal shutdown was bypassed: steps=%v", steps)
	}
	h.pass()
	if len(steps) != 2 || countDiskEvents(getServer(t, h.c, "survival"), v1alpha1.EventDiskFull) != 1 {
		t.Fatal("a stopped full disk repeated hooks or events")
	}
}

func TestDiskFullSurvivesBadSamplesAndFreshLowAllowsANewStart(t *testing.T) {
	h, claim := newDiskHost(t)
	recordDiskSample(t, h, claim, 100)
	h.pass()
	fullRaw := getServer(t, h.c, "survival").Annotations[v1alpha1.AnnotationDiskUsage]
	for _, mode := range []string{"missing", "damaged", "stale", "replacement claim"} {
		server := getServer(t, h.c, "survival")
		if server.Annotations == nil {
			server.Annotations = map[string]string{}
		}
		switch mode {
		case "missing":
			delete(server.Annotations, v1alpha1.AnnotationDiskUsage)
		case "damaged":
			server.Annotations[v1alpha1.AnnotationDiskUsage] = "broken"
		case "stale":
			server.Annotations[v1alpha1.AnnotationDiskUsage] = fullRaw
			h.clock = h.clock.Add(4 * time.Minute)
		case "replacement claim":
			if err := h.c.Delete(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
			claim.ResourceVersion, claim.UID = "", "replacement-claim"
			if err := h.c.Create(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
		}
		server.Spec.DesiredState = v1alpha1.DesiredRunning
		server.Generation++
		if err := h.c.Update(context.Background(), server); err != nil {
			t.Fatal(err)
		}
		server = h.pass()
		if server.Spec.DesiredState != v1alpha1.DesiredStopped || !meta.IsStatusConditionFalse(server.Status.Conditions, v1alpha1.ConditionDiskSpaceAvailable) {
			t.Fatalf("%s unlocked the full disk", mode)
		}
	}
	h.clock = h.clock.Add(time.Minute)
	recordDiskSample(t, h, claim, 80)
	server := h.pass()
	if !meta.IsStatusConditionTrue(server.Status.Conditions, v1alpha1.ConditionDiskSpaceAvailable) || server.Spec.DesiredState != v1alpha1.DesiredStopped {
		t.Fatal("a successful cleanup did not release the gate or started the server on its own")
	}
	h.start()
	if server := h.pass(); server.Spec.DesiredState != v1alpha1.DesiredRunning {
		t.Fatal("a fresh sample below quota still blocked a requested start")
	}
	var kept corev1.PersistentVolumeClaim
	if err := h.c.Get(context.Background(), client.ObjectKeyFromObject(claim), &kept); err != nil {
		t.Fatal("quota enforcement removed the world volume")
	}
}

func TestDiskQuotaVolumeReadFailureCannotAdvancePendingRestart(t *testing.T) {
	h, claim := newDiskHost(t)
	server := getServer(t, h.c, "survival")
	server.Annotations = map[string]string{v1alpha1.AnnotationRestart: `{"request":"pending","pod_uid":"pod-1"}`}
	if err := h.c.Update(context.Background(), server); err != nil {
		t.Fatal(err)
	}
	recordDiskSample(t, h, claim, 100)
	before := getSTS(t, h.c, "survival").DeepCopy()
	h.r.Volumes = interceptor.NewClient(h.c.(client.WithWatch), interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("volume read failed")
		},
	})
	if _, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(server)}); err == nil {
		t.Fatal("volume lookup failure was treated as permission to restart")
	}
	after := getServer(t, h.c, "survival")
	if !reflect.DeepEqual(before.Spec, getSTS(t, h.c, "survival").Spec) || !podPresent(t, h.c) || after.Spec.DesiredState != v1alpha1.DesiredRunning || after.Annotations[v1alpha1.AnnotationRestart] != server.Annotations[v1alpha1.AnnotationRestart] || countDiskEvents(after, v1alpha1.EventDiskFull) != 0 {
		t.Fatal("volume lookup failure changed the workload, consumed the request or advanced desired state")
	}
	after.Spec.DesiredState = v1alpha1.DesiredStopped
	if err := h.c.Update(context.Background(), after); err != nil {
		t.Fatal(err)
	}
	h.pass()
	if *getSTS(t, h.c, "survival").Spec.Replicas != 0 {
		t.Fatal("a volume lookup failure blocked an explicit stop")
	}
}
