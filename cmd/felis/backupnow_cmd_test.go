package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/api"
	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/naming"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// bnRig drives backupNowRun against scripted servers and Jobs on a fake clock that
// moves only when the run sleeps.
type bnRig struct {
	out, errw bytes.Buffer
	clock     time.Time
	events    []string
	worlds    []backupNowWorld
	// stopAfter is how many polls a halted server takes to stop; -1 never does.
	stopAfter map[string]int
	polls     map[string]int
	reqErr    map[string]error
	// states is what the server's new backup Job reports on each poll after the
	// request, the last one repeating; "" is no Job.
	states   map[string][]string
	messages map[string]string
	jobPolls map[string]int
	// stopErr fails a server's stop polls; jobsFail fails the nth (1-based) Job
	// list of a server.
	stopErr  map[string]error
	jobsFail map[string]int
	jobCalls map[string]int
	// ctx is the run's context, and onAct runs after each halt and request.
	ctx   context.Context
	onAct func(event string)
}

func newBNRig(worlds ...backupNowWorld) *bnRig {
	return &bnRig{
		clock:     time.Unix(1_800_000_000, 0),
		worlds:    worlds,
		stopAfter: map[string]int{},
		polls:     map[string]int{},
		reqErr:    map[string]error{},
		states:    map[string][]string{},
		messages:  map[string]string{},
		jobPolls:  map[string]int{},
		stopErr:   map[string]error{},
		jobsFail:  map[string]int{},
		jobCalls:  map[string]int{},
		ctx:       context.Background(),
		onAct:     func(string) {},
	}
}

func (g *bnRig) run(names []string, yes, stop bool) int {
	r := backupNowRun{
		out: &g.out, errw: &g.errw, ns: "minecraft",
		list: func(context.Context) ([]backupNowWorld, error) {
			return append([]backupNowWorld(nil), g.worlds...), nil
		},
		stopped: func(_ context.Context, name string) (bool, error) {
			g.polls[name]++
			if err := g.stopErr[name]; err != nil {
				return false, err
			}
			n := g.stopAfter[name]
			return n >= 0 && g.polls[name] > n, nil
		},
		halt: func(_ context.Context, name string) error {
			g.events = append(g.events, "halt "+name)
			g.onAct("halt " + name)
			return nil
		},
		request: func(_ context.Context, name string) error {
			g.events = append(g.events, "request "+name)
			g.onAct("request " + name)
			if err := g.reqErr[name]; err != nil {
				return err
			}
			g.jobPolls[name] = 0
			return nil
		},
		jobs: func(_ context.Context, name string) ([]api.AsyncJob, error) {
			// Every server has an older finished backup, and a restore Job that
			// shows up with the new backup: neither is the Job to wait for.
			g.jobCalls[name]++
			if g.jobCalls[name] == g.jobsFail[name] {
				return nil, errors.New("the apiserver is gone")
			}
			out := []api.AsyncJob{{Name: "backup-" + name + "-old", Kind: "backup", State: "succeeded"}}
			n, requested := g.jobPolls[name]
			if !requested {
				return out, nil
			}
			g.jobPolls[name] = n + 1
			states := g.states[name]
			if len(states) == 0 {
				states = []string{"succeeded"}
			}
			state := states[min(n, len(states)-1)]
			if state == "" {
				return out, nil
			}
			return append([]api.AsyncJob{
				{Name: "restore-" + name + "-x", Kind: "restore", State: "succeeded"},
				{Name: "backup-" + name + "-new", Kind: "backup", State: state, Message: g.messages[name]},
			}, out...), nil
		},
		now:   func() time.Time { return g.clock },
		sleep: func(_ context.Context, d time.Duration) { g.clock = g.clock.Add(d) },
	}
	return r.run(g.ctx, names, yes, stop)
}

func stoppedWorld(name string) backupNowWorld {
	return backupNowWorld{name: name, phase: "Stopped", stopped: true, hasWorld: true}
}

func runningWorld(name string) backupNowWorld {
	return backupNowWorld{name: name, phase: "Running", hasWorld: true}
}

func emptyWorld(name string) backupNowWorld {
	return backupNowWorld{name: name, phase: "Stopped", stopped: true}
}

const (
	bnManualKeep = "Each archive is a manual backup: a server that already holds [archive] manual_keep of them loses its oldest.\n"
	bnStopping   = "Stopping disconnects the players on those servers, and they stay stopped afterwards.\n"
	bnOffsite    = "The archives reach the off-site bucket with the hourly copy; sudo systemctl start felis-offsite.service sends them now.\n"
)

func TestBackupNowPlanChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		stop bool
		want string
	}{
		{false, "felis backup-now: 4 server(s), backed up one at a time:\n" +
			"  alpha    Stopped   back up\n" +
			"  charlie  Stopped   skip: no world volume (never started, nothing to save)\n" +
			"  bravo    Running   skip: running (stop it first, or pass -stop)\n" +
			"  delta    Starting  skip: running (stop it first, or pass -stop)\n" +
			bnManualKeep +
			"Nothing changed. Run again with -yes to back them up.\n"},
		{true, "felis backup-now: 4 server(s), backed up one at a time:\n" +
			"  alpha    Stopped   back up\n" +
			"  charlie  Stopped   skip: no world volume (never started, nothing to save)\n" +
			"  bravo    Running   stop, then back up\n" +
			"  delta    Starting  stop, then back up\n" +
			bnManualKeep + bnStopping +
			"Nothing changed. Run again with -yes to back them up.\n"},
	} {
		t.Run(fmt.Sprintf("stop=%v", tc.stop), func(t *testing.T) {
			delta := runningWorld("delta")
			delta.phase = "Starting"
			g := newBNRig(stoppedWorld("alpha"), runningWorld("bravo"), emptyWorld("charlie"), delta)
			if code := g.run(nil, false, tc.stop); code != 0 {
				t.Fatalf("exit = %d, want 0; stderr %q", code, g.errw.String())
			}
			if g.out.String() != tc.want {
				t.Fatalf("plan =\n%s\nwant\n%s", g.out.String(), tc.want)
			}
			if len(g.events) != 0 || len(g.polls) != 0 {
				t.Fatalf("the plan acted: events %v, polls %v", g.events, g.polls)
			}
		})
	}
}

// A plan that saves nothing says so, without the manual_keep warning; a running
// server counts as something to save once -stop is given.
func TestBackupNowPlanWithNothingToSave(t *testing.T) {
	for _, tc := range []struct {
		stop bool
		want string
	}{
		{false, "felis backup-now: 2 server(s), backed up one at a time:\n" +
			"  charlie  Stopped   skip: no world volume (never started, nothing to save)\n" +
			"  bravo    Running   skip: running (stop it first, or pass -stop)\n" +
			"Nothing to back up.\n"},
		{true, "felis backup-now: 2 server(s), backed up one at a time:\n" +
			"  charlie  Stopped   skip: no world volume (never started, nothing to save)\n" +
			"  bravo    Running   stop, then back up\n" +
			bnManualKeep + bnStopping +
			"Nothing changed. Run again with -yes to back them up.\n"},
	} {
		g := newBNRig(runningWorld("bravo"), emptyWorld("charlie"))
		if code := g.run(nil, false, tc.stop); code != 0 {
			t.Fatalf("stop=%v: exit = %d, want 0", tc.stop, code)
		}
		if g.out.String() != tc.want {
			t.Fatalf("stop=%v: plan =\n%s\nwant\n%s", tc.stop, g.out.String(), tc.want)
		}
	}
}

func TestBackupNowBacksUpEachWorldInTurn(t *testing.T) {
	g := newBNRig(stoppedWorld("alpha"), runningWorld("bravo"), emptyWorld("charlie"), stoppedWorld("delta"))
	g.states["alpha"] = []string{"", "running", "succeeded"}
	g.states["delta"] = []string{"running", "failed"}
	g.messages["delta"] = "felis backup: not enough free disk for the archive"
	g.stopAfter["bravo"] = 2
	g.states["bravo"] = []string{"running", "running", "running", "succeeded"}

	code := g.run(nil, true, true)
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (delta failed)", code)
	}
	if want := []string{"request alpha", "request delta", "halt bravo", "request bravo"}; !reflect.DeepEqual(g.events, want) {
		t.Fatalf("events = %v, want %v", g.events, want)
	}
	_, run, _ := strings.Cut(g.out.String(), bnStopping+"")
	want := "  alpha: backing up\n" +
		"  alpha: archived in 4s\n" +
		"  delta: backing up\n" +
		"  delta: failed: felis backup: not enough free disk for the archive (kubectl -n minecraft logs job/backup-delta-new)\n" +
		"  bravo: stopping\n" +
		"  bravo: stopped after 4s\n" +
		"  bravo: backing up\n" +
		"  bravo: archived in 6s\n" +
		"2 backed up, 1 failed, 1 without a world.\n" +
		"Left stopped: bravo. Start them from the panel when you are done.\n" +
		bnOffsite
	if run != want {
		t.Fatalf("run =\n%s\nwant\n%s", run, want)
	}
}

func TestBackupNowSkipsRunningServersWithoutStop(t *testing.T) {
	g := newBNRig(stoppedWorld("alpha"), runningWorld("bravo"))
	if code := g.run(nil, true, false); code != 1 {
		t.Fatalf("exit = %d, want 1 (bravo was not backed up)", code)
	}
	if want := []string{"request alpha"}; !reflect.DeepEqual(g.events, want) {
		t.Fatalf("events = %v, want %v", g.events, want)
	}
	if len(g.polls) != 0 {
		t.Fatalf("polled a server it did not stop: %v", g.polls)
	}
	_, run, _ := strings.Cut(g.out.String(), "Nothing changed")
	if run != "" {
		t.Fatalf("-yes printed the plan's closing line")
	}
	_, run, _ = strings.Cut(g.out.String(), bnManualKeep)
	want := "  alpha: backing up\n  alpha: archived in 0s\n1 backed up, 1 skipped (running).\n" + bnOffsite
	if run != want {
		t.Fatalf("run =\n%s\nwant\n%s", run, want)
	}
}

func TestBackupNowExitsCleanWhenEverythingIsSaved(t *testing.T) {
	// -stop with nothing running stops nothing and says nothing about stopping.
	g := newBNRig(stoppedWorld("alpha"), emptyWorld("charlie"))
	if code := g.run(nil, true, true); code != 0 {
		t.Fatalf("exit = %d, want 0; output\n%s", code, g.out.String())
	}
	_, run, _ := strings.Cut(g.out.String(), bnManualKeep)
	if want := "  alpha: backing up\n  alpha: archived in 0s\n1 backed up, 1 without a world.\n" + bnOffsite; run != want {
		t.Fatalf("run =\n%s\nwant\n%s", run, want)
	}

	g = newBNRig(emptyWorld("charlie"))
	if code := g.run(nil, true, false); code != 0 {
		t.Fatalf("exit = %d, want 0 for a server with nothing to save", code)
	}
	// Nothing to archive: no manual_keep warning, and no off-site hint.
	if want := "felis backup-now: 1 server(s), backed up one at a time:\n" +
		"  charlie  Stopped   skip: no world volume (never started, nothing to save)\n" +
		"0 backed up, 1 without a world.\n"; g.out.String() != want {
		t.Fatalf("output =\n%s\nwant\n%s", g.out.String(), want)
	}
	if len(g.events) != 0 {
		t.Fatalf("events = %v, want none", g.events)
	}
}

func TestBackupNowStopsAtAnUnreachableAPI(t *testing.T) {
	g := newBNRig(stoppedWorld("alpha"), stoppedWorld("bravo"), runningWorld("charlie"))
	g.reqErr["alpha"] = fmt.Errorf("%w: dial tcp 10.43.0.9:8081: connect: connection refused", errBackupAPIUnreachable)
	if code := g.run(nil, true, true); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if want := []string{"request alpha"}; !reflect.DeepEqual(g.events, want) {
		t.Fatalf("events = %v, want %v: nothing after the API proved unreachable, and no server stopped", g.events, want)
	}
	_, run, _ := strings.Cut(g.out.String(), bnStopping)
	want := "  alpha: failed: felis-api unreachable (a backup needs it alive): dial tcp 10.43.0.9:8081: connect: connection refused\n" +
		"Stopped: nothing more can be backed up until felis-api answers (kubectl -n felis get pods).\n" +
		"0 backed up, 1 failed.\n"
	if run != want {
		t.Fatalf("run =\n%s\nwant\n%s", run, want)
	}

	// Any other refusal is that world's alone: the run goes on.
	g = newBNRig(stoppedWorld("alpha"), stoppedWorld("bravo"))
	g.reqErr["alpha"] = errors.New("felis-api: the world is being restored")
	if code := g.run(nil, true, false); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if want := []string{"request alpha", "request bravo"}; !reflect.DeepEqual(g.events, want) {
		t.Fatalf("events = %v, want %v", g.events, want)
	}
}

func TestBackupNowGivesUpOnAServerThatDoesNotStop(t *testing.T) {
	g := newBNRig(runningWorld("bravo"), runningWorld("echo"))
	g.stopAfter["bravo"] = -1
	if code := g.run(nil, true, true); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if want := []string{"halt bravo", "halt echo", "request echo"}; !reflect.DeepEqual(g.events, want) {
		t.Fatalf("events = %v, want %v", g.events, want)
	}
	// One poll at the start and one per 2s sleep up to the 10-minute mark.
	if g.polls["bravo"] != 301 {
		t.Fatalf("bravo polled %d times, want 301", g.polls["bravo"])
	}
	_, run, _ := strings.Cut(g.out.String(), bnStopping)
	want := "  bravo: stopping\n" +
		"  bravo: failed: did not stop within 10m0s (kubectl -n minecraft describe minecraftserver bravo)\n" +
		"  echo: stopping\n" +
		"  echo: stopped after 0s\n" +
		"  echo: backing up\n" +
		"  echo: archived in 0s\n" +
		"1 backed up, 1 failed.\n" +
		"Left stopped: bravo, echo. Start them from the panel when you are done.\n" +
		bnOffsite
	if run != want {
		t.Fatalf("run =\n%s\nwant\n%s", run, want)
	}
}

func TestBackupNowReportsAJobThatNeverRuns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		states []string
		polls  int
		want   string
	}{
		{"never appears", []string{""}, 31,
			"felis-api accepted the backup, but no backup Job appeared within 1m0s (kubectl -n minecraft get jobs)"},
		{"deleted while running", []string{"running", "running", ""}, 3,
			"its backup Job backup-alpha-new was deleted before it finished"},
		{"fails without a message", []string{"failed"}, 1,
			"the backup Job failed (kubectl -n minecraft logs job/backup-alpha-new)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newBNRig(stoppedWorld("alpha"))
			g.states["alpha"] = tc.states
			if code := g.run(nil, true, false); code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			if !strings.Contains(g.out.String(), "  alpha: failed: "+tc.want+"\n") {
				t.Fatalf("output =\n%s\nwant the line %q", g.out.String(), tc.want)
			}
			if g.jobPolls["alpha"] != tc.polls {
				t.Fatalf("polled the Jobs %d times after the request, want %d", g.jobPolls["alpha"], tc.polls)
			}
		})
	}
}

func TestBackupNowNamedServers(t *testing.T) {
	g := newBNRig(stoppedWorld("alpha"), stoppedWorld("bravo"), stoppedWorld("delta"))
	if code := g.run([]string{"delta", "alpha", "delta"}, true, false); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if want := []string{"request delta", "request alpha"}; !reflect.DeepEqual(g.events, want) {
		t.Fatalf("events = %v, want %v", g.events, want)
	}
	if !strings.HasPrefix(g.out.String(), "felis backup-now: 2 server(s), backed up one at a time:\n  delta  Stopped   back up\n  alpha  Stopped   back up\n") {
		t.Fatalf("plan =\n%s", g.out.String())
	}

	for _, tc := range []struct{ name, want string }{
		{"login", "felis backup-now: login is a system server: its world is rebuilt by felis setup and has no backups\n"},
		{"lobby", "felis backup-now: lobby is a system server: its world is rebuilt by felis setup and has no backups\n"},
		{"nope", "felis backup-now: no server named \"nope\"\n"},
	} {
		g := newBNRig(stoppedWorld("alpha"))
		if code := g.run([]string{"alpha", tc.name}, true, false); code != 2 {
			t.Fatalf("%s: exit = %d, want 2", tc.name, code)
		}
		if g.errw.String() != tc.want {
			t.Fatalf("%s: stderr = %q, want %q", tc.name, g.errw.String(), tc.want)
		}
		if len(g.events) != 0 || g.out.Len() != 0 {
			t.Fatalf("%s: acted on a bad name: events %v, output %q", tc.name, g.events, g.out.String())
		}
	}

	g = newBNRig()
	if code := g.run(nil, true, false); code != 0 || g.out.String() != "felis backup-now: there are no user servers\n" {
		t.Fatalf("empty fleet: exit %d, output %q", code, g.out.String())
	}
}

// The world list mirrors the backup route's own gate, so the plan says exactly what
// the route would refuse.
func TestListBackupNowWorlds(t *testing.T) {
	withStatus := func(ms *v1alpha1.MinecraftServer, ready bool) *v1alpha1.MinecraftServer {
		ms.Status.Ready = ready
		return ms
	}
	pvc := func(server, ns string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: naming.WorldPVCName(server), Namespace: ns}}
	}
	pod := func(name, ns string, labels map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels}}
	}
	gameLabels := func(server string) map[string]string {
		return map[string]string{v1alpha1.LabelServer: server, v1alpha1.LabelComponent: "server"}
	}
	other := mcServer("zulu", v1alpha1.DesiredStopped, v1alpha1.PhaseStopped)
	other.Namespace = "elsewhere"
	hotel := mcServer("hotel", v1alpha1.DesiredStopped, "")
	objs := []client.Object{
		mcServer("alpha", v1alpha1.DesiredStopped, v1alpha1.PhaseStopped), pvc("alpha", haltNS),
		// A backup Job's pod carries the server label with its own component, and
		// a game pod of the same name in another namespace is somebody else's.
		pod("backup-alpha-1-x", haltNS, map[string]string{v1alpha1.LabelServer: "alpha", "app.kubernetes.io/component": "world-backup"}),
		pod("alpha-0", "elsewhere", gameLabels("alpha")),
		withStatus(mcServer("bravo", v1alpha1.DesiredRunning, v1alpha1.PhaseRunning), true), pvc("bravo", haltNS), pod("bravo-0", haltNS, gameLabels("bravo")),
		mcServer("charlie", v1alpha1.DesiredStopped, v1alpha1.PhaseStopped),
		mcServer("delta", v1alpha1.DesiredStopped, v1alpha1.PhaseStopped), pvc("delta", haltNS), pod("delta-0", haltNS, gameLabels("delta")),
		mcServer("echo", v1alpha1.DesiredStopped, v1alpha1.PhaseStopping), pvc("echo", haltNS),
		mcServer("foxtrot", v1alpha1.DesiredRunning, v1alpha1.PhaseStopped), pvc("foxtrot", haltNS),
		withStatus(mcServer("golf", v1alpha1.DesiredStopped, v1alpha1.PhaseStopped), true), pvc("golf", haltNS),
		hotel, pvc("hotel", haltNS),
		mcServer("login", v1alpha1.DesiredStopped, v1alpha1.PhaseStopped), pvc("login", haltNS),
		mcServer("lobby", v1alpha1.DesiredStopped, v1alpha1.PhaseStopped), pvc("lobby", haltNS),
		other, pvc("zulu", "elsewhere"),
	}
	got, err := listBackupNowWorlds(context.Background(), haltClient(t, objs...), haltNS)
	if err != nil {
		t.Fatal(err)
	}
	want := []backupNowWorld{
		{name: "alpha", phase: "Stopped", stopped: true, hasWorld: true},
		{name: "bravo", phase: "Running", hasWorld: true},
		{name: "charlie", phase: "Stopped", stopped: true},
		{name: "delta", phase: "Stopped", hasWorld: true},   // its pod is still going
		{name: "echo", phase: "Stopping", hasWorld: true},   // still stopping
		{name: "foxtrot", phase: "Stopped", hasWorld: true}, // asked to start
		{name: "golf", phase: "Stopped", hasWorld: true},    // still reports ready
		{name: "hotel", phase: "Stopped", hasWorld: true},   // not reconciled yet: the desired state shows
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("worlds =\n%+v\nwant\n%+v", got, want)
	}
}

func TestBackupNowStopsWhenInterrupted(t *testing.T) {
	t.Run("between worlds", func(t *testing.T) {
		g := newBNRig(stoppedWorld("alpha"), stoppedWorld("bravo"))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g.ctx = ctx
		g.onAct = func(e string) {
			if e == "request alpha" {
				cancel()
			}
		}
		if code := g.run(nil, true, false); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if want := []string{"request alpha"}; !reflect.DeepEqual(g.events, want) {
			t.Fatalf("events = %v, want %v", g.events, want)
		}
		_, run, _ := strings.Cut(g.out.String(), bnManualKeep)
		want := "  alpha: backing up\n  alpha: archived in 0s\n" +
			"Interrupted: a backup Job already started runs to its end.\n" +
			"1 backed up.\n" + bnOffsite
		if run != want {
			t.Fatalf("run =\n%s\nwant\n%s", run, want)
		}
	})
	t.Run("while a Job runs", func(t *testing.T) {
		g := newBNRig(stoppedWorld("alpha"))
		g.states["alpha"] = []string{"running"}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g.ctx = ctx
		g.onAct = func(string) { cancel() }
		if code := g.run(nil, true, false); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if g.jobPolls["alpha"] != 1 {
			t.Fatalf("polled the Job %d times after the interrupt, want 1", g.jobPolls["alpha"])
		}
		if !strings.Contains(g.out.String(), "  alpha: failed: context canceled\nInterrupted: a backup Job already started runs to its end.\n0 backed up, 1 failed.\n") {
			t.Fatalf("output =\n%s", g.out.String())
		}
	})
	t.Run("while a server stops", func(t *testing.T) {
		g := newBNRig(runningWorld("bravo"))
		g.stopAfter["bravo"] = -1
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g.ctx = ctx
		g.onAct = func(string) { cancel() }
		if code := g.run(nil, true, true); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if g.polls["bravo"] != 1 {
			t.Fatalf("polled bravo %d times after the interrupt, want 1", g.polls["bravo"])
		}
		_, run, _ := strings.Cut(g.out.String(), bnStopping)
		want := "  bravo: stopping\n  bravo: failed: context canceled\n" +
			"Interrupted: a backup Job already started runs to its end.\n" +
			"0 backed up, 1 failed.\n" +
			"Left stopped: bravo. Start them from the panel when you are done.\n"
		if run != want {
			t.Fatalf("run =\n%s\nwant\n%s", run, want)
		}
	})
}

func TestBackupNowReportsClusterErrors(t *testing.T) {
	g := newBNRig(runningWorld("bravo"))
	g.stopErr["bravo"] = errors.New("the apiserver is gone")
	if code := g.run(nil, true, true); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	_, run, _ := strings.Cut(g.out.String(), bnStopping)
	// The stop was asked for, so the server stays stopped whatever the poll said.
	want := "  bravo: stopping\n  bravo: failed: the apiserver is gone\n0 backed up, 1 failed.\n" +
		"Left stopped: bravo. Start them from the panel when you are done.\n"
	if run != want {
		t.Fatalf("run =\n%s\nwant\n%s", run, want)
	}

	for _, tc := range []struct {
		call   int
		events []string
	}{
		{1, nil},                       // before the request: nothing is asked for
		{2, []string{"request alpha"}}, // the first poll after it
	} {
		g := newBNRig(stoppedWorld("alpha"))
		g.jobsFail["alpha"] = tc.call
		if code := g.run(nil, true, false); code != 1 {
			t.Fatalf("call %d: exit = %d, want 1", tc.call, code)
		}
		if !reflect.DeepEqual(g.events, tc.events) {
			t.Fatalf("call %d: events = %v, want %v", tc.call, g.events, tc.events)
		}
		if !strings.Contains(g.out.String(), "  alpha: failed: list its backup Jobs: the apiserver is gone\n") {
			t.Fatalf("call %d: output =\n%s", tc.call, g.out.String())
		}
	}
}
