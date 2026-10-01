package distributed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/archivetransfer"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/placement"
	"felis.lolicon.best/internal/restore"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const MigrationAnnotation = "felis.lolicon.best/migration"

// Admission errors keep HTTP policy in the API layer.
var ErrBusy = errors.New("server must be fully stopped with no maintenance operation")
var ErrNotFound = errors.New("migration not found")

type Node struct {
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	Ready        bool     `json:"ready"`
	Approved     bool     `json:"approved"`
	Addresses    []string `json:"addresses"`
	Architecture string   `json:"architecture"`
}

func (m *Manager) Nodes(ctx context.Context) ([]Node, error) {
	var list corev1.NodeList
	if err := m.Client.List(ctx, &list); err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(list.Items))
	for _, n := range list.Items {
		info := Node{Name: n.Name, Role: n.Labels[placement.LabelRole], Ready: placement.Online(&n), Approved: n.Labels[placement.LabelApproved] == "true" && !n.Spec.Unschedulable, Addresses: []string{}, Architecture: n.Status.NodeInfo.Architecture}
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP || a.Type == corev1.NodeExternalIP {
				info.Addresses = append(info.Addresses, a.Address)
			}
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Manager) ValidateNode(ctx context.Context, name string) error {
	return placement.Worker(ctx, m.Client, name)
}

// Operation is persisted on the CR together with its non-expiring maintenance lock.
// SourcePVC is retained even after success, and retry never changes the committed active world.
type Operation struct {
	ID         string                  `json:"id"`
	Server     string                  `json:"server"`
	State      string                  `json:"state"`
	Stage      string                  `json:"stage"`
	SourceNode string                  `json:"sourceNode"`
	TargetNode string                  `json:"targetNode"`
	SourcePVC  string                  `json:"sourcePVC"`
	TargetPVC  string                  `json:"targetPVC"`
	Backup     archivetransfer.Receipt `json:"backup"`
	Owner      string                  `json:"-"`
	Started    time.Time               `json:"started"`
	Updated    time.Time               `json:"updated"`
	Error      string                  `json:"error,omitempty"`
	Switched   bool                    `json:"switched"`
	Attempt    int                     `json:"attempt"`
}

// owner travels in persistence, but never on the public operation view.
type persistedOperation struct {
	Operation
	OwnerID string `json:"ownerId"`
}

func readOperation(s *v1alpha1.MinecraftServer) (Operation, error) {
	var stored persistedOperation
	if s.Annotations[MigrationAnnotation] == "" {
		return Operation{}, ErrNotFound
	}
	if err := json.Unmarshal([]byte(s.Annotations[MigrationAnnotation]), &stored); err != nil {
		return Operation{}, err
	}
	stored.Operation.Owner = stored.OwnerID
	return stored.Operation, nil
}

func (m *Manager) Migration(ctx context.Context, server, id string) (Operation, error) {
	var s v1alpha1.MinecraftServer
	if err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: server}, &s); err != nil {
		return Operation{}, err
	}
	op, err := readOperation(&s)
	if err == nil && id != "" && op.ID != id {
		return Operation{}, ErrNotFound
	}
	return op, err
}

func (m *Manager) quiet(ctx context.Context, s *v1alpha1.MinecraftServer, own ...string) error {
	if s.Spec.DesiredState != v1alpha1.DesiredStopped || s.Status.Phase != v1alpha1.PhaseStopped || s.Status.Ready {
		return ErrBusy
	}
	var pods corev1.PodList
	if err := m.Client.List(ctx, &pods, client.InNamespace(m.Namespace), client.MatchingLabels{v1alpha1.LabelServer: s.Name}); err != nil {
		return err
	}
	// Even terminal maintenance Pods must have exited before a new attempt writes its PVC.
	for _, p := range pods.Items {
		allowed := len(own) > 0 && p.Labels[MigrationAnnotation] == own[0]
		if p.Labels[v1alpha1.LabelComponent] == "server" || (!allowed && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed) {
			return ErrBusy
		}
	}
	return nil
}

func (m *Manager) BeginMigration(ctx context.Context, server, target, owner string) (Operation, error) {
	if err := m.ValidateNode(ctx, target); err != nil {
		return Operation{}, err
	}
	var op Operation
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var s v1alpha1.MinecraftServer
		if err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: server}, &s); err != nil {
			return err
		}
		if previous, err := readOperation(&s); err == nil && previous.State != "succeeded" {
			if previous.TargetNode == target {
				op = previous
				return nil
			}
			return ErrBusy
		} else if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if s.Spec.ReaperExempt || s.Labels[v1alpha1.LabelSystemRole] != "" {
			return ErrBusy
		}
		if err := m.quiet(ctx, &s); err != nil {
			return err
		}
		var jobs batchv1.JobList
		if err := m.Client.List(ctx, &jobs, client.InNamespace(m.Namespace), client.MatchingLabels{maintenance.LabelServer: server}); err != nil {
			return err
		}
		if _, held := maintenance.Holder(server, s.Annotations, jobs.Items, time.Now()); held {
			return ErrBusy
		}
		world, err := m.Resolve(ctx, server)
		if err != nil {
			return err
		}
		source := world.Node
		if source == "" {
			source = m.Controller
		}
		if source == target {
			return errors.New("source and target nodes are identical")
		}
		// Bound local-path worlds have a physical node; reject a guessed or mismatched source.
		var pvc corev1.PersistentVolumeClaim
		if err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: world.Claim}, &pvc); err != nil {
			return err
		}
		if pvc.Spec.VolumeName == "" {
			return errors.New("source world is not bound")
		}
		if err := m.volumeOnNode(ctx, pvc.Spec.VolumeName, source); err != nil {
			return err
		}
		id := archivetransfer.ID()
		now := time.Now().UTC()
		op = Operation{ID: id, Server: server, State: "backing_up", Stage: "backing_up", SourceNode: source, TargetNode: target, SourcePVC: world.Claim, TargetPVC: "world-" + server + "-m" + id[:12], Owner: owner, Started: now, Updated: now}
		return m.save(ctx, &s, op, true)
	})
	return op, err
}

func (m *Manager) volumeOnNode(ctx context.Context, volume, node string) error {
	var pv corev1.PersistentVolume
	if err := m.Client.Get(ctx, types.NamespacedName{Name: volume}, &pv); err != nil {
		return err
	}
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return errors.New("migration requires a node-local volume with node affinity")
	}
	var n corev1.Node
	if err := m.Client.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
		return err
	}
	for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		if len(term.MatchFields) > 0 {
			continue
		}
		matched := len(term.MatchExpressions) > 0
		for _, e := range term.MatchExpressions {
			if e.Operator != corev1.NodeSelectorOpIn {
				matched = false
				break
			}
			found := false
			for _, v := range e.Values {
				if n.Labels[e.Key] == v {
					found = true
				}
			}
			if !found {
				matched = false
				break
			}
		}
		if matched {
			return nil
		}
	}
	return errors.New("world volume is not on the recorded execution node")
}

func (m *Manager) save(ctx context.Context, s *v1alpha1.MinecraftServer, op Operation, lock bool) error {
	before := s.DeepCopy()
	op.Updated = time.Now().UTC()
	raw, _ := json.Marshal(persistedOperation{Operation: op, OwnerID: op.Owner})
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	s.Annotations[MigrationAnnotation] = string(raw)
	if lock {
		s.Annotations[maintenance.Annotation] = maintenance.LockValue(maintenance.KindMigration, op.Started)
	} else {
		delete(s.Annotations, maintenance.Annotation)
	}
	return m.Client.Patch(ctx, s, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (m *Manager) RetryMigration(ctx context.Context, server, id string) (Operation, error) {
	var op Operation
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var s v1alpha1.MinecraftServer
		if err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: server}, &s); err != nil {
			return err
		}
		var err error
		op, err = readOperation(&s)
		if err != nil {
			return err
		}
		if op.ID != id {
			return ErrNotFound
		}
		if op.State != "failed" {
			return ErrBusy
		}
		if err := m.quiet(ctx, &s); err != nil {
			return err
		}
		if err := m.ValidateNode(ctx, op.TargetNode); err != nil {
			return err
		}
		op.State = op.Stage
		op.Error = ""
		op.Attempt++
		return m.save(ctx, &s, op, true)
	})
	return op, err
}

func (m *Manager) ReconcileMigrations(ctx context.Context) error {
	var servers v1alpha1.MinecraftServerList
	if err := m.Client.List(ctx, &servers, client.InNamespace(m.Namespace)); err != nil {
		return err
	}
	var errs []error
	for i := range servers.Items {
		s := &servers.Items[i]
		op, err := readOperation(s)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if op.State == "succeeded" {
			if err := m.expireMigrationJobs(ctx, op.ID); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if op.State == "failed" {
			continue
		}
		err = m.advance(ctx, s, &op)
		if err != nil {
			if apierrors.IsConflict(err) {
				continue
			}
			op.Stage = op.State
			op.State = "failed"
			op.Error = err.Error()
			if saveErr := m.save(ctx, s, op, true); saveErr != nil {
				errs = append(errs, saveErr)
			}
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) advance(ctx context.Context, s *v1alpha1.MinecraftServer, op *Operation) error {
	if err := m.quiet(ctx, s, op.ID); err != nil {
		return err
	}
	if s.Annotations[maintenance.Annotation] != maintenance.LockValue(maintenance.KindMigration, op.Started) {
		return errors.New("persistent migration lock was changed")
	}
	if err := m.ValidateNode(ctx, op.TargetNode); err != nil {
		return err
	}
	if !op.Switched && (s.WorldPVC() != op.SourcePVC || (s.Spec.NodeName != "" && s.Spec.NodeName != op.SourceNode)) {
		return errors.New("source placement changed during migration")
	}
	var source corev1.Node
	if !op.Switched {
		if err := m.Client.Get(ctx, types.NamespacedName{Name: op.SourceNode}, &source); err != nil {
			return err
		}
		if !placement.Online(&source) {
			return errors.New("source node is offline")
		}
	}
	name := fmt.Sprintf("migration-%s-%d", op.ID[:16], op.Attempt)
	switch op.State {
	case "backing_up":
		var j batchv1.Job
		err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: name + "-backup"}, &j)
		if apierrors.IsNotFound(err) {
			job, t, err := m.uploadJob(op.Server, op.SourcePVC, op.SourceNode, name+"-backup")
			if err != nil {
				return err
			}
			raw, _ := json.Marshal(t)
			job.Annotations = map[string]string{archivetransfer.Annotation: string(raw)}
			job.Labels[MigrationAnnotation] = op.ID
			job.Spec.Template.Labels[MigrationAnnotation] = op.ID
			job.Spec.TTLSecondsAfterFinished = nil
			return m.Client.Create(ctx, job)
		}
		if err != nil {
			return err
		}
		var t archivetransfer.Ticket
		if err = json.Unmarshal([]byte(j.Annotations[archivetransfer.Annotation]), &t); err != nil {
			return err
		}
		rec, err := m.Archive.Receipt(ctx, t.ID)
		if err != nil {
			if maintenance.JobFinished(&j) {
				return fmt.Errorf("migration backup has no durable receipt: %w", err)
			}
			return nil
		}
		if rec.Ref != t.Ref || rec.SHA256 == "" {
			return errors.New("migration backup receipt mismatch")
		}
		if m.Record == nil {
			return errors.New("backup recorder unavailable")
		}
		if err = m.Record(ctx, Backup{ID: t.ID, Server: op.Server, Owner: op.Owner, Reason: "pre_restore", Receipt: rec}); err != nil {
			return err
		}
		op.Backup = rec
		op.State = "restoring"
		op.Stage = op.State
		return m.save(ctx, s, *op, true)
	case "restoring":
		if err := m.ensureTarget(ctx, s, op); err != nil {
			return err
		}
		var j batchv1.Job
		err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: name + "-restore"}, &j)
		if apierrors.IsNotFound(err) {
			p := restore.JobParams{Server: op.Server, WorldPVC: op.TargetPVC, BackupRef: op.Backup.Ref, ArchiveStore: "tarLocal", Namespace: m.Namespace, ServiceAccount: "felis-restore", Image: m.Image, WorldsRoot: "/world", BackupRoot: m.Archive.Root, Deadline: 2 * time.Hour}
			p, err = m.restoreParams(ctx, p)
			if err != nil {
				return err
			}
			if p.SHA256 != op.Backup.SHA256 {
				return errors.New("migration archive digest changed")
			}
			job, err := restore.RestoreJob(p)
			if err != nil {
				return err
			}
			job.Name = name + "-restore"
			m.pin(&job.Spec.Template.Spec, op.TargetNode)
			job.Labels[LabelTransfer] = "true"
			job.Spec.Template.Labels[LabelTransfer] = "true"
			job.Labels[MigrationAnnotation] = op.ID
			job.Spec.Template.Labels[MigrationAnnotation] = op.ID
			job.Spec.TTLSecondsAfterFinished = nil
			return m.Client.Create(ctx, job)
		}
		if err != nil {
			return err
		}
		if !maintenance.JobFinished(&j) {
			return nil
		}
		if !jobSucceeded(&j) {
			return errors.New("target restore or read-back verification failed")
		}
		// Job Complete alone is not enough for a RWO handoff: all its processes must be gone.
		if err := m.quiet(ctx, s); err != nil {
			return nil
		}
		var pvc corev1.PersistentVolumeClaim
		if err = m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: op.TargetPVC}, &pvc); err != nil {
			return err
		}
		if err = m.volumeOnNode(ctx, pvc.Spec.VolumeName, op.TargetNode); err != nil {
			return err
		}
		op.State = "switching"
		op.Stage = op.State
		return m.save(ctx, s, *op, true)
	case "switching":
		var sts appsv1.StatefulSet
		err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: s.Name}, &sts)
		if err == nil {
			if sts.Spec.Replicas != nil && *sts.Spec.Replicas != 0 {
				return ErrBusy
			}
			if sts.Status.Replicas != 0 {
				return nil
			}
			policy := metav1.DeletePropagationForeground
			return m.Client.Delete(ctx, &sts, &client.DeleteOptions{PropagationPolicy: &policy, Preconditions: &metav1.Preconditions{UID: &sts.UID, ResourceVersion: &sts.ResourceVersion}})
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
		// One optimistic CR write commits node, claim and progress. No failure can roll this back.
		target := s.DeepCopy()
		committed := *op
		target.Spec.NodeName = op.TargetNode
		target.Spec.Storage.ClaimName = op.TargetPVC
		committed.Switched = true
		committed.State = "succeeded"
		committed.Stage = "succeeded"
		committed.Updated = time.Now().UTC()
		raw, _ := json.Marshal(persistedOperation{Operation: committed, OwnerID: op.Owner})
		target.Annotations[MigrationAnnotation] = string(raw)
		delete(target.Annotations, maintenance.Annotation)
		if err := m.Client.Patch(ctx, target, client.MergeFromWithOptions(s.DeepCopy(), client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		*s, *op = *target, committed
		return nil
	default:
		return fmt.Errorf("unknown migration stage %q", op.State)
	}
}

func (m *Manager) ensureTarget(ctx context.Context, s *v1alpha1.MinecraftServer, op *Operation) error {
	var pvc corev1.PersistentVolumeClaim
	err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: op.TargetPVC}, &pvc)
	if err == nil {
		if pvc.Labels[MigrationAnnotation] != op.ID {
			return errors.New("target PVC identity mismatch")
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	var source corev1.PersistentVolumeClaim
	if err = m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: op.SourcePVC}, &source); err != nil {
		return err
	}
	size := source.Spec.Resources.Requests[corev1.ResourceStorage]
	if size.IsZero() {
		size = resource.MustParse("8Gi")
	}
	pvc = corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: op.TargetPVC, Namespace: m.Namespace, Labels: map[string]string{maintenance.LabelServer: s.Name, MigrationAnnotation: op.ID}}, Spec: corev1.PersistentVolumeClaimSpec{AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, StorageClassName: source.Spec.StorageClassName, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}}}}
	return m.Client.Create(ctx, &pvc)
}

func (m *Manager) expireMigrationJobs(ctx context.Context, id string) error {
	var jobs batchv1.JobList
	if err := m.Client.List(ctx, &jobs, client.InNamespace(m.Namespace), client.MatchingLabels{MigrationAnnotation: id}); err != nil {
		return err
	}
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if j.Spec.TTLSecondsAfterFinished != nil || !maintenance.JobFinished(j) {
			continue
		}
		before := j.DeepCopy()
		ttl := int32(600)
		j.Spec.TTLSecondsAfterFinished = &ttl
		if err := m.Client.Patch(ctx, j, client.MergeFrom(before)); err != nil {
			return err
		}
	}
	return nil
}
