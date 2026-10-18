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
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/placement"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// failoverFixture is the migration fixture with alice running on b, b gone, and an archive
// store that knows any backup it is asked about.
func failoverFixture(t *testing.T) (*Manager, context.Context) {
	t.Helper()
	m, ctx := fixture(t)
	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inspect" {
			http.NotFound(w, r)
			return
		}
		var body struct{ Ref string }
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(archivetransfer.Receipt{Ref: body.Ref, SHA256: strings.Repeat("b", 64), Size: 70})
	}))
	t.Cleanup(store.Close)
	m.Archive.URL = store.URL
	s := getServer(t, m, ctx)
	s.Spec.DesiredState = v1alpha1.DesiredRunning
	s.Annotations = map[string]string{v1alpha1.AnnotationLiveBackup: `{"job":"backup-alice-x"}`}
	if err := m.Client.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	s.Status.Phase, s.Status.Ready, s.Status.NodeName = v1alpha1.PhaseRunning, true, "b"
	if err := m.Client.Status().Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	setReady(t, m, "b", false)
	return m, ctx
}

func setReady(t *testing.T, m *Manager, name string, ready bool) {
	t.Helper()
	var n corev1.Node
	if err := m.Client.Get(context.Background(), types.NamespacedName{Name: name}, &n); err != nil {
		t.Fatal(err)
	}
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionUnknown
	}
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status, LastTransitionTime: metav1.NewTime(time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC))}}
	if err := m.Client.Status().Update(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
}

// fenceNode does what node-control's fence task does to the node.
func fenceNode(t *testing.T, m *Manager, name string) {
	t.Helper()
	var n corev1.Node
	if err := m.Client.Get(context.Background(), types.NamespacedName{Name: name}, &n); err != nil {
		t.Fatal(err)
	}
	n.Spec.Unschedulable = true
	delete(n.Labels, placement.LabelApproved)
	n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: TaintOutOfService, Value: "nodeshutdown", Effect: corev1.TaintEffectNoExecute})
	if err := m.Client.Update(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
}

func reconcile(t *testing.T, m *Manager, ctx context.Context) {
	t.Helper()
	if err := m.ReconcileMigrations(ctx); err != nil {
		t.Fatal(err)
	}
}

func worldJob(name, node string, finished bool) *batchv1.Job {
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "minecraft", Labels: map[string]string{maintenance.LabelServer: "alice"}},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{NodeSelector: map[string]string{placement.LabelIdentity: node}}}}}
	if finished {
		j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	}
	return j
}

func TestFailoverRestoresTheBackupOnAnotherNode(t *testing.T) {
	m, ctx := failoverFixture(t)
	game := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "alice-0", Namespace: "minecraft", Labels: map[string]string{v1alpha1.LabelServer: "alice", v1alpha1.LabelComponent: "server"}}, Spec: corev1.PodSpec{NodeName: "b"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	stuck, done := worldJob("backup-alice-stuck", "b", false), worldJob("backup-alice-done", "b", true)
	for _, o := range []client.Object{game, stuck, done} {
		if err := m.Client.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	done.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := m.Client.Status().Update(ctx, done); err != nil {
		t.Fatal(err)
	}

	op, err := m.BeginFailover(ctx, "alice", "c", "owner", archivetransfer.Receipt{Ref: "/backups/alice-1.tar.gz", Size: 70})
	if err != nil {
		t.Fatal(err)
	}
	if op.Kind != KindFailover || op.State != "fencing" || op.SourceNode != "b" || op.Resume != "Running" || op.TargetPVC == op.SourcePVC {
		t.Fatalf("begun = %+v", op)
	}
	s := getServer(t, m, ctx)
	if s.Spec.DesiredState != v1alpha1.DesiredStopped || s.Annotations[v1alpha1.AnnotationLiveBackup] != "" {
		t.Fatalf("server after begin: desired %s, annotations %v", s.Spec.DesiredState, s.Annotations)
	}
	if kind, held := maintenance.Holder(s.Name, s.Annotations, nil, time.Now()); !held || kind != maintenance.KindMigration {
		t.Fatal("the failover holds no world lock")
	}
	if again, err := m.BeginFailover(ctx, "alice", "c", "owner", archivetransfer.Receipt{Ref: "/backups/other.tar.gz"}); err != nil || again.ID != op.ID {
		t.Fatalf("asking again = %+v (%v)", again, err)
	}

	// Not fenced: the stage waits, but the stuck Job pinned to the lost node goes now.
	reconcile(t, m, ctx)
	if cur, _ := m.Migration(ctx, "alice", op.ID); cur.State != "fencing" {
		t.Fatalf("before the fence: %+v", cur)
	}
	if err := m.Client.Get(ctx, client.ObjectKeyFromObject(stuck), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("the unfinished Job on the lost node survived: %v", err)
	}
	if err := m.Client.Get(ctx, client.ObjectKeyFromObject(done), &batchv1.Job{}); err != nil {
		t.Errorf("a finished Job was deleted: %v", err)
	}
	if pending, err := m.FencePending(ctx); err != nil || strings.Join(pending, ",") != "b" {
		t.Fatalf("pending before the fence = %v (%v)", pending, err)
	}

	// Fenced, but the old pod is still there: still waiting, and still asking for a fence.
	fenceNode(t, m, "b")
	reconcile(t, m, ctx)
	if cur, _ := m.Migration(ctx, "alice", op.ID); cur.State != "fencing" {
		t.Fatalf("with the old pod left: %+v", cur)
	}
	if pending, _ := m.FencePending(ctx); strings.Join(pending, ",") != "b" {
		t.Fatalf("a pod left on the fenced node is not pending: %v", pending)
	}
	if err := m.Client.Delete(ctx, game); err != nil {
		t.Fatal(err)
	}
	s = getServer(t, m, ctx)
	s.Status.Phase, s.Status.Ready = v1alpha1.PhaseStopped, false
	if err := m.Client.Status().Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	if pending, _ := m.FencePending(ctx); len(pending) != 0 {
		t.Fatalf("pending after the fence = %v", pending)
	}

	reconcile(t, m, ctx) // fencing → restoring
	reconcile(t, m, ctx) // the archive store's digest recorded
	reconcile(t, m, ctx) // the target claim and the restore Job
	cur, _ := m.Migration(ctx, "alice", op.ID)
	if cur.State != "restoring" || cur.Backup.SHA256 != strings.Repeat("b", 64) {
		t.Fatalf("restoring = %+v", cur)
	}
	var jobs batchv1.JobList
	if err := m.Client.List(ctx, &jobs, client.MatchingLabels{MigrationAnnotation: op.ID}); err != nil || len(jobs.Items) != 1 {
		t.Fatalf("restore Jobs = %d (%v)", len(jobs.Items), err)
	}
	restore := &jobs.Items[0]
	if restore.Spec.Template.Spec.NodeSelector[placement.LabelIdentity] != "c" || !strings.HasSuffix(restore.Name, "-restore") {
		t.Fatalf("restore Job %s pinned to %v", restore.Name, restore.Spec.Template.Spec.NodeSelector)
	}
	assertJobIsolation(t, restore)
	var pvc corev1.PersistentVolumeClaim
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: op.TargetPVC}, &pvc); err != nil {
		t.Fatal(err)
	}
	pvc.Spec.VolumeName = "target"
	if err := m.Client.Update(ctx, &pvc); err != nil {
		t.Fatal(err)
	}
	if err := m.Client.Create(ctx, localPV("target", "c")); err != nil {
		t.Fatal(err)
	}
	restore.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := m.Client.Status().Update(ctx, restore); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		reconcile(t, m, ctx)
	}
	s = getServer(t, m, ctx)
	if s.Spec.NodeName != "c" || s.WorldPVC() != op.TargetPVC || s.Spec.DesiredState != v1alpha1.DesiredRunning {
		t.Fatalf("after the switch: node %q claim %q desired %s", s.Spec.NodeName, s.WorldPVC(), s.Spec.DesiredState)
	}
	if _, held := maintenance.Holder(s.Name, s.Annotations, nil, time.Now()); held {
		t.Fatal("the switch kept the world lock")
	}
	if cur, _ := m.Migration(ctx, "alice", op.ID); cur.State != "succeeded" || !cur.Switched {
		t.Fatalf("finished = %+v", cur)
	}
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: op.SourcePVC}, &pvc); err != nil {
		t.Errorf("the old world's claim was removed: %v", err)
	}
}

func TestFailoverAdmission(t *testing.T) {
	m, ctx := fixture(t)
	backup := archivetransfer.Receipt{Ref: "/backups/alice-1.tar.gz"}
	if _, err := m.BeginFailover(ctx, "alice", "c", "owner", backup); !errors.Is(err, ErrNodeNotLost) {
		t.Fatalf("off an online node = %v", err)
	}
	migration, err := m.BeginMigration(ctx, "alice", "c", "owner")
	if err != nil {
		t.Fatal(err)
	}
	setReady(t, m, "b", false)
	if _, err := m.BeginFailover(ctx, "alice", "b", "owner", backup); err == nil {
		t.Fatal("a failover onto the lost node itself was begun")
	}
	if _, err := m.BeginFailover(ctx, "alice", "c", "owner", archivetransfer.Receipt{}); err == nil {
		t.Fatal("a failover without a backup was begun")
	}
	op, err := m.BeginFailover(ctx, "alice", "c", "owner", backup)
	if err != nil {
		t.Fatal(err)
	}
	if op.ID == migration.ID || op.Kind != KindFailover || op.Resume != "" {
		t.Fatalf("the failover over a migration = %+v", op)
	}
	if err := m.Client.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "d", Labels: map[string]string{placement.LabelIdentity: "d", placement.LabelRole: placement.RoleWorker, placement.LabelApproved: "true"}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.BeginFailover(ctx, "alice", "d", "owner", backup); !errors.Is(err, ErrBusy) {
		t.Fatalf("redirecting an underway failover = %v", err)
	}

	// A failover that failed may go elsewhere.
	s := getServer(t, m, ctx)
	op.State, op.Error = "failed", "the target went away"
	if err := m.save(ctx, s, op, true); err != nil {
		t.Fatal(err)
	}
	again, err := m.BeginFailover(ctx, "alice", "d", "owner", backup)
	if err != nil || again.ID == op.ID || again.TargetNode != "d" {
		t.Fatalf("after a failed failover = %+v (%v)", again, err)
	}
}

func TestFailoverStopsWhenTheServerIsStarted(t *testing.T) {
	m, ctx := failoverFixture(t)
	op, err := m.BeginFailover(ctx, "alice", "c", "owner", archivetransfer.Receipt{Ref: "/backups/alice-1.tar.gz"})
	if err != nil {
		t.Fatal(err)
	}
	s := getServer(t, m, ctx)
	s.Spec.DesiredState = v1alpha1.DesiredRunning
	if err := m.Client.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	reconcile(t, m, ctx)
	if cur, _ := m.Migration(ctx, "alice", op.ID); cur.State != "failed" || cur.Stage != "fencing" || !strings.Contains(cur.Error, "started") {
		t.Fatalf("after a start = %+v", cur)
	}
}

func TestFailoverTargetAndLostWorkers(t *testing.T) {
	m, ctx := failoverFixture(t)
	controller := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a", Labels: map[string]string{placement.LabelIdentity: "a", placement.LabelRole: placement.RoleController}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	cordoned := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "d", Labels: map[string]string{placement.LabelIdentity: "d", placement.LabelRole: placement.RoleWorker, placement.LabelApproved: "true"}}, Spec: corev1.NodeSpec{Unschedulable: true}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	early := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "0w", Labels: map[string]string{placement.LabelIdentity: "0w", placement.LabelRole: placement.RoleWorker, placement.LabelApproved: "true"}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	unassigned := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "x"}}
	onA := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "minecraft"}}
	onEarly := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "dave", Namespace: "minecraft"}, Spec: v1alpha1.MinecraftServerSpec{NodeName: "0w"}}
	lobby := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: "minecraft", Labels: map[string]string{v1alpha1.LabelSystemRole: "lobby"}}, Spec: v1alpha1.MinecraftServerSpec{NodeName: "c"}}
	for _, o := range []client.Object{controller, cordoned, early, unassigned, onA, onEarly, lobby} {
		if err := m.Client.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	// a and 0w hold one world each, c only a system server: c is the least loaded.
	if got, err := m.FailoverTarget(ctx); err != nil || got != "c" {
		t.Fatalf("target = %q (%v), want c", got, err)
	}
	carol := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "carol", Namespace: "minecraft"}, Spec: v1alpha1.MinecraftServerSpec{NodeName: "c"}}
	if err := m.Client.Create(ctx, carol); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.FailoverTarget(ctx); got != "a" {
		t.Fatalf("on a tie = %q, want the controller", got)
	}
	erin := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "erin", Namespace: "minecraft"}}
	if err := m.Client.Create(ctx, erin); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.FailoverTarget(ctx); got != "0w" {
		t.Fatalf("on a tie between workers = %q, want 0w", got)
	}
	setReady(t, m, "c", false)
	setReady(t, m, "a", false)
	setReady(t, m, "0w", false)
	if _, err := m.FailoverTarget(ctx); err == nil {
		t.Fatal("a target was found with every other node down or cordoned")
	}

	fenceNode(t, m, "0w")
	lost, up, err := m.LostNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, counted := lost["x"]; up || counted || len(lost) != 2 || !lost["b"].Equal(time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("lost = %v, controller online = %v", lost, up)
	}
	setReady(t, m, "a", true)
	if _, up, _ := m.LostNodes(ctx); !up {
		t.Fatal("the online controller reads as down")
	}
}

func TestNodesReportFenceAndLoss(t *testing.T) {
	m, ctx := failoverFixture(t)
	fenceNode(t, m, "b")
	nodes, err := m.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		switch n.Name {
		case "b":
			if !n.Fenced || n.LostSince == nil || n.Ready || n.Approved {
				t.Errorf("lost b = %+v", n)
			}
		case "c":
			if n.Fenced || n.LostSince != nil {
				t.Errorf("online c = %+v", n)
			}
		}
	}
	if err := m.Client.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a", Labels: map[string]string{placement.LabelIdentity: "a", placement.LabelRole: placement.RoleController}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}); err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateNode(ctx, "a"); err != nil {
		t.Errorf("the controller is refused as a target: %v", err)
	}
	setReady(t, m, "a", false)
	if err := m.ValidateNode(ctx, "a"); err == nil {
		t.Error("an offline controller is accepted as a target")
	}
	if err := m.ValidateNode(ctx, "b"); err == nil {
		t.Error("the lost worker is accepted as a target")
	}
}

func TestFenced(t *testing.T) {
	taint := corev1.Taint{Key: TaintOutOfService, Value: "nodeshutdown", Effect: corev1.TaintEffectNoExecute}
	node := func(unschedulable bool, approved string, taints ...corev1.Taint) *corev1.Node {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "b", Labels: map[string]string{}}, Spec: corev1.NodeSpec{Unschedulable: unschedulable, Taints: taints}}
		if approved != "" {
			n.Labels[placement.LabelApproved] = approved
		}
		return n
	}
	for name, tc := range map[string]struct {
		n    *corev1.Node
		want bool
	}{
		"fenced":                           {node(true, "", taint), true},
		"still schedulable":                {node(false, "", taint), false},
		"still approved":                   {node(true, "true", taint), false},
		"no taint":                         {node(true, ""), false},
		"taint that only stops scheduling": {node(true, "", corev1.Taint{Key: TaintOutOfService, Effect: corev1.TaintEffectNoSchedule}), false},
		"another NoExecute taint":          {node(true, "", corev1.Taint{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoExecute}), false},
	} {
		if got := Fenced(tc.n); got != tc.want {
			t.Errorf("%s: Fenced = %v, want %v", name, got, tc.want)
		}
	}
}

func TestServersOnListsTheLostNodesWorlds(t *testing.T) {
	m, ctx := failoverFixture(t)
	ran := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "minecraft"}}
	elsewhere := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "carol", Namespace: "minecraft"}, Spec: v1alpha1.MinecraftServerSpec{NodeName: "c"}}
	lobby := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "lobby", Namespace: "minecraft", Labels: map[string]string{v1alpha1.LabelSystemRole: "lobby"}}, Spec: v1alpha1.MinecraftServerSpec{NodeName: "b"}}
	for _, o := range []client.Object{ran, elsewhere, lobby} {
		if err := m.Client.Create(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	ran.Status.NodeName = "b"
	if err := m.Client.Status().Update(ctx, ran); err != nil {
		t.Fatal(err)
	}
	op, err := m.BeginFailover(ctx, "alice", "c", "owner", archivetransfer.Receipt{Ref: "/backups/alice-1.tar.gz"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.ServersOn(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Name != "alice" || got[1].Name != "bob" {
		t.Fatalf("servers on b = %+v", got)
	}
	if got[0].Operation == nil || got[0].Operation.ID != op.ID || got[0].DesiredState != "Stopped" || got[1].Operation != nil {
		t.Fatalf("alice = %+v, bob = %+v", got[0], got[1])
	}
}

func TestFailoverDropsAMigrationsLeftovers(t *testing.T) {
	m, ctx := fixture(t)
	migration, err := m.BeginMigration(ctx, "alice", "c", "owner")
	if err != nil {
		t.Fatal(err)
	}
	reconcile(t, m, ctx) // the migration's backup Job
	var jobs batchv1.JobList
	if err := m.Client.List(ctx, &jobs, client.MatchingLabels{MigrationAnnotation: migration.ID}); err != nil || len(jobs.Items) != 1 {
		t.Fatalf("migration Jobs = %d (%v)", len(jobs.Items), err)
	}
	setReady(t, m, "b", false)
	if _, err := m.BeginFailover(ctx, "alice", "c", "owner", archivetransfer.Receipt{Ref: "/backups/alice-1.tar.gz"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Client.List(ctx, &jobs, client.MatchingLabels{MigrationAnnotation: migration.ID}); err != nil || len(jobs.Items) != 0 {
		t.Fatalf("the migration's Jobs outlived it: %d (%v)", len(jobs.Items), err)
	}
}

func TestFailoverFailsWhenItsGroundShifts(t *testing.T) {
	for name, shift := range map[string]func(*testing.T, *Manager){
		"lock": func(t *testing.T, m *Manager) {
			s := getServer(t, m, context.Background())
			s.Annotations[maintenance.Annotation] = maintenance.LockValue(maintenance.KindRestore, time.Now())
			if err := m.Client.Update(context.Background(), s); err != nil {
				t.Fatal(err)
			}
		},
		"target": func(t *testing.T, m *Manager) { setReady(t, m, "c", false) },
	} {
		t.Run(name, func(t *testing.T) {
			m, ctx := failoverFixture(t)
			op, err := m.BeginFailover(ctx, "alice", "c", "owner", archivetransfer.Receipt{Ref: "/backups/alice-1.tar.gz"})
			if err != nil {
				t.Fatal(err)
			}
			shift(t, m)
			reconcile(t, m, ctx)
			if cur, _ := m.Migration(ctx, "alice", op.ID); cur.State != "failed" {
				t.Fatalf("after the %s changed: %+v", name, cur)
			}
		})
	}
}

func TestFailoverWithoutTheOldClaim(t *testing.T) {
	m, ctx := failoverFixture(t)
	s := getServer(t, m, ctx)
	s.Spec.Storage.Size, s.Spec.Storage.StorageClassName = "3Gi", "fast"
	if err := m.Client.Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	op, err := m.BeginFailover(ctx, "alice", "c", "owner", archivetransfer.Receipt{Ref: "/backups/alice-1.tar.gz", SHA256: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Client.Delete(ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: op.SourcePVC, Namespace: "minecraft"}}); err != nil {
		t.Fatal(err)
	}
	fenceNode(t, m, "b")
	s = getServer(t, m, ctx)
	s.Status.Phase, s.Status.Ready = v1alpha1.PhaseStopped, false
	if err := m.Client.Status().Update(ctx, s); err != nil {
		t.Fatal(err)
	}
	reconcile(t, m, ctx)
	reconcile(t, m, ctx)
	var pvc corev1.PersistentVolumeClaim
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: "minecraft", Name: op.TargetPVC}, &pvc); err != nil {
		cur, _ := m.Migration(ctx, "alice", op.ID)
		t.Fatalf("no target claim (%v); failover %+v", err, cur)
	}
	if size := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; size.String() != "3Gi" || pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast" {
		t.Fatalf("target claim = %+v", pvc.Spec)
	}
}

func TestFailoverWaitsForTheFence(t *testing.T) {
	m, ctx := fixture(t)
	setReady(t, m, "b", false)
	op, err := m.BeginFailover(ctx, "alice", "c", "owner", archivetransfer.Receipt{Ref: "/backups/alice-1.tar.gz"})
	if err != nil {
		t.Fatal(err)
	}
	reconcile(t, m, ctx)
	if cur, _ := m.Migration(ctx, "alice", op.ID); cur.State != "fencing" {
		t.Fatalf("restoring before the fence: %+v", cur)
	}
	if pending, _ := m.FencePending(ctx); strings.Join(pending, ",") != "b" {
		t.Fatalf("pending = %v, want b", pending)
	}
	fenceNode(t, m, "b")
	if pending, _ := m.FencePending(ctx); len(pending) != 0 {
		t.Fatalf("pending after the fence = %v", pending)
	}
	reconcile(t, m, ctx)
	if cur, _ := m.Migration(ctx, "alice", op.ID); cur.State != "restoring" {
		t.Fatalf("after the fence: %+v", cur)
	}
}

func TestReplicaCandidatesAreRunningWorkerWorlds(t *testing.T) {
	m, ctx := fixture(t)
	running := func(name, node string, mutate ...func(*v1alpha1.MinecraftServer)) *v1alpha1.MinecraftServer {
		s := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "minecraft"}, Spec: v1alpha1.MinecraftServerSpec{DesiredState: v1alpha1.DesiredRunning, NodeName: node}}
		for _, f := range mutate {
			f(s)
		}
		return s
	}
	servers := []*v1alpha1.MinecraftServer{
		running("bob", "c"),
		running("carol", ""), // on the controller
		running("dave", "a"), // on the controller by name
		running("erin", "c", func(s *v1alpha1.MinecraftServer) { s.Labels = map[string]string{v1alpha1.LabelSystemRole: "lobby"} }),
		running("frank", "c", func(s *v1alpha1.MinecraftServer) { s.Spec.DesiredState = v1alpha1.DesiredStopped }),
		running("grace", "c"), // starting: not ready yet
		running("heidi", "c", func(s *v1alpha1.MinecraftServer) {
			s.Annotations = map[string]string{MigrationAnnotation: `{"id":"x","state":"restoring"}`}
		}),
		running("ivan", "c", func(s *v1alpha1.MinecraftServer) {
			s.Annotations = map[string]string{MigrationAnnotation: `{"id":"x","state":"succeeded"}`}
		}),
	}
	for _, s := range servers {
		if err := m.Client.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		s.Status.Phase, s.Status.Ready = v1alpha1.PhaseRunning, s.Name != "grace"
		if err := m.Client.Status().Update(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := m.ReplicaCandidates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "bob,ivan" {
		t.Fatalf("candidates = %v, want bob,ivan", got)
	}
}

func TestControllerHAFailsOverALostController(t *testing.T) {
	m, ctx := fixture(t)
	controller := func(name string, ready bool) *corev1.Node {
		status := corev1.ConditionTrue
		if !ready {
			status = corev1.ConditionUnknown
		}
		return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{placement.LabelIdentity: name, placement.LabelRole: placement.RoleController}},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status, LastTransitionTime: metav1.NewTime(time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC))}}}}
	}
	for _, n := range []*corev1.Node{controller("a", false), controller("a2", true)} {
		if err := m.Client.Create(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.LostNode(ctx, "a"); !errors.Is(err, ErrNodeNotLost) {
		t.Fatalf("a lost controller without HA = %v", err)
	}
	m.HA = true
	if _, err := m.LostNode(ctx, "a"); err != nil {
		t.Fatalf("a lost controller with HA = %v", err)
	}
	if err := m.ValidateNode(ctx, "a2"); err != nil {
		t.Errorf("the second controller is refused as a target: %v", err)
	}
	lost, up, err := m.LostNodes(ctx)
	if err != nil || !up || len(lost) != 1 || lost["a"].IsZero() {
		t.Fatalf("lost = %v, controller up = %v (%v)", lost, up, err)
	}

	setReady(t, m, "a2", false)
	if _, err := m.LostNode(ctx, "a"); !errors.Is(err, ErrNodeNotLost) {
		t.Fatalf("the last controller standing could be failed over: %v", err)
	}

	bob := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Name: "bob", Namespace: "minecraft"}, Spec: v1alpha1.MinecraftServerSpec{DesiredState: v1alpha1.DesiredRunning}}
	if err := m.Client.Create(ctx, bob); err != nil {
		t.Fatal(err)
	}
	bob.Status.Phase, bob.Status.Ready = v1alpha1.PhaseRunning, true
	if err := m.Client.Status().Update(ctx, bob); err != nil {
		t.Fatal(err)
	}
	if got, _ := m.ReplicaCandidates(ctx); strings.Join(got, ",") != "bob" {
		t.Errorf("replicas with HA = %v, want bob on the home controller", got)
	}
	m.HA = false
	if got, _ := m.ReplicaCandidates(ctx); len(got) != 0 {
		t.Errorf("replicas without HA = %v", got)
	}
	if _, err := m.LostNode(ctx, "missing"); !apierrors.IsNotFound(err) {
		t.Errorf("an unknown node = %v", err)
	}
}
