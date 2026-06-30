// Package operator reconciles MinecraftServer objects (spec §4, §5, §7). The
// CRD is the lifecycle source-of-truth; this controller renders the
// StatefulSet/Service/PVC from it, gates readiness on an RCON probe, and injects
// graceful shutdown. It never reads or writes business-layer (Postgres) fields.
package operator

import (
	"context"
	"fmt"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/metrics"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Requeue cadences for the transient phases.
const (
	requeueStarting = 5 * time.Second
	requeueStopping = 5 * time.Second
	requeueSecret   = 10 * time.Second
)

// Reconciler reconciles a MinecraftServer with its managed children.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Prober gates readiness on RCON reachability.
	Prober Prober
	// Now is injectable for deterministic timestamps in tests; defaults to
	// metav1.Now.
	Now func() metav1.Time
}

func (r *Reconciler) now() metav1.Time {
	if r.Now != nil {
		return r.Now()
	}
	return metav1.Now()
}

// SetupWithManager wires the controller to watch MinecraftServers and the
// children it owns.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.MinecraftServer{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Complete(r)
}

// Reconcile drives a single MinecraftServer toward spec.desiredState.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var server v1alpha1.MinecraftServer
	if err := r.Get(ctx, req.NamespacedName, &server); err != nil {
		// Deletion is handled by owner references on the children.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	desired := server.Spec.DesiredState
	if desired == "" {
		desired = v1alpha1.DesiredStopped
	}
	if desired == v1alpha1.DesiredStopped {
		return r.reconcileStopped(ctx, &server)
	}
	return r.reconcileRunning(ctx, &server)
}

func (r *Reconciler) reconcileRunning(ctx context.Context, server *v1alpha1.MinecraftServer) (ctrl.Result, error) {
	if err := r.ensureServices(ctx, server); err != nil {
		return ctrl.Result{}, err
	}

	desired, err := buildStatefulSet(server, 1)
	if err != nil {
		// A malformed spec (e.g. bad storage quantity) is terminal until edited.
		r.markFailed(server, "InvalidSpec", err.Error())
		return ctrl.Result{}, r.patchStatus(ctx, server)
	}
	if err := controllerutil.SetControllerReference(server, desired, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.applyStatefulSet(ctx, desired); err != nil {
		return ctrl.Result{}, err
	}

	var current appsv1.StatefulSet
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), &current); err != nil {
		return ctrl.Result{}, err
	}

	// The pod must first pass its tcpSocket readiness (readyReplicas >= 1).
	if current.Status.ReadyReplicas < 1 {
		r.markStarting(server, "PodNotReady", "waiting for pod TCP readiness")
		if err := r.patchStatus(ctx, server); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueStarting}, nil
	}

	// Then the operator gates true readiness on an RCON probe (spec §5).
	if server.Spec.Rcon.Enabled {
		password, err := r.rconPassword(ctx, server)
		if err != nil {
			r.markStarting(server, "RconSecretUnavailable", err.Error())
			if perr := r.patchStatus(ctx, server); perr != nil {
				return ctrl.Result{}, perr
			}
			return ctrl.Result{RequeueAfter: requeueSecret}, nil
		}
		if err := r.Prober.Probe(ctx, rconAddress(server), password); err != nil {
			r.markStarting(server, "RconNotReachable", err.Error())
			if perr := r.patchStatus(ctx, server); perr != nil {
				return ctrl.Result{}, perr
			}
			return ctrl.Result{RequeueAfter: requeueStarting}, nil
		}
	}

	r.markRunningReady(server)
	return ctrl.Result{}, r.patchStatus(ctx, server)
}

func (r *Reconciler) reconcileStopped(ctx context.Context, server *v1alpha1.MinecraftServer) (ctrl.Result, error) {
	var sts appsv1.StatefulSet
	err := r.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: server.Name}, &sts)
	if apierrors.IsNotFound(err) {
		r.markStopped(server)
		return ctrl.Result{}, r.patchStatus(ctx, server)
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Scaling to zero triggers each pod's preStop RCON save+stop (spec §7).
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 {
		zero := int32(0)
		sts.Spec.Replicas = &zero
		if err := r.Update(ctx, &sts); err != nil {
			return ctrl.Result{}, err
		}
	}

	if sts.Status.Replicas > 0 || sts.Status.ReadyReplicas > 0 {
		r.markStopping(server)
		if err := r.patchStatus(ctx, server); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueStopping}, nil
	}

	r.markStopped(server)
	return ctrl.Result{}, r.patchStatus(ctx, server)
}

func (r *Reconciler) ensureServices(ctx context.Context, server *v1alpha1.MinecraftServer) error {
	for _, svc := range []*corev1.Service{buildHeadlessService(server), buildClientService(server)} {
		if err := controllerutil.SetControllerReference(server, svc, r.Scheme); err != nil {
			return err
		}
		if err := r.applyService(ctx, svc); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) rconPassword(ctx context.Context, server *v1alpha1.MinecraftServer) (string, error) {
	ref := server.Spec.Rcon.SecretRef
	if ref.Name == "" || ref.Key == "" {
		return "", fmt.Errorf("rcon.secretRef.name and .key are required when rcon is enabled")
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: ref.Name}, &secret); err != nil {
		return "", err
	}
	b, ok := secret.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("secret %q has no key %q", ref.Name, ref.Key)
	}
	return string(b), nil
}

// applyStatefulSet creates the StatefulSet or, if it exists, updates only its
// mutable fields (StatefulSet selector/serviceName/volumeClaimTemplates are
// immutable and must not be re-sent).
func (r *Reconciler) applyStatefulSet(ctx context.Context, desired *appsv1.StatefulSet) error {
	var existing appsv1.StatefulSet
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Labels = desired.Labels
	existing.Spec.Replicas = desired.Spec.Replicas
	existing.Spec.Template = desired.Spec.Template
	return r.Update(ctx, &existing)
}

// applyService creates or updates a Service, preserving the cluster-assigned
// ClusterIP so the update does not orphan the address.
func (r *Reconciler) applyService(ctx context.Context, desired *corev1.Service) error {
	var existing corev1.Service
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	desired.ResourceVersion = existing.ResourceVersion
	desired.Spec.ClusterIP = existing.Spec.ClusterIP
	desired.Spec.ClusterIPs = existing.Spec.ClusterIPs
	return r.Update(ctx, desired)
}

func (r *Reconciler) patchStatus(ctx context.Context, server *v1alpha1.MinecraftServer) error {
	return r.Status().Update(ctx, server)
}

// --- status mutators -------------------------------------------------------

func (r *Reconciler) markStarting(server *v1alpha1.MinecraftServer, reason, msg string) {
	server.Status.Phase = v1alpha1.PhaseStarting
	server.Status.Ready = false
	server.Status.ObservedGeneration = server.Generation
	// Anchor felis_start_duration_seconds (spec §23) at the first Starting pass of
	// this start attempt. Set-once (cleared on stop) so re-entrant Starting
	// reconciles preserve the original anchor and the observed duration spans the
	// whole start, not just the last requeue. A server that reaches readiness
	// without ever passing through Starting leaves this nil, and markRunningReady
	// skips the observation rather than recording a bogus one.
	if server.Status.StartRequestedAt == nil {
		t := r.now()
		server.Status.StartRequestedAt = &t
	}
	server.Status.Endpoint = v1alpha1.EndpointStatus{Mode: v1alpha1.EndpointFallback, Address: server.Spec.FallbackServer}
	server.Status.LiveMotd = server.Spec.Motd.Starting
	r.setCondition(server, v1alpha1.ConditionReady, metav1.ConditionFalse, reason, msg)
	r.setCondition(server, v1alpha1.ConditionRconReached, metav1.ConditionFalse, reason, msg)
}

func (r *Reconciler) markRunningReady(server *v1alpha1.MinecraftServer) {
	server.Status.Phase = v1alpha1.PhaseRunning
	server.Status.Ready = true
	server.Status.ObservedGeneration = server.Generation
	if server.Status.ReadySignalAt == nil {
		t := r.now()
		server.Status.ReadySignalAt = &t
		// Observe felis_start_duration_seconds (spec §23) exactly once, when
		// readiness is first reached. StartRequestedAt was persisted by an earlier
		// Starting reconcile; if it is nil the server became ready without a
		// Starting pass and there is no meaningful start interval to record.
		if server.Status.StartRequestedAt != nil {
			metrics.StartDurationSeconds.Observe(t.Sub(server.Status.StartRequestedAt.Time).Seconds())
		}
	}
	server.Status.Endpoint = v1alpha1.EndpointStatus{Mode: v1alpha1.EndpointDirect, Address: gameAddress(server)}
	server.Status.LiveMotd = server.Spec.Motd.Running
	r.setCondition(server, v1alpha1.ConditionRconReached, metav1.ConditionTrue, "Probed", "RCON probe succeeded")
	r.setCondition(server, v1alpha1.ConditionReady, metav1.ConditionTrue, "RconReached", "server is accepting RCON")
}

func (r *Reconciler) markStopping(server *v1alpha1.MinecraftServer) {
	server.Status.Phase = v1alpha1.PhaseStopping
	server.Status.Ready = false
	server.Status.ObservedGeneration = server.Generation
	server.Status.Endpoint = v1alpha1.EndpointStatus{Mode: v1alpha1.EndpointFallback, Address: server.Spec.FallbackServer}
	server.Status.LiveMotd = server.Spec.Motd.Stopped
	r.setCondition(server, v1alpha1.ConditionReady, metav1.ConditionFalse, "Stopping", "scaling down")
}

func (r *Reconciler) markStopped(server *v1alpha1.MinecraftServer) {
	server.Status.Phase = v1alpha1.PhaseStopped
	server.Status.Ready = false
	server.Status.ObservedGeneration = server.Generation
	server.Status.ReadySignalAt = nil
	// Clear the start anchor so the next Running transition re-anchors and
	// felis_start_duration_seconds measures the new start, not since the last one.
	server.Status.StartRequestedAt = nil
	server.Status.Players = v1alpha1.PlayersStatus{}
	server.Status.Endpoint = v1alpha1.EndpointStatus{Mode: v1alpha1.EndpointFallback, Address: server.Spec.FallbackServer}
	server.Status.LiveMotd = server.Spec.Motd.Stopped
	r.setCondition(server, v1alpha1.ConditionReady, metav1.ConditionFalse, "Stopped", "desiredState is Stopped")
	r.setCondition(server, v1alpha1.ConditionRconReached, metav1.ConditionFalse, "Stopped", "server is stopped")
}

func (r *Reconciler) markFailed(server *v1alpha1.MinecraftServer, reason, msg string) {
	server.Status.Phase = v1alpha1.PhaseFailed
	server.Status.Ready = false
	server.Status.ObservedGeneration = server.Generation
	r.setCondition(server, v1alpha1.ConditionReady, metav1.ConditionFalse, reason, msg)
	r.setCondition(server, v1alpha1.ConditionProvisioned, metav1.ConditionFalse, reason, msg)
}

func (r *Reconciler) setCondition(server *v1alpha1.MinecraftServer, condType string, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(&server.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: server.Generation,
		LastTransitionTime: r.now(),
	})
}
