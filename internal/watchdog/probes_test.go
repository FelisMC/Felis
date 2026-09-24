package watchdog

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/dbbackup"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func deploy(name string, available int32) *appsv1.Deployment {
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "felis"}}
	d.Status.AvailableReplicas = available
	return d
}

func server(name, role string, desired v1alpha1.DesiredState, phase v1alpha1.Phase) *v1alpha1.MinecraftServer {
	ms := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "minecraft"}}
	if role != "" {
		ms.Labels = map[string]string{v1alpha1.LabelSystemRole: role}
	}
	ms.Spec.DesiredState = desired
	ms.Status.Phase = phase
	return ms
}

func failedJob(name string, at time.Time) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "minecraft"}}
	j.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
		LastTransitionTime: metav1.NewTime(at),
	}}
	return j
}

func findingKeys(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Key)
	}
	sort.Strings(out)
	return out
}

// TestClusterCheck: exactly the broken things are reported — a control-plane
// Deployment down or missing, the login gate not running, a failed user server,
// a recent failed backup Job, a late reaper and a node under disk pressure —
// and the healthy or deliberate states are not.
func TestClusterCheck(t *testing.T) {
	now := t0
	login := server("login", "login", v1alpha1.DesiredRunning, v1alpha1.PhaseStarting)
	login.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: "False", Message: "rcon unreachable"}}
	reaper := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "felis-reaper", Namespace: "minecraft",
		CreationTimestamp: metav1.NewTime(now.Add(-72 * time.Hour))}}
	reaper.Status.LastSuccessfulTime = &metav1.Time{Time: now.Add(-50 * time.Hour)}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	node.Status.Conditions = []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
		{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue},
		{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(
		deploy("felis-api", 1), deploy("felis-operator", 0),
		login,
		server("lobby", "lobby", v1alpha1.DesiredRunning, v1alpha1.PhaseRunning),
		server("halted", "lobby", v1alpha1.DesiredStopped, v1alpha1.PhaseStopped),
		server("broken", "", v1alpha1.DesiredRunning, v1alpha1.PhaseFailed),
		server("asleep", "", v1alpha1.DesiredStopped, v1alpha1.PhaseStopped),
		failedJob("backup-survival-abc", now.Add(-time.Hour)),
		failedJob("backup-old-abc", now.Add(-48*time.Hour)),
		reaper, node,
	).Build()

	got, err := Cluster{Client: cl, ControlNamespace: "felis", MinecraftNamespace: "minecraft"}.Check(context.Background(), now)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := []string{
		"deployment/felis-operator", "deployment/registry",
		"job-failed/backup-survival-abc", "node/n1/DiskPressure", "reaper-stale",
		"server-failed/broken", "system-server/login",
	}
	if strings.Join(findingKeys(got), ",") != strings.Join(want, ",") {
		t.Fatalf("findings = %v, want %v", findingKeys(got), want)
	}
	for _, f := range got {
		switch f.Key {
		case "system-server/login":
			if f.Severity != Critical || !strings.Contains(f.Summary, "rcon unreachable") || !strings.Contains(f.SummaryEN, "players cannot join") {
				t.Errorf("login finding = %+v", f)
			}
		case "deployment/registry":
			if !strings.Contains(f.SummaryEN, "missing") {
				t.Errorf("registry finding = %+v, want missing", f)
			}
		case "job-failed/backup-survival-abc":
			if !f.Event || !strings.Contains(f.SummaryEN, "world backup Job") {
				t.Errorf("job finding = %+v", f)
			}
		}
	}
}

// TestClusterCheckAPIDown: a List the API server refuses is an error, not an
// empty (healthy-looking) cluster.
func TestClusterCheckAPIDown(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	if _, err := (Cluster{Client: cl, ControlNamespace: "felis", MinecraftNamespace: "minecraft"}).Check(context.Background(), t0); err == nil {
		t.Fatal("Check succeeded against a client that knows no kinds")
	}
}

func TestBackupFinding(t *testing.T) {
	dir := t.TempDir()
	if f := BackupFinding(dir, t0); f == nil || !strings.Contains(f.SummaryEN, "no control-plane database backup") {
		t.Fatalf("empty dir: %+v", f)
	}
	touch := func(at time.Time) {
		if err := os.WriteFile(filepath.Join(dir, dbbackup.BundleName(at, "daily")), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	touch(t0.Add(-30 * time.Hour))
	if f := BackupFinding(dir, t0); f == nil || !strings.Contains(f.SummaryEN, "30h0m0s old") {
		t.Fatalf("stale: %+v", f)
	}
	touch(t0.Add(-2 * time.Hour))
	if f := BackupFinding(dir, t0); f != nil {
		t.Fatalf("fresh backup reported: %+v", f)
	}
}

func TestMemoryFinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meminfo")
	write := func(avail int) {
		body := "MemTotal:       24000000 kB\nMemFree:          100000 kB\nMemAvailable:   " + strconv.Itoa(avail) + " kB\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(2000000)
	if f := MemoryFinding(path); f == nil || f.Key != "memory" {
		t.Fatalf("8%% available: %+v", f)
	}
	write(6000000)
	if f := MemoryFinding(path); f != nil {
		t.Fatalf("25%% available reported: %+v", f)
	}
	if f := MemoryFinding(filepath.Join(t.TempDir(), "none")); f != nil {
		t.Fatalf("missing meminfo reported: %+v", f)
	}
}

// TestDiskFindingsDedupAndSkip: two paths on one filesystem yield at most one
// finding, and a path that does not exist is skipped.
func TestDiskFindingsDedupAndSkip(t *testing.T) {
	dir := t.TempDir()
	got := DiskFindings([]string{dir, filepath.Join(dir, "."), filepath.Join(dir, "missing")})
	if len(got) > 1 {
		t.Fatalf("findings = %v, want at most one for one filesystem", findingKeys(got))
	}
}
