package reaper

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReleaseKeepsOwnershipUntilExtraPortsAreConfirmedCleared(t *testing.T) {
	for _, mode := range []string{"retire world", "retire empty", "idle world", "idle empty"} {
		t.Run(mode, func(t *testing.T) {
			candidate := retiring("ports", "old-owner", false)
			if mode == "idle world" || mode == "idle empty" {
				candidate.RetireRequestedAt = time.Time{}
				candidate.LastActiveAt = idleBy(20 * Day)
			}
			r, store, cluster, archiver := newReaper(DefaultConfig(), candidate)
			if mode == "retire empty" || mode == "idle empty" {
				cluster.noWorld = map[string]bool{WorldPVCName(candidate.Name): true}
			}
			cluster.clearPortsErr = ErrNotQuiet
			sum := mustRun(t, r)
			if sum.AwaitingStop != 1 || len(store.released) != 0 || store.byName[candidate.Name].OwnerID != "old-owner" {
				t.Fatal("unconfirmed port cleanup released ownership")
			}
			archives := archiver.archives
			cluster.clearPortsErr = nil
			mustRun(t, r)
			if !reflect.DeepEqual(cluster.clearedPorts, []string{"ports/uid-ports"}) || len(store.released) != 1 || store.byName[candidate.Name].OwnerID != "" || archiver.archives != archives {
				t.Fatal("release did not retry cleanup or duplicated the completed archive")
			}
		})
	}
}

func TestReleaseRechecksOwnerBeforeClearingPorts(t *testing.T) {
	candidate := retiring("ports", "old-owner", false)
	r, store, cluster, _ := newReaper(DefaultConfig(), candidate)
	store.byName[candidate.Name].OwnerID = "new-owner"
	err := r.releaseWorld(context.Background(), testNow, candidate, cluster.crds[candidate.Name])
	if !errors.Is(err, ErrNotQuiet) || len(cluster.clearedPorts) != 0 || len(store.released) != 0 || store.byName[candidate.Name].OwnerID != "new-owner" {
		t.Fatal("stale reaper candidate cleared a new owner's ports")
	}
}

func TestReaperPortCleanupRequiresFreshConfirmationAndMatchingUID(t *testing.T) {
	for _, mode := range []string{"ready", "stale", "replaced", "missing"} {
		t.Run(mode, func(t *testing.T) {
			server := holdServer(v1alpha1.DesiredStopped, v1alpha1.PhaseStopped)
			server.UID, server.Generation = "current-cr", 3
			server.Status.Conditions = []metav1.Condition{{Type: v1alpha1.ConditionExtraPortsReady, Status: metav1.ConditionTrue, Reason: "Ready", ObservedGeneration: 3}}
			if mode == "stale" {
				server.Status.Conditions[0].ObservedGeneration = 2
			}
			if mode == "replaced" {
				server.Spec.ExtraPorts = []v1alpha1.ExtraPort{{Protocol: corev1.ProtocolUDP, Port: 24454}}
			}
			k, c, _ := holdCluster(t, server)
			if mode == "missing" {
				if err := c.Delete(context.Background(), server); err != nil {
					t.Fatal(err)
				}
			}
			uid := "current-cr"
			if mode == "replaced" {
				uid = "old-cr"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			err := k.ClearExtraPorts(ctx, server.Name, uid)
			switch mode {
			case "ready", "missing":
				if err != nil {
					t.Fatal(err)
				}
			case "stale":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("stale confirmation was accepted: %v", err)
				}
			case "replaced":
				if !errors.Is(err, ErrNotQuiet) {
					t.Fatal("replacement CR did not reject the stale cleanup")
				}
				var current v1alpha1.MinecraftServer
				if err := c.Get(context.Background(), client.ObjectKeyFromObject(server), &current); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(current.Spec, server.Spec) {
					t.Fatal("cleanup changed a replacement CR")
				}
			}
		})
	}
}
