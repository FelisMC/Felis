package distributed

import (
	"context"
	"errors"
	"sort"
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
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Failover restores a lost node's worlds from backups after fencing, retaining the old claims.

const (
	KindFailover = "failover"
	// TaintOutOfService permits Pod deletion and volume detachment without waiting for the lost kubelet.
	TaintOutOfService = "node.kubernetes.io/out-of-service"
	AnnotationFenced  = "felis.lolicon.best/fenced"
)

var ErrNodeNotLost = errors.New("only an offline worker can be failed over")

func Fenced(n *corev1.Node) bool {
	if !n.Spec.Unschedulable || n.Labels[placement.LabelApproved] == "true" {
		return false
	}
	for _, t := range n.Spec.Taints {
		if t.Key == TaintOutOfService && t.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}

func lostSince(n *corev1.Node) *time.Time {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue && !c.LastTransitionTime.IsZero() {
			t := c.LastTransitionTime.UTC()
			return &t
		}
	}
	return nil
}

func (m *Manager) placedOn(s *v1alpha1.MinecraftServer) string {
	switch {
	case s.Spec.NodeName != "":
		return s.Spec.NodeName
	case s.Status.NodeName != "":
		return s.Status.NodeName
	}
	return m.Controller
}

// LostNode accepts offline workers, or controllers when HA leaves another controller online.
func (m *Manager) LostNode(ctx context.Context, name string) (*corev1.Node, error) {
	var nodes corev1.NodeList
	if err := m.Client.List(ctx, &nodes); err != nil {
		return nil, err
	}
	var lost *corev1.Node
	others := false
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Name == name {
			lost = n
		} else if n.Labels[placement.LabelRole] == placement.RoleController && placement.Admitted(n, m.Controller) {
			others = true
		}
	}
	if lost == nil {
		return nil, apierrors.NewNotFound(corev1.Resource("nodes"), name)
	}
	if placement.Online(lost) {
		return nil, ErrNodeNotLost
	}
	switch lost.Labels[placement.LabelRole] {
	case placement.RoleWorker:
		return lost, nil
	case placement.RoleController:
		if m.HA && others {
			return lost, nil
		}
	}
	return nil, ErrNodeNotLost
}

type PlacedServer struct {
	Name         string
	DesiredState string
	Operation    *Operation
}

func (m *Manager) ServersOn(ctx context.Context, node string) ([]PlacedServer, error) {
	var list v1alpha1.MinecraftServerList
	if err := m.Client.List(ctx, &list, client.InNamespace(m.Namespace)); err != nil {
		return nil, err
	}
	var out []PlacedServer
	for i := range list.Items {
		s := &list.Items[i]
		if s.Labels[v1alpha1.LabelSystemRole] != "" || m.placedOn(s) != node {
			continue
		}
		p := PlacedServer{Name: s.Name, DesiredState: string(s.Spec.DesiredState)}
		if op, err := readOperation(s); err == nil && !op.Settled() {
			p.Operation = &op
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// BeginFailover is idempotent for the same target; failed operations may retry on another target.
func (m *Manager) BeginFailover(ctx context.Context, server, target, owner string, backup archivetransfer.Receipt) (Operation, error) {
	if backup.Ref == "" {
		return Operation{}, errors.New("a failover needs a backup to restore")
	}
	if err := m.ValidateNode(ctx, target); err != nil {
		return Operation{}, err
	}
	var op Operation
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var s v1alpha1.MinecraftServer
		if err := m.Client.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: server}, &s); err != nil {
			return err
		}
		if s.Labels[v1alpha1.LabelSystemRole] != "" {
			return ErrBusy
		}
		source := m.placedOn(&s)
		if _, err := m.LostNode(ctx, source); err != nil {
			return err
		}
		previous, err := readOperation(&s)
		switch {
		case err == nil && !previous.Settled():
			if previous.Kind == KindFailover && previous.TargetNode == target {
				op = previous
				return nil
			}
			if previous.Kind == KindFailover && previous.State != "failed" {
				return ErrBusy
			}
			// Discard unfinished work that depends on the lost source or a failed target.
			if err := m.discard(ctx, previous); err != nil {
				return err
			}
		case err != nil && !errors.Is(err, ErrNotFound):
			return err
		}
		id := archivetransfer.ID()
		now := time.Now().UTC()
		op = Operation{ID: id, Kind: KindFailover, Server: server, State: "fencing", Stage: "fencing", SourceNode: source, TargetNode: target, SourcePVC: s.WorldPVC(), TargetPVC: "world-" + server + "-f" + id[:12], Backup: backup, Owner: owner, Started: now, Updated: now}
		if s.Spec.DesiredState == v1alpha1.DesiredRunning {
			op.Resume = string(v1alpha1.DesiredRunning)
		}
		before := s.DeepCopy()
		s.Spec.DesiredState = v1alpha1.DesiredStopped
		// A live backup frozen on the lost node has nothing left to thaw.
		delete(s.Annotations, v1alpha1.AnnotationLiveBackup)
		return m.saveFrom(ctx, before, &s, op, true)
	})
	return op, err
}

// fence removes unfinished maintenance Jobs and waits for node fencing and Pod removal.
func (m *Manager) fence(ctx context.Context, s *v1alpha1.MinecraftServer, op *Operation) error {
	if s.Annotations[maintenance.Annotation] != maintenance.LockValue(maintenance.KindMigration, op.Started) {
		return errors.New("persistent migration lock was changed")
	}
	if s.Spec.DesiredState != v1alpha1.DesiredStopped {
		return errors.New("the server was started during the failover")
	}
	if err := m.ValidateNode(ctx, op.TargetNode); err != nil {
		return err
	}
	var jobs batchv1.JobList
	if err := m.Client.List(ctx, &jobs, client.InNamespace(m.Namespace), client.MatchingLabels{maintenance.LabelServer: op.Server}); err != nil {
		return err
	}
	background := metav1.DeletePropagationBackground
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if maintenance.JobFinished(j) {
			continue
		}
		if err := m.Client.Delete(ctx, j, &client.DeleteOptions{PropagationPolicy: &background}); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	var node corev1.Node
	if err := m.Client.Get(ctx, types.NamespacedName{Name: op.SourceNode}, &node); client.IgnoreNotFound(err) != nil {
		return err
	} else if err == nil && !Fenced(&node) {
		return nil
	}
	if err := m.quiet(ctx, s, op.ID); errors.Is(err, ErrBusy) {
		return nil
	} else if err != nil {
		return err
	}
	op.State = "restoring"
	op.Stage = op.State
	return m.save(ctx, s, *op, true)
}

// FencePending includes nodes with remaining server Pods even if they are already fenced.
func (m *Manager) FencePending(ctx context.Context) ([]string, error) {
	var servers v1alpha1.MinecraftServerList
	if err := m.Client.List(ctx, &servers, client.InNamespace(m.Namespace)); err != nil {
		return nil, err
	}
	pending := map[string]bool{}
	for i := range servers.Items {
		op, err := readOperation(&servers.Items[i])
		if err != nil || op.Kind != KindFailover || op.State != "fencing" || pending[op.SourceNode] {
			continue
		}
		var node corev1.Node
		if err := m.Client.Get(ctx, types.NamespacedName{Name: op.SourceNode}, &node); client.IgnoreNotFound(err) != nil {
			return nil, err
		} else if err != nil {
			continue
		}
		if !Fenced(&node) {
			pending[op.SourceNode] = true
			continue
		}
		var pods corev1.PodList
		if err := m.Client.List(ctx, &pods, client.InNamespace(m.Namespace), client.MatchingLabels{v1alpha1.LabelServer: op.Server}); err != nil {
			return nil, err
		}
		for _, p := range pods.Items {
			if p.Spec.NodeName == op.SourceNode {
				pending[op.SourceNode] = true
				break
			}
		}
	}
	out := make([]string, 0, len(pending))
	for n := range pending {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

var errFailoverTarget = errors.New("no online node can take the worlds")

// FailoverTarget prefers the fewest user worlds, breaking ties by controller role then name.
func (m *Manager) FailoverTarget(ctx context.Context) (string, error) {
	var nodes corev1.NodeList
	if err := m.Client.List(ctx, &nodes); err != nil {
		return "", err
	}
	var servers v1alpha1.MinecraftServerList
	if err := m.Client.List(ctx, &servers, client.InNamespace(m.Namespace)); err != nil {
		return "", err
	}
	load := map[string]int{}
	for i := range servers.Items {
		if servers.Items[i].Labels[v1alpha1.LabelSystemRole] == "" {
			load[m.placedOn(&servers.Items[i])]++
		}
	}
	var candidates []string
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if !n.Spec.Unschedulable && placement.Admitted(n, m.Controller) {
			candidates = append(candidates, n.Name)
		}
	}
	if len(candidates) == 0 {
		return "", errFailoverTarget
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		switch {
		case load[a] != load[b]:
			return load[a] < load[b]
		case (a == m.Controller) != (b == m.Controller):
			return a == m.Controller
		}
		return a < b
	})
	return candidates[0], nil
}

func (m *Manager) LostNodes(ctx context.Context) (lost map[string]time.Time, controllerOnline bool, err error) {
	var nodes corev1.NodeList
	if err := m.Client.List(ctx, &nodes); err != nil {
		return nil, false, err
	}
	lost = map[string]time.Time{}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		controller := n.Name == m.Controller || n.Labels[placement.LabelRole] == placement.RoleController
		if controller && placement.Admitted(n, m.Controller) {
			controllerOnline = true
			continue
		}
		if placement.Online(n) || Fenced(n) || (n.Labels[placement.LabelRole] != placement.RoleWorker && !(controller && m.HA)) {
			continue
		}
		since := time.Time{}
		if t := lostSince(n); t != nil {
			since = *t
		}
		lost[n.Name] = since
	}
	return lost, controllerOnline, nil
}

func (m *Manager) ReplicaCandidates(ctx context.Context) ([]string, error) {
	var list v1alpha1.MinecraftServerList
	if err := m.Client.List(ctx, &list, client.InNamespace(m.Namespace)); err != nil {
		return nil, err
	}
	var out []string
	for i := range list.Items {
		s := &list.Items[i]
		if s.Labels[v1alpha1.LabelSystemRole] != "" || (m.placedOn(s) == m.Controller && !m.HA) {
			continue
		}
		if s.Spec.DesiredState != v1alpha1.DesiredRunning || s.Status.Phase != v1alpha1.PhaseRunning || !s.Status.Ready {
			continue
		}
		if op, err := readOperation(s); err == nil && !op.Settled() {
			continue
		}
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out, nil
}
