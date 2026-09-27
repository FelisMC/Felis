package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/config"
	"felis.lolicon.best/internal/dbbackup"
	"felis.lolicon.best/internal/offsite"
	"felis.lolicon.best/internal/watchdog"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// statusCluster is a one-node install: felis-api with a restarted pod, three
// servers (one of them the lobby), and a pod of another app whose restarts
// are not felis-api's.
func statusClusterObjects() []client.Object {
	replicas := int32(1)
	apiLabels := map[string]string{"app": "felis-api"}
	return []client.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "felis-1"},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
				NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.36.4+k3s1", OSImage: "CentOS Stream 9", KernelVersion: "5.14.0-630.el9.aarch64"},
			},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "felis-api", Namespace: "felis"},
			Spec: appsv1.DeploymentSpec{
				Replicas: &replicas,
				Selector: &metav1.LabelSelector{MatchLabels: apiLabels},
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: "api", Image: "registry.felis.svc:5000/felis/felis-api:v1.4.0@sha256:0123456789abcdef0123456789abcdef",
				}}}},
			},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "felis-api-7d9", Namespace: "felis", Labels: apiLabels},
			Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "api", RestartCount: 2}}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "other-1", Namespace: "felis", Labels: map[string]string{"app": "other"}},
			Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "x", RestartCount: 5}}},
		},
		&v1alpha1.MinecraftServer{
			ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: "minecraft"},
			Spec:       v1alpha1.MinecraftServerSpec{DesiredState: v1alpha1.DesiredRunning},
			Status:     v1alpha1.MinecraftServerStatus{Phase: v1alpha1.PhaseRunning, Players: v1alpha1.PlayersStatus{Online: 2, Max: 20}},
		},
		&v1alpha1.MinecraftServer{
			ObjectMeta: metav1.ObjectMeta{Name: "creative", Namespace: "minecraft"},
			Spec:       v1alpha1.MinecraftServerSpec{DesiredState: v1alpha1.DesiredStopped},
			Status:     v1alpha1.MinecraftServerStatus{Phase: v1alpha1.PhaseStopped},
		},
		&v1alpha1.MinecraftServer{
			ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: "minecraft", Labels: map[string]string{v1alpha1.LabelSystemRole: "lobby"}},
			Spec:       v1alpha1.MinecraftServerSpec{DesiredState: v1alpha1.DesiredRunning},
			Status:     v1alpha1.MinecraftServerStatus{Phase: v1alpha1.PhaseStarting},
		},
		// Another namespace's server is not this install's.
		&v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "other"}},
	}
}

// statusTestEnv is a host with the cluster above, a database backup nine hours
// old, an off-site copy last good two hours ago, and one alert open.
func statusTestEnv(t *testing.T) statusEnv {
	t.Helper()
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	var w watchdogFlags
	w.controlNS = "felis"
	w.backupDir = filepath.Join(dir, "db-backups")
	w.offsiteStatus = filepath.Join(dir, "offsite-status.json")
	w.statePath = filepath.Join(dir, "state.json")
	w.fallbackState = filepath.Join(dir, "fallback.json")
	w.diskPaths = "/nonexistent-felis-status-test"
	unitDir := filepath.Join(dir, "systemd")
	for _, d := range []string{w.backupDir, unitDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(w.backupDir, dbbackup.BundleName(now.Add(-9*time.Hour), dbbackup.LabelDaily)), "x", 0o600)
	writeTestFile(t, filepath.Join(w.backupDir, dbbackup.BundleName(now.Add(-33*time.Hour), dbbackup.LabelDaily)), "x", 0o600)
	if err := offsite.WriteStatus(w.offsiteStatus, offsite.Status{
		LastAttempt: now.Add(-2 * time.Hour), LastSuccess: now.Add(-2 * time.Hour), Bucket: "felis-dr", Endpoint: "https://s3.example.com",
	}); err != nil {
		t.Fatal(err)
	}
	state := &watchdog.State{Alerts: map[string]*watchdog.Alert{
		"db-backup-servers": {Finding: watchdog.Finding{Key: "db-backup-servers", Severity: watchdog.Warning}, FirstSeen: now.Add(-3 * time.Hour), Notified: now.Add(-90 * time.Minute)},
		"proxy":             {Finding: watchdog.Finding{Key: "proxy", Severity: watchdog.Critical}, FirstSeen: now.Add(-time.Hour), Notified: now.Add(-time.Hour), ClearedAt: now.Add(-10 * time.Minute)},
		"disk//var":         {Finding: watchdog.Finding{Key: "disk//var", Severity: watchdog.Critical}, FirstSeen: now.Add(-4 * time.Minute)},
	}}
	if err := watchdog.SaveState(w.statePath, state); err != nil {
		t.Fatal(err)
	}
	meminfo := filepath.Join(dir, "meminfo")
	writeTestFile(t, meminfo, "MemTotal:        8000000 kB\nMemFree:          100000 kB\nMemAvailable:    2000000 kB\n", 0o644)
	writeTestFile(t, filepath.Join(unitDir, "felis-watchdog.timer"), "[Unit]\n", 0o644)
	cfg := &config.Config{Offsite: config.OffsiteConfig{Endpoint: "https://s3.example.com", Bucket: "felis-dr"}}
	return statusEnv{
		cfg: cfg, w: w, cl: fake.NewClientBuilder().WithScheme(newSystemServerScheme(t)).WithObjects(statusClusterObjects()...).Build(),
		backups: func(context.Context) (map[string]time.Time, error) {
			return map[string]time.Time{"survival": now.Add(-3 * time.Hour)}, nil
		},
		run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			switch strings.Join(append([]string{name}, args...), " ") {
			case "systemctl is-active felis-watchdog.timer":
				return []byte("active\n"), nil
			case "systemctl show --timestamp=unix -p Result -p ExecMainExitTimestamp felis-watchdog.service":
				return []byte("Result=success\nExecMainExitTimestamp=@" + strconv.FormatInt(now.Add(-70*time.Second).Unix(), 10) + "\n"), nil
			}
			return nil, errors.New("unexpected")
		},
		unitDir: unitDir, meminfo: meminfo, host: "felis-test", now: now,
	}
}

// lineFields is the words of the first line of out that starts with prefix
// once trimmed.
func lineFields(out, prefix string) []string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), prefix) {
			return strings.Fields(l)
		}
	}
	return nil
}

func TestStatusReport(t *testing.T) {
	env := statusTestEnv(t)
	var out bytes.Buffer
	printStatus(context.Background(), env, &out)
	got := out.String()
	for _, want := range []string{
		"on felis-test at 2026-09-27 12:00 UTC\n",
		"node:     felis-1 Ready, k3s v1.36.4+k3s1, CentOS Stream 9, kernel 5.14.0-630.el9.aarch64\n",
		"\ncontrol plane (namespace felis):\n",
		"\nproxy:    not on this host\n",
		"\nservers (namespace minecraft): 3, 1 running, 2 players online\n",
		"  database: newest " + dbbackup.BundleName(env.now.Add(-9*time.Hour), dbbackup.LabelDaily) + ", 9h0m ago; 2 bundles in " + env.w.backupDir + "\n",
		"  off-site: last good sync 2h0m ago to felis-dr\n",
		"  memory: 1.9 GiB available of 7.6 GiB\n",
		"  timer active, last run 1m ago (success)\n",
		"  alerts: 2 open (sudo felis doctor says where to look)\n" +
			"    db-backup-servers (warning, mailed 1h30m ago)\n" +
			"    disk//var (critical, seen 4m ago, not mailed yet)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q:\n%s", want, got)
		}
	}
	for prefix, want := range map[string]string{
		"felis-api": "felis-api 1/1 ready felis-api:v1.4.0@0123456789ab restarts 2",
		"NAME":      "NAME ROLE DESIRED PHASE PLAYERS NEWEST WORLD BACKUP",
		"creative":  "creative - Stopped Stopped - none",
		"lobby":     "lobby lobby Running Starting - none",
		"survival":  "survival - Running Running 2/20 3h0m ago",
	} {
		if f := strings.Join(lineFields(got, prefix), " "); f != want {
			t.Errorf("row %q = %q, want %q\n%s", prefix, f, want, got)
		}
	}
	if strings.Contains(got, "elsewhere") || strings.Contains(got, "other-1") {
		t.Errorf("status shows what is not this install's:\n%s", got)
	}
}

// Whatever is down reads as down, and the rest of the report still prints.
func TestStatusWithPartsDown(t *testing.T) {
	env := statusTestEnv(t)
	env.cl, env.clErr = nil, errors.New("connection refused")
	env.cfg = &config.Config{}
	var out bytes.Buffer
	printStatus(context.Background(), env, &out)
	for _, want := range []string{
		"cluster:  unreachable (connection refused)\n",
		"\nservers (namespace minecraft): unknown while the cluster is unreachable\n",
		"  off-site: not configured, every backup is on this machine only\n",
		"  timer active, last run",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}

	env = statusTestEnv(t)
	env.backups = func(context.Context) (map[string]time.Time, error) { return nil, errors.New("postgres is down") }
	out.Reset()
	printStatus(context.Background(), env, &out)
	if f := strings.Join(lineFields(out.String(), "survival"), " "); f != "survival - Running Running 2/20 ?" {
		t.Errorf("survival with PostgreSQL down = %q", f)
	}
	if !strings.Contains(out.String(), "  world backups unknown: postgres is down\n") {
		t.Errorf("status does not say why the backups are unknown:\n%s", out.String())
	}
}

func TestShortImage(t *testing.T) {
	for in, want := range map[string]string{
		"registry.felis.svc:5000/felis/felis-api:v1.4.0":                                       "felis-api:v1.4.0",
		"docker.io/library/postgres:17@sha256:0123456789abcdef0123":                            "postgres:17@0123456789ab",
		"felis-operator@sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba98765432": "felis-operator@fedcba987654",
		"busybox": "busybox",
	} {
		if got := shortImage(in); got != want {
			t.Errorf("shortImage(%q) = %q, want %q", in, got, want)
		}
	}
}

// A node that is not ready, a Deployment without replicas or containers, and
// lists the API server refuses each read as such.
func TestStatusClusterEdges(t *testing.T) {
	env := statusTestEnv(t)
	env.cfg = &config.Config{K8s: config.K8sConfig{Namespace: "games"}}
	env.cl = fake.NewClientBuilder().WithScheme(newSystemServerScheme(t)).WithObjects(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "felis-1"}, Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}, {Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue}}}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "felis-bare", Namespace: "felis"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "felis"}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{RestartCount: 4}}}},
	).Build()
	var out bytes.Buffer
	printStatus(context.Background(), env, &out)
	got := out.String()
	for _, want := range []string{
		"node:     felis-1 NotReady, k3s , , kernel \n",
		"\nservers (namespace games): 0, 0 running, 0 players online\n\nbackups:\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status lacks %q:\n%s", want, got)
		}
	}
	if f := strings.Join(lineFields(got, "felis-bare"), " "); f != "felis-bare 0/1 ready - restarts 0" {
		t.Errorf("row felis-bare = %q, want its one replica wanted, no image, and no pod of its own:\n%s", f, got)
	}

	failing := func(what client.ObjectList) {
		env.cl = fake.NewClientBuilder().WithScheme(newSystemServerScheme(t)).WithObjects(statusClusterObjects()...).
			WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if fmt.Sprintf("%T", list) == fmt.Sprintf("%T", what) {
					return errors.New("forbidden")
				}
				return c.List(ctx, list, opts...)
			}}).Build()
		out.Reset()
		printStatus(context.Background(), env, &out)
	}
	for _, tc := range []struct {
		list client.ObjectList
		want string
	}{
		{&corev1.NodeList{}, "cluster:  unreachable (forbidden)\n\nproxy:"},
		{&appsv1.DeploymentList{}, "\ncontrol plane (namespace felis):\n  cannot list it: forbidden\n\nproxy:"},
		{&corev1.PodList{}, "\ncontrol plane (namespace felis):\n  cannot list it: forbidden\n\nproxy:"},
		{&v1alpha1.MinecraftServerList{}, "\nservers (namespace games): cannot list them: forbidden\n\nbackups:"},
	} {
		failing(tc.list)
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("listing %T refused: status lacks %q:\n%s", tc.list, tc.want, out.String())
		}
	}
}

func TestStatusProxy(t *testing.T) {
	env := statusTestEnv(t)
	writeTestFile(t, filepath.Join(env.unitDir, "felis-velocity.service"), "[Unit]\n", 0o644)
	env.run = fakeSystemctl("", map[string]string{"felis-velocity.service": "active"})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	env.w.proxyAddr = ln.Addr().String()
	var out bytes.Buffer
	statusProxy(context.Background(), env, &out)
	if want := "\nproxy:    felis-velocity active, " + env.w.proxyAddr + " accepts connections\n"; out.String() != want {
		t.Errorf("proxy listening: %q, want %q", out.String(), want)
	}
	ln.Close()
	out.Reset()
	statusProxy(context.Background(), env, &out)
	if want := "\nproxy:    felis-velocity active, " + env.w.proxyAddr + " refuses connections (dial tcp " + env.w.proxyAddr + ": "; !strings.HasPrefix(out.String(), want) {
		t.Errorf("proxy gone: %q, want it to start %q", out.String(), want)
	}
}

func TestStatusBackups(t *testing.T) {
	env := statusTestEnv(t)
	status := func() string {
		var out bytes.Buffer
		statusBackups(env, &out)
		return out.String()
	}
	for _, tc := range []struct {
		what, dir, want string
	}{
		{"no -backup-dir", "", "  database: not checked (felis-watchdog.service names no -backup-dir)\n"},
		{"an empty directory", t.TempDir(), "  database: none in %s\n"},
		{"a file where the directory should be", filepath.Join(env.w.backupDir, dbbackup.BundleName(env.now.Add(-9*time.Hour), dbbackup.LabelDaily)), "  database: cannot read %s: "},
	} {
		env.w.backupDir = tc.dir
		want := tc.want
		if strings.Contains(want, "%s") {
			want = fmt.Sprintf(want, tc.dir)
		}
		if got := status(); !strings.Contains(got, want) {
			t.Errorf("%s: %q, want %q", tc.what, got, want)
		}
	}

	for _, tc := range []struct {
		what   string
		status *offsite.Status
		raw    string
		want   string
	}{
		{"never synced", nil, "", "  off-site: never synced\n"},
		{"never succeeded", &offsite.Status{LastAttempt: env.now.Add(-time.Hour), LastError: "403 Forbidden"}, "", "  off-site: never succeeded; last attempt 1h0m ago: 403 Forbidden\n"},
		{"the last attempt failed", &offsite.Status{LastAttempt: env.now.Add(-30 * time.Minute), LastSuccess: env.now.Add(-26 * time.Hour), LastError: "timeout", Bucket: "felis-dr"}, "",
			"  off-site: last good sync 26h0m ago to felis-dr; the last attempt, 30m ago, failed: timeout\n"},
		{"an error an earlier attempt left", &offsite.Status{LastAttempt: env.now.Add(-2 * time.Hour), LastSuccess: env.now.Add(-time.Hour), LastError: "timeout", Bucket: "felis-dr"}, "",
			"  off-site: last good sync 1h0m ago to felis-dr\n"},
		{"an unreadable record", nil, "{", "  off-site: unexpected end of JSON input\n"},
	} {
		os.Remove(env.w.offsiteStatus)
		switch {
		case tc.status != nil:
			if err := offsite.WriteStatus(env.w.offsiteStatus, *tc.status); err != nil {
				t.Fatal(err)
			}
		case tc.raw != "":
			writeTestFile(t, env.w.offsiteStatus, tc.raw, 0o600)
		}
		if got := status(); !strings.HasSuffix(got, tc.want) {
			t.Errorf("%s: %q, want it to end %q", tc.what, got, tc.want)
		}
	}
}

func TestStatusHost(t *testing.T) {
	env := statusTestEnv(t)
	dir := t.TempDir()
	env.w.diskPaths = "/nonexistent-felis-status-test," + dir + "," + dir
	env.meminfo = filepath.Join(dir, "meminfo")
	var out bytes.Buffer
	statusHost(env, &out)
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 3 || lines[0] != "" || lines[1] != "host:" || !strings.HasPrefix(lines[2], "  disk "+dir+": ") || !strings.HasSuffix(lines[2], "% free)") {
		t.Errorf("host: %q, want one line for %s (a path on a disk already shown, and one that is not there, print none) and no memory line", lines, dir)
	}

	writeTestFile(t, env.meminfo, "MemTotal: 4096 kB\ngarbage\nMemAvailable: 1024 kB\n", 0o644)
	if total, avail, ok := readMeminfo(env.meminfo); !ok || total != 4096*1024 || avail != 1024*1024 {
		t.Errorf("readMeminfo = %d, %d, %v", total, avail, ok)
	}
	writeTestFile(t, env.meminfo, "MemAvailable: 1024 kB\n", 0o644)
	if _, _, ok := readMeminfo(env.meminfo); ok {
		t.Error("readMeminfo without MemTotal: ok")
	}
}

func TestStatusWatchdog(t *testing.T) {
	env := statusTestEnv(t)
	status := func() string {
		var out bytes.Buffer
		statusWatchdog(context.Background(), env, &out)
		return out.String()
	}
	run := env.run
	env.run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "show" {
			return []byte("Result=success\nExecMainExitTimestamp=\n"), nil
		}
		return run(ctx, name, args...)
	}
	if err := watchdog.SaveState(env.w.statePath, &watchdog.State{Alerts: map[string]*watchdog.Alert{
		"proxy": {Finding: watchdog.Finding{Key: "proxy", Severity: watchdog.Critical}, FirstSeen: env.now.Add(-time.Hour), ClearedAt: env.now.Add(-time.Minute)},
	}}); err != nil {
		t.Fatal(err)
	}
	if got, want := status(), "\nwatchdog:\n  timer active\n  alerts: none open\n"; got != want {
		t.Errorf("a watchdog that has not run and has nothing open: %q, want %q", got, want)
	}

	writeTestFile(t, env.w.statePath, "{", 0o600)
	if got := status(); !strings.Contains(got, "\n  alerts: unknown (") {
		t.Errorf("an unreadable state: %q", got)
	}

	if err := os.Remove(filepath.Join(env.unitDir, "felis-watchdog.timer")); err != nil {
		t.Fatal(err)
	}
	if got, want := status(), "\nwatchdog:\n  not installed: nothing checks this host\n"; got != want {
		t.Errorf("no watchdog timer: %q, want %q", got, want)
	}
}

// Rows print by name in whatever order the API server lists them, a server
// the operator has not reconciled yet reads as dashes, and open alerts print
// by key in whatever order the state's map yields them.
func TestStatusOrder(t *testing.T) {
	env := statusTestEnv(t)
	objs := append(statusClusterObjects(),
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "felis-operator", Namespace: "felis"}},
		&v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "fresh", Namespace: "minecraft"}})
	env.cl = fake.NewClientBuilder().WithScheme(newSystemServerScheme(t)).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			// The fake lists by name; the other way round, then.
			if err := c.List(ctx, list, opts...); err != nil {
				return err
			}
			items, err := meta.ExtractList(list)
			if err != nil {
				return err
			}
			slices.Reverse(items)
			return meta.SetList(list, items)
		}}).Build()
	var out bytes.Buffer
	printStatus(context.Background(), env, &out)
	var rows []string
	for _, l := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(l); len(f) > 0 && (strings.HasPrefix(f[0], "felis-") || slices.Contains([]string{"creative", "fresh", "lobby", "survival"}, f[0])) {
			rows = append(rows, strings.Join(f, " "))
		}
	}
	want := []string{
		"felis-api 1/1 ready felis-api:v1.4.0@0123456789ab restarts 2",
		"felis-operator 0/1 ready - restarts 0",
		"creative - Stopped Stopped - none",
		"fresh - - - - none",
		"lobby lobby Running Starting - none",
		"survival - Running Running 2/20 3h0m ago",
	}
	if !slices.Equal(rows, want) {
		t.Errorf("rows %q, want %q:\n%s", rows, want, out.String())
	}

	alerts := map[string]*watchdog.Alert{}
	for i, key := range []string{"a", "b", "c", "d"} {
		alerts[key] = &watchdog.Alert{Finding: watchdog.Finding{Key: key, Severity: watchdog.Warning}, FirstSeen: env.now.Add(-time.Duration(i+1) * time.Minute)}
	}
	if err := watchdog.SaveState(env.w.statePath, &watchdog.State{Alerts: alerts}); err != nil {
		t.Fatal(err)
	}
	wantAlerts := "  alerts: 4 open (sudo felis doctor says where to look)\n" +
		"    a (warning, seen 1m ago, not mailed yet)\n    b (warning, seen 2m ago, not mailed yet)\n" +
		"    c (warning, seen 3m ago, not mailed yet)\n    d (warning, seen 4m ago, not mailed yet)\n"
	// A map starts its walk at random: fifty walks all in order by chance
	// is out of the question.
	for range 50 {
		out.Reset()
		statusWatchdog(context.Background(), env, &out)
		if !strings.HasSuffix(out.String(), wantAlerts) {
			t.Fatalf("alerts %q, want them to end %q", out.String(), wantAlerts)
		}
	}
}

// A selector the API server would have refused picks no pods.
func TestPodRestartsBadSelector(t *testing.T) {
	pods := []corev1.Pod{{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{RestartCount: 3}}}}}
	if n := podRestarts(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app", Operator: "Near"}}}, pods); n != 0 {
		t.Errorf("podRestarts with a bad selector = %d, want 0", n)
	}
	if n := podRestarts(&metav1.LabelSelector{}, pods); n != 3 {
		t.Errorf("podRestarts with a selector that picks all = %d, want 3", n)
	}
}
