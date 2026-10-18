package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/placement"
	"felis.lolicon.best/internal/platform"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func haScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func haTestNode(name, role string, ready bool) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{placement.LabelIdentity: name}}}
	if role != "" {
		n.Labels[placement.LabelRole] = role
	}
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionUnknown
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}
	return n
}

func postgresPod(node string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "felis-postgres-x", Namespace: "felis", Labels: map[string]string{platform.LabelComponent: platform.ComponentPostgres}}, Spec: corev1.PodSpec{NodeName: node}}
}

func TestHostLeaderIsWherePostgresRuns(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		objs []client.Object
		want string
	}{
		"database on c":           {[]client.Object{haTestNode("a", "controller", true), haTestNode("c", "controller", true), haTestNode("w", "worker", true), postgresPod("c")}, "c"},
		"database on a lost node": {[]client.Object{haTestNode("a", "controller", false), haTestNode("c", "controller", true), haTestNode("b", "controller", true), postgresPod("a")}, "b"},
		"database not placed":     {[]client.Object{haTestNode("c", "controller", true), haTestNode("w", "worker", true), postgresPod("")}, "c"},
		"single node, no roles":   {[]client.Object{haTestNode("host", "", true), postgresPod("host")}, "host"},
		"single node, no pod yet": {[]client.Object{haTestNode("host", "", true)}, "host"},
	} {
		cl := fake.NewClientBuilder().WithScheme(haScheme(t)).WithObjects(tc.objs...).Build()
		got, err := hostLeader(ctx, cl, "felis")
		if err != nil || got != tc.want {
			t.Errorf("%s: leader = %q (%v), want %q", name, got, err, tc.want)
		}
	}
	gone := postgresPod("c")
	now := metav1.Now()
	gone.DeletionTimestamp, gone.Finalizers = &now, []string{"x"}
	cl := fake.NewClientBuilder().WithScheme(haScheme(t)).WithObjects(haTestNode("a", "controller", true), haTestNode("c", "controller", true), gone).Build()
	if got, _ := hostLeader(ctx, cl, "felis"); got != "a" {
		t.Errorf("with the database's pod going away = %q, want a", got)
	}
	cl = fake.NewClientBuilder().WithScheme(haScheme(t)).WithObjects(haTestNode("a", "controller", false), haTestNode("w", "worker", true)).Build()
	if _, err := hostLeader(ctx, cl, "felis"); err == nil {
		t.Error("a leader was named with every controller down")
	}
}

func TestHALeaderVerdict(t *testing.T) {
	var out bytes.Buffer
	down := errors.New("connection refused")
	for _, tc := range []struct {
		self, leader  string
		err           error
		orUnreachable bool
		want          int
	}{
		{"a", "a", nil, false, 0},
		{"b", "a", nil, false, 1},
		{"b", "a", nil, true, 1},
		{"a", "", down, true, 0},
		{"a", "", down, false, 1},
	} {
		if got := haLeaderVerdict(tc.self, tc.leader, tc.err, tc.orUnreachable, false, &out); got != tc.want {
			t.Errorf("%+v = %d, want %d", tc, got, tc.want)
		}
	}
	out.Reset()
	haLeaderVerdict("a", "a", nil, false, true, &out)
	if out.Len() != 0 {
		t.Errorf("-q printed %q", out.String())
	}
}

func TestSelfNodeName(t *testing.T) {
	dir := t.TempDir()
	dropin := filepath.Join(dir, "50-felis.yaml")
	if err := os.WriteFile(dropin, []byte("write-kubeconfig-mode: \"0600\"\nnode-name: \"ctl-b\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := selfNodeName(dropin); got != "ctl-b" {
		t.Errorf("from the drop-in = %q", got)
	}
	host, _ := os.Hostname()
	if got := selfNodeName(filepath.Join(dir, "missing")); got != strings.ToLower(host) {
		t.Errorf("fallback = %q, want %q", got, strings.ToLower(host))
	}
}

func TestCopyTreeForAVolume(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(from, "base"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(from, "base", "data"), []byte("rows"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(from, "ro"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(to, "stale"), []byte("half a copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := copyTree(from, to, copyOptions{}); err == nil {
		t.Fatal("a plain copy wrote into a non-empty target")
	}
	st, err := copyTree(from, to, copyOptions{owner: true, replace: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.files != 1 {
		t.Errorf("copied %d files", st.files)
	}
	if _, err := os.Stat(filepath.Join(to, "stale")); !os.IsNotExist(err) {
		t.Error("the stale entry survived the replace")
	}
	if raw, _ := os.ReadFile(filepath.Join(to, "base", "data")); string(raw) != "rows" {
		t.Errorf("data = %q", raw)
	}
	if fi, err := os.Stat(filepath.Join(to, "ro")); err != nil || fi.Mode().Perm() != 0o555 {
		t.Errorf("ro mode = %v (%v), want 0555", fi.Mode().Perm(), err)
	}
	os.Chmod(filepath.Join(to, "ro"), 0o755)
	os.Chmod(filepath.Join(from, "ro"), 0o755)
}

// haCluster is a single controller about to turn on HA: the control plane running, the
// lobby running on its world, a user server that stays put.
func haCluster(t *testing.T) (client.Client, *[]*batchv1.Job) {
	cl, jobs, _ := haClusterFailing(t)
	return cl, jobs
}

// haClusterFailing is haCluster whose copy Job named by the returned string fails.
func haClusterFailing(t *testing.T) (client.Client, *[]*batchv1.Job, *string) {
	t.Helper()
	one := int32(1)
	deploy := func(ns, name string) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: appsv1.DeploymentSpec{Replicas: &one}}
	}
	pvc := func(ns, name, size string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: corev1.PersistentVolumeClaimSpec{Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}}}}
	}
	lobby := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: "minecraft", Labels: map[string]string{v1alpha1.LabelSystemRole: "lobby"}}, Spec: v1alpha1.MinecraftServerSpec{DesiredState: v1alpha1.DesiredRunning}}
	survival := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "survival", Namespace: "minecraft"}, Spec: v1alpha1.MinecraftServerSpec{DesiredState: v1alpha1.DesiredRunning}}
	// login was created on the replicated class already: nothing to move.
	login := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "login", Namespace: "minecraft", Labels: map[string]string{v1alpha1.LabelSystemRole: "login"}}}
	replicated := pvc("minecraft", login.WorldPVC(), "1Gi")
	class := platform.DefaultReplicatedStorageClass
	replicated.Spec.StorageClassName = &class
	var jobs []*batchv1.Job
	fail := new(string)
	cl := fake.NewClientBuilder().WithScheme(haScheme(t)).WithObjects(
		deploy("felis", platform.SAOperator), deploy("felis", platform.SAAPI), deploy("felis", "registry"), deploy("felis", platform.PostgresName), deploy("minecraft", platform.ArchiveName),
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: "felis-reaper", Namespace: "minecraft"}},
		pvc("felis", platform.UploadsPVCName, "5Gi"), pvc("felis", "registry", "20Gi"), pvc("minecraft", "felis-backups", "30Gi"), pvc("minecraft", lobby.WorldPVC(), "2Gi"), pvc("minecraft", survival.WorldPVC(), "8Gi"),
		lobby, survival, login, replicated,
	).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if j, ok := obj.(*batchv1.Job); ok {
			cond := batchv1.JobComplete
			if j.Name == *fail {
				cond = batchv1.JobFailed
			}
			j.Status.Conditions = []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue}}
			jobs = append(jobs, j.DeepCopy())
		}
		return c.Create(ctx, obj, opts...)
	}}).Build()
	return cl, &jobs, fail
}

func haMover(cl client.Client, dir string) *haMigration {
	return &haMigration{cl: cl, controlNS: "felis", mcNS: "minecraft", backupPVC: "felis-backups", class: platform.DefaultReplicatedStorageClass, node: "a", image: "felis:1", dbSize: "10Gi", dbPath: dir, stateDir: dir, poll: time.Millisecond, timeout: time.Second, out: io.Discard}
}

func TestMigrateStorageMovesTheControlPlane(t *testing.T) {
	ctx := context.Background()
	cl, jobs := haCluster(t)
	dir := t.TempDir()
	if err := haMover(cl, dir).run(ctx); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"felis/felis-postgres-ha": "10Gi", "felis/felis-uploads-ha": "5Gi", "felis/registry-ha": "20Gi", "minecraft/felis-backups-ha": "30Gi", "minecraft/world-lobby-ha": "2Gi"}
	for key, size := range want {
		ns, name, _ := strings.Cut(key, "/")
		var pvc corev1.PersistentVolumeClaim
		if err := cl.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &pvc); err != nil {
			t.Errorf("%s: %v", key, err)
			continue
		}
		if q := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; q.String() != size || *pvc.Spec.StorageClassName != platform.DefaultReplicatedStorageClass {
			t.Errorf("%s = %s on %v", key, q.String(), *pvc.Spec.StorageClassName)
		}
	}
	if len(*jobs) != len(want) {
		t.Fatalf("%d copy Jobs, want %d", len(*jobs), len(want))
	}
	for _, j := range *jobs {
		spec := j.Spec.Template.Spec
		if spec.NodeSelector["kubernetes.io/hostname"] != "a" || spec.Containers[0].Image != "felis:1" || strings.Join(spec.Containers[0].Command[1:3], " ") != "ha copy-volume" {
			t.Errorf("job %s = %+v", j.Name, spec)
		}
		src := spec.Volumes[0]
		if j.Name == "ha-copy-felis-postgres-ha" {
			if src.HostPath == nil || src.HostPath.Path == "" {
				t.Errorf("the database copy reads %+v", src)
			}
		} else if src.PersistentVolumeClaim == nil || !src.PersistentVolumeClaim.ReadOnly {
			t.Errorf("job %s reads %+v writable", j.Name, src)
		}
		if caps := spec.Containers[0].SecurityContext.Capabilities; len(caps.Add) != 3 || caps.Drop[0] != "ALL" {
			t.Errorf("job %s capabilities = %+v", j.Name, caps)
		}
	}
	for _, d := range []struct{ ns, name string }{{"felis", platform.SAAPI}, {"felis", platform.PostgresName}, {"minecraft", platform.ArchiveName}} {
		var dep appsv1.Deployment
		if err := cl.Get(ctx, types.NamespacedName{Namespace: d.ns, Name: d.name}, &dep); err != nil || *dep.Spec.Replicas != 0 {
			t.Errorf("%s not stopped: %v", d.name, err)
		}
	}
	var cron batchv1.CronJob
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "felis-reaper"}, &cron); err != nil || cron.Spec.Suspend == nil || !*cron.Spec.Suspend {
		t.Error("the reaper was not suspended")
	}
	var lobby, survival v1alpha1.MinecraftServer
	cl.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "lobby"}, &lobby)
	cl.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "survival"}, &survival)
	if lobby.Spec.Storage.ClaimName != "world-lobby-ha" || lobby.Spec.Storage.StorageClassName != platform.DefaultReplicatedStorageClass || lobby.Spec.DesiredState != v1alpha1.DesiredRunning {
		t.Errorf("lobby = %+v", lobby.Spec)
	}
	if survival.Spec.Storage.ClaimName != "" || survival.Spec.DesiredState != v1alpha1.DesiredRunning {
		t.Errorf("a user server was touched: %+v", survival.Spec)
	}
	var login v1alpha1.MinecraftServer
	cl.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: "login"}, &login)
	if login.Spec.Storage.ClaimName != "" {
		t.Errorf("a world already replicated was moved: %+v", login.Spec)
	}

	*jobs = nil
	if err := haMover(cl, dir).run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(*jobs) != 0 {
		t.Fatalf("a second run copied again: %d Jobs", len(*jobs))
	}
}

func TestMigrateStorageResumes(t *testing.T) {
	ctx := context.Background()
	cl, jobs, fail := haClusterFailing(t)
	dir := t.TempDir()
	*fail = "ha-copy-registry-ha"
	m := haMover(cl, dir)
	if err := m.run(ctx); err == nil || !strings.Contains(err.Error(), "registry-ha") {
		t.Fatalf("a failed copy = %v", err)
	}
	st, _ := haMover(cl, dir).loadState()
	if !st.Copied["felis/felis-postgres-ha"] || st.Copied["felis/registry-ha"] || st.Desired["lobby"] != "Running" {
		t.Fatalf("state after the failure = %+v", st)
	}
	*fail = ""
	*jobs = nil
	if err := haMover(cl, dir).run(ctx); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, j := range *jobs {
		names = append(names, j.Name)
	}
	if strings.Contains(strings.Join(names, ","), "postgres") || !strings.Contains(strings.Join(names, ","), "ha-copy-registry-ha") {
		t.Fatalf("the resumed run copied %v", names)
	}
}

func TestCSIMountDirFindsTheReplicatedVolume(t *testing.T) {
	root := t.TempDir()
	mount := filepath.Join(root, "pods", "uid-1", "volumes", "kubernetes.io~csi", "pvc-123", "mount")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := csiMountDir(root, "pvc-123"); err != nil || got != mount {
		t.Fatalf("mounted = %q (%v)", got, err)
	}
	if _, err := csiMountDir(root, "pvc-other"); err == nil {
		t.Error("a volume mounted nowhere here was found")
	}
}

func TestMigrateStorageOnAFreshInstall(t *testing.T) {
	cl, jobs := haCluster(t)
	m := haMover(cl, t.TempDir())
	m.dbPath = filepath.Join(t.TempDir(), "missing")
	if err := m.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, j := range *jobs {
		if j.Name == "ha-copy-felis-postgres-ha" {
			t.Fatal("a missing database directory was copied")
		}
	}
}
