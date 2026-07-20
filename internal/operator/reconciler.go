// Package operator reconciles MinecraftServer objects (spec §4, §5, §7). The
// CRD is the lifecycle source-of-truth; this controller renders the
// StatefulSet/Service/PVC from it, gates readiness on an RCON probe, and injects
// graceful shutdown. It never reads or writes business-layer (Postgres) fields.
package operator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	requeueStarting            = 5 * time.Second
	requeueStopping            = 5 * time.Second
	requeueSecret              = 10 * time.Second
	defaultTimeoutSeconds      = 300
	defaultReadinessTimeoutSec = 300
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
	endpointAddress, err := r.ensureServices(ctx, server)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Before the StatefulSet, not after: buildEnv wires RCON_PASSWORD as a
	// secretKeyRef, so a pod created ahead of its Secret never starts — it sits in
	// CreateContainerConfigError, which reads like a broken image rather than a
	// missing key.
	if err := r.ensureRconSecret(ctx, server); err != nil {
		r.markStarting(server, "RconSecretUnavailable", err.Error())
		if perr := r.patchStatus(ctx, server); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{RequeueAfter: requeueSecret}, nil
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
		if r.startupTimedOut(server) {
			r.markFailed(server, "StartupTimeout", "pod did not become ready within startup timeout")
		}
		if err := r.patchStatus(ctx, server); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueStarting}, nil
	}
	if endpointAddress == "" {
		r.markStarting(server, "ServiceAddressPending", "waiting for the client Service ClusterIP")
		if err := r.patchStatus(ctx, server); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueStarting}, nil
	}

	// Then the operator gates true readiness on an RCON probe (spec §5), which
	// also samples the current player tally for Status.Players.
	var players PlayerCount
	if server.Spec.Rcon.Enabled {
		password, err := r.rconPassword(ctx, server)
		if err != nil {
			r.markStarting(server, "RconSecretUnavailable", err.Error())
			if perr := r.patchStatus(ctx, server); perr != nil {
				return ctrl.Result{}, perr
			}
			return ctrl.Result{RequeueAfter: requeueSecret}, nil
		}
		pc, err := r.Prober.Probe(ctx, rconAddress(server), password)
		if err != nil {
			r.markStarting(server, "RconNotReachable", err.Error())
			if r.readinessTimedOut(server) {
				r.markFailed(server, "ReadinessTimeout", "RCON probe did not succeed within readiness timeout")
			}
			if perr := r.patchStatus(ctx, server); perr != nil {
				return ctrl.Result{}, perr
			}
			return ctrl.Result{RequeueAfter: requeueStarting}, nil
		}
		players = pc
	}

	// Idle auto-stop (spec §8): when enabled, the server is Running, and the
	// player tally is zero, track the empty duration and auto-stop when the
	// configured timeout expires. The existing RCON probe already supplies
	// the player count — no extra network cost.
	// Rcon.Enabled is part of the condition because `players` is only a real tally
	// when the probe above ran: with RCON off it keeps its zero value, which this
	// branch would read as "empty" and use to stop a server full of people.
	if server.Spec.Rcon.Enabled && server.Spec.Idle.AutoStopEnabled && server.Spec.Idle.EmptySecondsBeforeStop > 0 {
		if players.Online == 0 {
			if server.Status.EmptySince == nil {
				t := r.now()
				server.Status.EmptySince = &t
			} else if r.now().Time.Sub(server.Status.EmptySince.Time).Seconds() >=
				float64(server.Spec.Idle.EmptySecondsBeforeStop) {
				server.Spec.DesiredState = v1alpha1.DesiredStopped
				server.Status.EmptySince = nil
				if err := r.Update(ctx, server); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			}
		} else if server.Status.EmptySince != nil {
			server.Status.EmptySince = nil
		}
	}

	r.markRunningReady(server, players, endpointAddress)
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

func (r *Reconciler) ensureServices(ctx context.Context, server *v1alpha1.MinecraftServer) (string, error) {
	endpointAddress := ""
	for _, svc := range []*corev1.Service{buildHeadlessService(server), buildClientService(server)} {
		if err := controllerutil.SetControllerReference(server, svc, r.Scheme); err != nil {
			return "", err
		}
		if err := r.applyService(ctx, svc); err != nil {
			return "", err
		}
		if svc.Name == server.Name {
			var current corev1.Service
			if err := r.Get(ctx, client.ObjectKeyFromObject(svc), &current); err != nil {
				return "", err
			}
			if current.Spec.ClusterIP != "" && current.Spec.ClusterIP != corev1.ClusterIPNone {
				endpointAddress = fmt.Sprintf("%s:%d", current.Spec.ClusterIP, GamePort)
			}
		}
	}
	return endpointAddress, nil
}

// ensureRconSecret creates the per-server RCON password Secret named by
// spec.rcon.secretRef the first time a server with RCON enabled reconciles, and
// leaves it alone afterwards. Provisioning lives here rather than in felis-api's
// CreateServer for three reasons: it is declarative (a server whose Secret was
// deleted heals on the next pass instead of staying permanently unreachable), the
// controller reference makes Kubernetes garbage-collect the Secret with the server
// so no delete path has to remember it, and it backfills — a server created before
// RCON existed only needs spec.rcon filled in, and the password appears without
// anyone handling it. felis-api never mints the password and never needs to: it
// reads the Secret at command time (internal/api/console.go rconPassword).
//
// The password is 32 hex chars from crypto/rand. It is generated once and never
// rotated here: rewriting it would leave the running server authenticating with
// the old value until its pod restarts, so rotation belongs to an explicit
// operation, not to a reconcile that runs every few seconds.
func (r *Reconciler) ensureRconSecret(ctx context.Context, server *v1alpha1.MinecraftServer) error {
	if !server.Spec.Rcon.Enabled {
		return nil
	}
	ref := server.Spec.Rcon.SecretRef
	if ref.Name == "" || ref.Key == "" {
		return fmt.Errorf("rcon.secretRef.name and .key are required when rcon is enabled")
	}
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: ref.Name}, &existing)
	if err == nil {
		if _, ok := existing.Data[ref.Key]; ok {
			return nil
		}
		return fmt.Errorf("secret %s exists but has no key %q", ref.Name, ref.Key)
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	password, err := randomRconPassword()
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ref.Name,
			Namespace: server.Namespace,
			Labels:    map[string]string{v1alpha1.LabelServer: server.Name},
		},
		Type: corev1.SecretTypeOpaque,
		// Data, not StringData: StringData is a write-only convenience the API server
		// folds into Data, so anything reading the object back — including this
		// package's own tests — would see an empty value. Encoding here keeps the
		// object self-consistent the moment it is built.
		Data: map[string][]byte{ref.Key: []byte(password)},
	}
	if err := controllerutil.SetControllerReference(server, secret, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, secret); err != nil {
		// Another reconcile (or a racing replica) won: that Secret is as good as
		// this one, so treat the collision as success rather than thrashing.
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return err
	}
	return nil
}

// randomRconPassword returns 16 crypto/rand bytes as hex. Hex, not base64: the
// value is written verbatim into server.properties, whose parser treats the line
// as raw text to end-of-line, and hex avoids every character (=, :, \, whitespace)
// that a properties file or a shell round-trip could interpret.
func randomRconPassword() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate rcon password: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
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

func (r *Reconciler) markRunningReady(server *v1alpha1.MinecraftServer, players PlayerCount, endpointAddress string) {
	server.Status.Phase = v1alpha1.PhaseRunning
	server.Status.Ready = true
	server.Status.ObservedGeneration = server.Generation
	// Refresh the player tally sampled by this reconcile's RCON probe so the panel
	// reports live occupancy instead of the 0/0 markStopped leaves behind.
	server.Status.Players = v1alpha1.PlayersStatus{Online: players.Online, Max: players.Max}
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
	server.Status.Endpoint = v1alpha1.EndpointStatus{Mode: v1alpha1.EndpointDirect, Address: endpointAddress}
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
	server.Status.EmptySince = nil // reset idle auto-stop timer
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

func (r *Reconciler) startupTimedOut(server *v1alpha1.MinecraftServer) bool {
	if server.Status.StartRequestedAt == nil {
		return false
	}
	timeout := time.Duration(server.Spec.Startup.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultTimeoutSeconds * time.Second
	}
	return r.now().Time.Sub(server.Status.StartRequestedAt.Time) >= timeout
}

func (r *Reconciler) readinessTimedOut(server *v1alpha1.MinecraftServer) bool {
	if server.Status.StartRequestedAt == nil {
		return false
	}
	timeout := time.Duration(server.Spec.Startup.ReadinessTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = defaultReadinessTimeoutSec * time.Second
	}
	return r.now().Time.Sub(server.Status.StartRequestedAt.Time) >= timeout
}
