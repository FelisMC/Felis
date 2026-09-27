package watchdog

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/dbbackup"
	"felis.lolicon.best/internal/imagepush"
	"felis.lolicon.best/internal/offsite"
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
		"deployment/felis-operator", "deployment/felis-postgres", "deployment/registry",
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
		case "deployment/felis-postgres":
			if f.Severity != Critical || !strings.Contains(f.SummaryEN, "database is down") {
				t.Errorf("database finding = %+v, want a critical database outage", f)
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
	touch := func(at time.Time, label string) {
		if err := os.WriteFile(filepath.Join(dir, dbbackup.BundleName(at, label)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A manual bundle alone: the timer has never written one.
	touch(t0.Add(-time.Hour), "manual")
	if f := BackupFinding(dir, t0); f == nil || f.Key != "db-backup" ||
		f.SummaryEN != "no daily control-plane database backup in "+dir+"; the newer felis-db-20260924T110000Z-manual.tar came from a manual run or another job and ages while the timer stays broken" ||
		f.Summary != dir+" 里没有每日定时的控制面数据库备份；更新的 felis-db-20260924T110000Z-manual.tar 来自手动或其他任务，定时任务修好之前它会一天天变旧" {
		t.Fatalf("manual only: %+v", f)
	}
	// A stale daily bundle under that fresh manual one: still the timer's alarm.
	touch(t0.Add(-30*time.Hour), "daily")
	if f := BackupFinding(dir, t0); f == nil || f.Key != "db-backup" ||
		f.SummaryEN != "the newest daily control-plane database backup is 30h old (felis-db-20260923T060000Z-daily.tar); the newer felis-db-20260924T110000Z-manual.tar came from a manual run or another job and ages while the timer stays broken" ||
		!strings.Contains(f.Hint, "sudo systemctl start felis-db-backup") {
		t.Fatalf("stale daily under a fresh manual: %+v", f)
	}
	stale := t.TempDir()
	if err := os.WriteFile(filepath.Join(stale, dbbackup.BundleName(t0.Add(-30*time.Hour), "daily")), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if f := BackupFinding(stale, t0); f == nil || f.SummaryEN != "the newest daily control-plane database backup is 30h old (felis-db-20260923T060000Z-daily.tar)" ||
		f.Summary != "最新的每日控制面数据库备份已是 30h 前（felis-db-20260923T060000Z-daily.tar）" {
		t.Fatalf("stale daily alone: %+v", f)
	}
	touch(t0.Add(-2*time.Hour), "daily")
	if f := BackupFinding(dir, t0); f != nil {
		t.Fatalf("fresh backup reported: %+v", f)
	}
}

// TestBackupFindingServers: a fresh newest bundle whose MinecraftServer export
// failed is reported, since a lost host restores from it; an older bundle
// with the same gap, a bundle taken without the export and a stale newest
// bundle are left to the checks that own them.
func TestBackupFindingServers(t *testing.T) {
	dir := t.TempDir()
	bundle := func(at time.Time, label, serversError string) {
		t.Helper()
		m, err := json.Marshal(map[string]any{"format": 1, "label": label, "servers_error": serversError})
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		if err := tw.WriteHeader(&tar.Header{Name: "MANIFEST.json", Mode: 0o600, Size: int64(len(m))}); err != nil {
			t.Fatal(err)
		}
		tw.Write(m)
		tw.Close()
		if err := os.WriteFile(filepath.Join(dir, dbbackup.BundleName(at, label)), buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bundle(t0.Add(-5*time.Hour), "daily", "k3s kubectl get minecraftservers: connection refused")
	bundle(t0.Add(-3*time.Hour), "manual", "")
	if f := BackupFinding(dir, t0); f != nil {
		t.Fatalf("an older bundle's gap reported under a whole newer one: %+v", f)
	}
	bundle(t0.Add(-2*time.Hour), "pre-migrate", "k3s kubectl get minecraftservers: connection refused")
	f := BackupFinding(dir, t0)
	if f == nil || f.Key != "db-backup-servers" || f.Severity != Warning ||
		!strings.Contains(f.SummaryEN, "felis-db-20260924T100000Z-pre-migrate.tar lacks the MinecraftServer objects (k3s kubectl get minecraftservers: connection refused)") ||
		!strings.Contains(f.Summary, "缺少 MinecraftServer 对象") || !strings.Contains(f.Hint, "felis db backup") {
		t.Fatalf("newest bundle without servers: %+v", f)
	}
	if f := BackupFinding(dir, t0.Add(30*time.Hour)); f == nil || f.Key != "db-backup" {
		t.Fatalf("stale: %+v, want the staleness finding", f)
	}
}

func TestOffsiteFinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "offsite", "status.json")
	if f := OffsiteFinding(path, t0); f == nil || !strings.Contains(f.SummaryEN, "never completed") {
		t.Fatalf("no status: %+v", f)
	}
	write := func(st offsite.Status) {
		if err := offsite.WriteStatus(path, st); err != nil {
			t.Fatal(err)
		}
	}
	write(offsite.Status{LastAttempt: t0.Add(-time.Hour), LastError: "bucket unreachable"})
	if f := OffsiteFinding(path, t0); f == nil || !strings.Contains(f.SummaryEN, "machine only (last error: bucket unreachable)") {
		t.Fatalf("failing from the start: %+v", f)
	}
	write(offsite.Status{LastAttempt: t0.Add(-time.Hour), LastSuccess: t0.Add(-13 * time.Hour), LastError: "access denied"})
	if f := OffsiteFinding(path, t0); f == nil || f.Severity != Warning || !strings.Contains(f.SummaryEN, "13h ago (last error: access denied)") {
		t.Fatalf("stale: %+v", f)
	}
	write(offsite.Status{LastAttempt: t0.Add(-time.Hour), LastSuccess: t0.Add(-2 * time.Hour), LastError: "one bundle failed"})
	if f := OffsiteFinding(path, t0); f != nil {
		t.Fatalf("a success within %s reported: %+v", offsite.StaleAfter, f)
	}
	// A key mismatch stops every later run too: reported without waiting out StaleAfter.
	write(offsite.Status{LastAttempt: t0.Add(-time.Hour), LastSuccess: t0.Add(-2 * time.Hour), LastError: "sealed with another key", KeyMismatch: true})
	if f := OffsiteFinding(path, t0); f == nil || !strings.Contains(f.SummaryEN, "has stopped") || !strings.Contains(f.SummaryEN, "(sealed with another key)") || !strings.Contains(f.Hint, "check-key") {
		t.Fatalf("key mismatch: %+v", f)
	}
	// Another host writing the bucket stops every later run too: reported at
	// once, a recent success notwithstanding.
	w := &offsite.Writer{HostID: "bbbbbbbbbbbbbbbb", Host: "prod-1", At: t0.Add(-5 * time.Hour)}
	write(offsite.Status{LastAttempt: t0.Add(-time.Hour), LastSuccess: t0.Add(-2 * time.Hour), Displaced: true, Writer: w})
	if f := OffsiteFinding(path, t0); f == nil || !strings.Contains(f.SummaryEN, "has stopped: host prod-1 (id bbbbbbbbbbbbbbbb) took the bucket over") || !strings.Contains(f.Summary, "prod-1") || !strings.Contains(f.Hint, "take-over -yes") {
		t.Fatalf("displaced: %+v", f)
	}
	write(offsite.Status{LastAttempt: t0.Add(-time.Hour), Standby: true, Writer: w})
	if f := OffsiteFinding(path, t0); f == nil || !strings.Contains(f.SummaryEN, "built from another host's backup") || !strings.Contains(f.SummaryEN, "host prod-1 (id bbbbbbbbbbbbbbbb) wrote it 5h ago") || !strings.Contains(f.Summary, "5h") || !strings.Contains(f.Hint, "take-over -yes") {
		t.Fatalf("standing by: %+v", f)
	}
	write(offsite.Status{LastAttempt: t0.Add(-time.Hour), Standby: true})
	if f := OffsiteFinding(path, t0); f == nil || !strings.Contains(f.SummaryEN, "names no host writing it") {
		t.Fatalf("standing by, no writer named: %+v", f)
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

func TestScanDBFinding(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "status.json")
	if f := ScanDBFinding(path, now); f == nil || !strings.Contains(f.SummaryEN, "never copied") {
		t.Errorf("no status file: %+v", f)
	}
	if err := imagepush.WriteMirrorStatus(path, imagepush.MirrorStatus{LastAttempt: now, LastSuccess: now.Add(-24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if f := ScanDBFinding(path, now); f != nil {
		t.Errorf("a day-old DB: %+v", f)
	}
	if err := imagepush.WriteMirrorStatus(path, imagepush.MirrorStatus{LastAttempt: now, LastSuccess: now.Add(-100 * time.Hour), LastError: "trivy-db: dial tcp: timeout"}); err != nil {
		t.Fatal(err)
	}
	f := ScanDBFinding(path, now)
	if f == nil || f.Severity != Warning || !strings.Contains(f.SummaryEN, "100h") || !strings.Contains(f.SummaryEN, "dial tcp") {
		t.Errorf("stale DB: %+v", f)
	}
}
