package operator_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/operator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// runThenStop takes survival to Running with a ready pod, flips it to Stopped and
// returns the result of the first Stopped pass.
func runThenStop(t *testing.T, r *operator.Reconciler, c client.Client) time.Duration {
	t.Helper()
	reconcile(t, r, "survival")
	markPodReady(t, c, "survival")
	reconcile(t, r, "survival")
	setDesired(t, c, v1alpha1.DesiredStopped)
	return reconcile(t, r, "survival").RequeueAfter
}

func setDesired(t *testing.T, c client.Client, desired v1alpha1.DesiredState) {
	t.Helper()
	server := getServer(t, c, "survival")
	server.Spec.DesiredState = desired
	if err := c.Update(context.Background(), server); err != nil {
		t.Fatalf("set desiredState %s: %v", desired, err)
	}
}

func at(offset time.Duration) func() metav1.Time {
	return func() metav1.Time { return metav1.NewTime(fixedNow().Add(offset)) }
}

func replicas(t *testing.T, c client.Client) int32 {
	t.Helper()
	sts := getSTS(t, c, "survival")
	if sts.Spec.Replicas == nil {
		return 1
	}
	return *sts.Spec.Replicas
}

// A stop with players on the server warns them, holds the pod for the window, then
// saves and scales down with a last line.
func TestStopNotice_WarnsPlayersThenStops(t *testing.T) {
	var saves, said []string
	prober := fakeProber{players: operator.PlayerCount{Online: 3, Max: 20, Known: true}, saves: &saves, broadcasts: &said}
	r, c := newReconciler(t, prober, runningServer(), rconSecret())

	if wait := runThenStop(t, r, c); wait != operator.StopNoticeWindow {
		t.Fatalf("requeue = %v, want the %v window", wait, operator.StopNoticeWindow)
	}
	if len(said) != 1 || !strings.Contains(said[0], "30 秒") || !strings.Contains(said[0], "30 seconds") {
		t.Fatalf("broadcasts = %q, want the 30-second warning in both languages", said)
	}
	server := getServer(t, c, "survival")
	if server.Status.StopNoticeAt == nil || !server.Status.StopNoticeAt.Equal(ptrTime(fixedNow())) {
		t.Errorf("stopNoticeAt = %v, want %v", server.Status.StopNoticeAt, fixedNow())
	}
	if server.Status.Phase != v1alpha1.PhaseRunning || !server.Status.Ready {
		t.Errorf("phase = %s ready=%v, want the server still Running while players are warned", server.Status.Phase, server.Status.Ready)
	}
	if n := replicas(t, c); n != 1 || len(saves) != 0 {
		t.Fatalf("replicas = %d saves = %d, want the pod left alone during the window", n, len(saves))
	}

	// A pass inside the window waits out the rest and says nothing new.
	r.Now = at(10 * time.Second)
	if wait := reconcile(t, r, "survival").RequeueAfter; wait != 20*time.Second {
		t.Errorf("requeue 10s in = %v, want 20s", wait)
	}
	if len(said) != 1 || len(saves) != 0 || replicas(t, c) != 1 {
		t.Fatalf("10s in: broadcasts=%q saves=%d replicas=%d, want nothing new", said, len(saves), replicas(t, c))
	}

	r.Now = at(operator.StopNoticeWindow)
	reconcile(t, r, "survival")
	if len(said) != 2 || !strings.Contains(said[1], "正在保存") {
		t.Errorf("broadcasts = %q, want the stopping-now line last", said)
	}
	if len(saves) != 1 || replicas(t, c) != 0 {
		t.Fatalf("saves = %d replicas = %d, want one save and the scale-down once the window is over", len(saves), replicas(t, c))
	}
	server = getServer(t, c, "survival")
	if server.Status.StopNoticeAt != nil || server.Status.Phase != v1alpha1.PhaseStopping {
		t.Errorf("stopNoticeAt = %v phase = %s, want the stamp cleared and Stopping", server.Status.StopNoticeAt, server.Status.Phase)
	}

	reconcile(t, r, "survival")
	if len(said) != 2 || len(saves) != 1 {
		t.Errorf("after the scale-down: broadcasts=%q saves=%d, want no more", said, len(saves))
	}
}

// Nobody to warn, or no way to warn them: the stop goes ahead on the first pass.
// A tally the server did not give still counts as maybe-populated.
func TestStopNotice_OnlyWhenSomeoneMayBeWarned(t *testing.T) {
	cases := []struct {
		name       string
		prober     fakeProber
		noRcon     bool
		wantNotice bool
		wantSaid   int
	}{
		{name: "empty server", prober: fakeProber{players: operator.PlayerCount{Online: 0, Max: 20, Known: true}}},
		{name: "probe fails", prober: fakeProber{err: errors.New("connection refused")}},
		{name: "broadcast fails", prober: fakeProber{players: operator.PlayerCount{Online: 2, Max: 20, Known: true}, broadcastErr: errors.New("i/o timeout")}, wantSaid: 1},
		{name: "rcon disabled", prober: fakeProber{players: operator.PlayerCount{Online: 2, Max: 20, Known: true}}, noRcon: true},
		{name: "unfamiliar list reply", prober: fakeProber{}, wantNotice: true, wantSaid: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var saves, said []string
			tc.prober.saves, tc.prober.broadcasts = &saves, &said
			s := runningServer()
			if tc.noRcon {
				// The Secret and its reference stay, as they do when RCON is switched off.
				s.Spec.Rcon.Enabled = false
			}
			r, c := newReconciler(t, tc.prober, s, rconSecret())

			wait := runThenStop(t, r, c)
			if len(said) != tc.wantSaid {
				t.Errorf("broadcasts = %q, want %d", said, tc.wantSaid)
			}
			stamped := getServer(t, c, "survival").Status.StopNoticeAt != nil
			if tc.wantNotice {
				if wait != operator.StopNoticeWindow || replicas(t, c) != 1 || !stamped {
					t.Errorf("requeue=%v replicas=%d stamped=%v, want the notice window", wait, replicas(t, c), stamped)
				}
				return
			}
			if replicas(t, c) != 0 || stamped {
				t.Errorf("replicas=%d stamped=%v, want an immediate stop", replicas(t, c), stamped)
			}
		})
	}
}

// Starting the server again inside the window calls the stop off: the warned
// players hear so, the pod stays, and a later stop warns afresh.
func TestStopNotice_CalledOffByStart(t *testing.T) {
	var saves, said []string
	prober := fakeProber{players: operator.PlayerCount{Online: 3, Max: 20, Known: true}, saves: &saves, broadcasts: &said}
	r, c := newReconciler(t, prober, runningServer(), rconSecret())
	runThenStop(t, r, c)

	r.Now = at(12 * time.Second)
	setDesired(t, c, v1alpha1.DesiredRunning)
	reconcile(t, r, "survival")
	if len(said) != 2 || !strings.Contains(said[1], "取消") {
		t.Errorf("broadcasts = %q, want the called-off line", said)
	}
	server := getServer(t, c, "survival")
	if server.Status.StopNoticeAt != nil {
		t.Errorf("stopNoticeAt = %v, want it cleared", server.Status.StopNoticeAt)
	}
	if replicas(t, c) != 1 || len(saves) != 0 || server.Status.Phase != v1alpha1.PhaseRunning {
		t.Fatalf("replicas=%d saves=%d phase=%s, want the server left running", replicas(t, c), len(saves), server.Status.Phase)
	}

	// A Running pass after that says nothing more.
	reconcile(t, r, "survival")
	if len(said) != 2 {
		t.Errorf("broadcasts = %q, want no repeat of the called-off line", said)
	}

	r.Now = at(40 * time.Second)
	setDesired(t, c, v1alpha1.DesiredStopped)
	if wait := reconcile(t, r, "survival").RequeueAfter; wait != operator.StopNoticeWindow {
		t.Errorf("requeue = %v, want a full window for the new stop", wait)
	}
	if len(said) != 3 || !strings.Contains(said[2], "30 秒") {
		t.Errorf("broadcasts = %q, want a fresh warning", said)
	}
	if got := getServer(t, c, "survival").Status.StopNoticeAt; got == nil || !got.Equal(ptrTime(at(40*time.Second)())) {
		t.Errorf("stopNoticeAt = %v, want the new stop's time", got)
	}
}
