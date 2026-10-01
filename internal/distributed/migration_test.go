package distributed

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/archivetransfer"
	"felis.lolicon.best/internal/backup"
	"felis.lolicon.best/internal/backupjob"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/placement"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func fixture(t *testing.T) (*Manager, context.Context) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, batchv1.AddToScheme, appsv1.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	node := func(name string) *corev1.Node {
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{placement.LabelIdentity: name, placement.LabelRole: placement.RoleWorker, placement.LabelApproved: "true", "kubernetes.io/hostname": name}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	}
	s := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "minecraft"}, Spec: v1alpha1.MinecraftServerSpec{DesiredState: v1alpha1.DesiredStopped, NodeName: "b"}, Status: v1alpha1.MinecraftServerStatus{Phase: v1alpha1.PhaseStopped}}
	s.Spec.Storage.Size = "1Gi"
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: s.WorldPVC(), Namespace: "minecraft"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "source", Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
	zero := int32(0)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(s, &batchv1.Job{}).WithObjects(node("b"), node("c"), s, pvc, localPV("source", "b"), &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "minecraft"}, Spec: appsv1.StatefulSetSpec{Replicas: &zero}}, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "minecraft"}, Spec: corev1.ServiceSpec{ClusterIP: "10.43.0.80"}}).Build()
	var receipt archivetransfer.Receipt
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/inspect" && r.Method == "POST" || strings.HasPrefix(r.URL.Path, "/receipts/") {
			var jobs batchv1.JobList
			cl.List(r.Context(), &jobs)
			for _, j := range jobs.Items {
				var ticket archivetransfer.Ticket
				if json.Unmarshal([]byte(j.Annotations[archivetransfer.Annotation]), &ticket) == nil && ticket.Ref != "" {
					receipt = archivetransfer.Receipt{Ref: ticket.Ref, SHA256: strings.Repeat("a", 64), Size: 50}
				}
			}
			if receipt.Ref == "" {
				http.NotFound(w, r)
				return
			}
			json.NewEncoder(w).Encode(receipt)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	m := &Manager{Client: cl, Namespace: "minecraft", Controller: "a", Image: "registry.local/felis:1", Archive: archivetransfer.Client{Root: "/backups", URL: server.URL, Key: strings.Repeat("k", 32)}, Record: func(context.Context, Backup) error { return nil }}
	m.Resolve = placement.Resolve(cl, "minecraft")
	return m, context.Background()
}

func localPV(name, node string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.PersistentVolumeSpec{NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}}}}}}}}
}

func getServer(t *testing.T, m *Manager, ctx context.Context) *v1alpha1.MinecraftServer {
	t.Helper()
	var s v1alpha1.MinecraftServer
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: "alice"}, &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

func TestMigrationSurvivesRestartAndKeepsIdentity(t *testing.T) {
	m, ctx := fixture(t)
	op, err := m.BeginMigration(ctx, "alice", "c", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := m.BeginMigration(ctx, "alice", "c", "owner"); err != nil || again.ID != op.ID {
		t.Fatalf("duplicate %+v: %v", again, err)
	}
	s := getServer(t, m, ctx)
	if kind, held := maintenance.Holder(s.Name, s.Annotations, nil, time.Now().Add(365*24*time.Hour)); !held || kind != maintenance.KindMigration {
		t.Fatal("migration lock expired")
	}
	if _, err := m.BeginMigration(ctx, "alice", "b", "owner"); !errors.Is(err, ErrBusy) {
		t.Fatalf("competing migration: %v", err)
	}
	// A restart reconstructs the coordinator solely from persisted CR and Job state.
	restarted := *m
	m = &restarted
	for i := 0; i < 3; i++ {
		if err := m.ReconcileMigrations(ctx); err != nil {
			t.Fatal(err)
		}
	}
	current, err := m.Migration(ctx, "alice", op.ID)
	if err != nil || current.State != "restoring" {
		t.Fatalf("progress %+v: %v", current, err)
	}
	var pvc corev1.PersistentVolumeClaim
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: op.TargetPVC}, &pvc); err != nil {
		t.Fatal(err)
	}
	pvc.Spec.VolumeName = "target"
	if err := m.Client.Update(ctx, &pvc); err != nil {
		t.Fatal(err)
	}
	if err := m.Client.Create(ctx, localPV("target", "c")); err != nil {
		t.Fatal(err)
	}
	var jobs batchv1.JobList
	if err := m.Client.List(ctx, &jobs); err != nil {
		t.Fatal(err)
	}
	for i := range jobs.Items {
		j := &jobs.Items[i]
		assertJobIsolation(t, j)
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
		if err := m.Client.Status().Update(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := m.ReconcileMigrations(ctx); err != nil {
			t.Fatal(err)
		}
	}
	s = getServer(t, m, ctx)
	if s.Spec.NodeName != "c" || s.WorldPVC() != op.TargetPVC || s.Spec.DesiredState != v1alpha1.DesiredStopped || s.Status.Ready {
		t.Fatalf("unsafe switch %+v", s)
	}
	if _, held := maintenance.Holder(s.Name, s.Annotations, nil, time.Now()); held {
		t.Fatal("success did not release lock")
	}
	var svc corev1.Service
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: "alice"}, &svc); err != nil || svc.Spec.ClusterIP != "10.43.0.80" {
		t.Fatalf("service changed: %v", err)
	}
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: op.SourcePVC}, &pvc); err != nil {
		t.Fatal("source PVC removed", err)
	}
	var sts appsv1.StatefulSet
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: "alice"}, &sts); !apierrors.IsNotFound(err) {
		t.Fatal("stopped immutable StatefulSet not removed", err)
	}
}

func assertJobIsolation(t *testing.T, j *batchv1.Job) {
	t.Helper()
	p := j.Spec.Template.Spec
	if p.AutomountServiceAccountToken == nil || *p.AutomountServiceAccountToken || p.HostNetwork || p.HostPID || p.HostIPC {
		t.Fatal("maintenance has host credentials/namespaces")
	}
	claims := 0
	for _, v := range p.Volumes {
		if v.Secret != nil || v.HostPath != nil {
			t.Fatal("maintenance carries secret or host mount")
		}
		if v.PersistentVolumeClaim != nil {
			claims++
		}
	}
	if claims != 1 {
		t.Fatalf("cross-node job mounts %d PVCs", claims)
	}
	for _, c := range p.Containers {
		if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
			t.Fatal("maintenance can escalate")
		}
		for _, e := range c.Env {
			if strings.Contains(e.Name, "DATABASE") || e.Name == archivetransfer.KeyEnv {
				t.Fatal("full credentials leaked")
			}
		}
	}
}

func TestLostSourceFailsClosedAndRetryKeepsSource(t *testing.T) {
	m, ctx := fixture(t)
	op, err := m.BeginMigration(ctx, "alice", "c", "owner")
	if err != nil {
		t.Fatal(err)
	}
	var n corev1.Node
	m.Client.Get(ctx, types.NamespacedName{Name: "b"}, &n)
	n.Status.Conditions = nil
	if err := m.Client.Status().Update(ctx, &n); err != nil {
		t.Fatal(err)
	}
	if err := m.ReconcileMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	failed, err := m.Migration(ctx, "alice", op.ID)
	if err != nil || failed.State != "failed" {
		t.Fatalf("offline source %+v: %v", failed, err)
	}
	s := getServer(t, m, ctx)
	if s.WorldPVC() != op.SourcePVC || s.Spec.NodeName != "b" {
		t.Fatal("failed migration switched placement")
	}
	if _, held := maintenance.Holder(s.Name, s.Annotations, nil, time.Now().Add(time.Hour)); !held {
		t.Fatal("failed migration lost lock")
	}
	if _, err := m.RetryMigration(ctx, "alice", op.ID); err != nil {
		t.Fatal(err)
	}
	if err := m.ReconcileMigrations(ctx); err != nil {
		t.Fatal(err)
	}
	if current, _ := m.Migration(ctx, "alice", op.ID); current.State != "failed" {
		t.Fatal("retry started tasks on offline source")
	}
}

func TestMigrationRejectsLiveProcessAndUnapprovedTarget(t *testing.T) {
	m, ctx := fixture(t)
	if err := m.Client.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "alice-0", Namespace: m.Namespace, Labels: map[string]string{v1alpha1.LabelServer: "alice", v1alpha1.LabelComponent: "server"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BeginMigration(ctx, "alice", "c", "owner"); !errors.Is(err, ErrBusy) {
		t.Fatal("live source accepted", err)
	}
	var n corev1.Node
	m.Client.Get(ctx, types.NamespacedName{Name: "c"}, &n)
	delete(n.Labels, placement.LabelApproved)
	m.Client.Update(ctx, &n)
	if err := m.ValidateNode(ctx, "c"); err == nil {
		t.Fatal("unapproved worker accepted")
	}
}

func TestMigrationFailureRetainsSourceAndCanRetry(t *testing.T) {
	for _, stage := range []string{"restoring", "switching"} {
		t.Run(stage, func(t *testing.T) {
			m, ctx := fixture(t)
			op, err := m.BeginMigration(ctx, "alice", "c", "owner")
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				if err := m.ReconcileMigrations(ctx); err != nil {
					t.Fatal(err)
				}
			}
			var jobs batchv1.JobList
			if err := m.Client.List(ctx, &jobs); err != nil {
				t.Fatal(err)
			}
			for i := range jobs.Items {
				j := &jobs.Items[i]
				condition := batchv1.JobComplete
				if stage == "restoring" && strings.HasSuffix(j.Name, "-restore") {
					condition = batchv1.JobFailed
				}
				j.Status.Conditions = []batchv1.JobCondition{{Type: condition, Status: corev1.ConditionTrue}}
				if err := m.Client.Status().Update(ctx, j); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "switching" {
				var pvc corev1.PersistentVolumeClaim
				if err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: op.TargetPVC}, &pvc); err != nil {
					t.Fatal(err)
				}
				pvc.Spec.VolumeName = "target"
				if err := m.Client.Update(ctx, &pvc); err != nil {
					t.Fatal(err)
				}
				if err := m.Client.Create(ctx, localPV("target", "c")); err != nil {
					t.Fatal(err)
				}
				fail := true
				m.Client = interceptor.NewClient(m.Client.(client.WithWatch), interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if s, ok := obj.(*v1alpha1.MinecraftServer); ok && s.Spec.NodeName == "c" && fail {
							fail = false
							return errors.New("commit interrupted")
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
				})
			}
			for i := 0; i < 3; i++ {
				if err := m.ReconcileMigrations(ctx); err != nil {
					t.Fatal(err)
				}
			}
			failed, err := m.Migration(ctx, "alice", op.ID)
			if err != nil || failed.State != "failed" || failed.Stage != stage || failed.Switched || failed.Error == "" {
				t.Fatalf("failure %+v: %v", failed, err)
			}
			s := getServer(t, m, ctx)
			if s.WorldPVC() != op.SourcePVC || s.Spec.NodeName != "b" || s.Spec.DesiredState != v1alpha1.DesiredStopped {
				t.Fatal("failed operation changed active source")
			}
			if _, held := maintenance.Holder(s.Name, s.Annotations, nil, time.Now().Add(30*24*time.Hour)); !held {
				t.Fatal("failed operation released lock")
			}
			archiver := &RemoteArchiver{TarLocal: &backup.TarLocal{BackupRoot: t.TempDir()}, Manager: m}
			if err := archiver.Delete(ctx, backup.ArchiveRef(failed.Backup.Ref)); !errors.Is(err, ErrBusy) {
				t.Fatalf("failed migration lost its safety archive: %v", err)
			}
			if retry, err := m.RetryMigration(ctx, "alice", op.ID); err != nil || retry.State != stage || retry.Attempt != 1 {
				t.Fatalf("retry %+v: %v", retry, err)
			}
			if err := m.ReconcileMigrations(ctx); err != nil {
				t.Fatal(err)
			}
			if stage == "switching" {
				if done, _ := m.Migration(ctx, "alice", op.ID); done.State != "succeeded" || !done.Switched {
					t.Fatalf("commit retry %+v", done)
				}
			}
		})
	}
}

func TestBackupResolvesActiveClaimAndPreservesConflictContract(t *testing.T) {
	m, ctx := fixture(t)
	p := backupjob.JobParams{Server: "alice", WorldPVC: "stale-claim", JobName: "manual-backup"}
	if err := m.CreateBackupJob(ctx, p); err != nil {
		t.Fatal(err)
	}
	var j batchv1.Job
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: p.JobName}, &j); err != nil {
		t.Fatal(err)
	}
	assertJobIsolation(t, &j)
	for _, v := range j.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != getServer(t, m, ctx).WorldPVC() {
			t.Fatal("backup used a stale world claim")
		}
	}
	if err := m.CreateBackupJob(ctx, p); !errors.Is(err, backupjob.ErrAlreadyExists) {
		t.Fatalf("duplicate backup: %v", err)
	}
}
