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

// Failover brings the worlds of a lost worker back on another node from their newest
// backups. It is a migration whose source can no longer be read:
//
//  1. The Owner asks for it in the panel, or felis-api does once the node has been gone
//     for the configured time when automatic failover is on. BeginFailover records an
//     Operation of KindFailover on each server whose world is on the lost node, takes the
//     world lock, and sets the server's desired state to Stopped, remembering whether it
//     was running.
//  2. node-control fences the node (its fence task): cordoned, its approval removed and
//     the out-of-service taint added, so Kubernetes releases its pods and volumes without
//     waiting for its kubelet, and the node takes no world again until the Owner approves
//     it again. A worker that only lost its network finds its pods gone when it comes back
//     and stops them.
//  3. fencing: the server's unfinished maintenance Jobs, all pinned to the lost node, are
//     deleted; once the node is fenced and nothing of the server is left on it, the
//     failover goes on.
//  4. restoring and switching are the migration's own: a new claim on the target node,
//     the backup restored and read back into it, then one write that points the server at
//     its new node and claim and gives it its desired state back.
//
// It costs whatever the world gained since that backup, which the panel shows before the
// Owner confirms. The old world stays on the lost node's disk under its old claim.

const (
	// KindFailover marks a failover Operation.
	KindFailover = "failover"
	// TaintOutOfService is Kubernetes' non-graceful node shutdown taint: the pods on the
	// node are deleted and their volumes detached without waiting for its kubelet.
	TaintOutOfService = "node.kubernetes.io/out-of-service"
	// AnnotationFenced records when node-control fenced the node.
	AnnotationFenced = "felis.lolicon.best/fenced"
)

// ErrNodeNotLost refuses a failover off a node that is online, or is not a worker.
var ErrNodeNotLost = errors.New("only an offline worker can be failed over")

// Fenced reports whether a node is out of service for a failover: cordoned, no longer
// approved, and tainted out of service.
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

// lostSince is when an offline node stopped reporting ready.
func lostSince(n *corev1.Node) *time.Time {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue && !c.LastTransitionTime.IsZero() {
			t := c.LastTransitionTime.UTC()
			return &t
		}
	}
	return nil
}

// placedOn is the node a server's world lives on.
func (m *Manager) placedOn(s *v1alpha1.MinecraftServer) string {
	switch {
	case s.Spec.NodeName != "":
		return s.Spec.NodeName
	case s.Status.NodeName != "":
		return s.Status.NodeName
	}
	return m.Controller
}

// LostNode returns the named node when it is offline and may be failed over: a worker, or,
// with controller high availability, a controller while another one carries on. Anything
// else is ErrNodeNotLost.
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

// PlacedServer is a user server whose world is on a node, with the operation it has
// underway, if any.
type PlacedServer struct {
	Name         string
	DesiredState string
	Operation    *Operation
}

// ServersOn lists the user servers whose world lives on node, by name.
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

// BeginFailover starts the failover of one server off its lost node onto target, from the
// backup archive given (its Ref, and its digest when one was recorded). Asking again for
// the same target returns the failover already underway. A migration the lost node holds
// up is given up first; a failover that failed may be started again towards another node.
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
			// A migration cannot finish without its lost node, nor can a failed failover
			// towards another target: drop what either made.
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

// fence is a failover's first stage: it waits for node-control to fence the lost node and
// for the server to have nothing left on it, deleting the server's unfinished maintenance
// Jobs meanwhile (they are pinned to the lost node and would keep its pods pending). Not
// yet done is not an error: the stage simply runs again on the next pass.
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

// FencePending lists the lost nodes failovers are waiting on, by name: not fenced yet, or
// fenced but still holding a pod of a server failing over (one a Job made after the
// fence). felis-api asks node-control to fence each of them (again).
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
			continue // a deleted node holds nothing
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

// errFailoverTarget is returned when no node can take the worlds of a lost one.
var errFailoverTarget = errors.New("no online node can take the worlds")

// FailoverTarget picks the node an automatic failover moves worlds to: of the online,
// schedulable nodes that may hold worlds (approved workers and the controller), the one
// holding the fewest user worlds; on a tie the controller, then the first by name.
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

// LostNodes lists the nodes that are offline and not fenced yet, workers and (with
// controller high availability) controllers, with when each was lost, and whether a
// controller is online. The automatic failover acts only when exactly one node is lost and
// a controller is up; a fenced node has been dealt with.
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

// ReplicaCandidates lists, by name, the user servers running on a worker (or, with
// controller high availability, on any node, the controllers being losable too) with
// nothing underway on their world: the ones felis-api keeps a recent replica of, so a
// failover off that node loses little.
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
