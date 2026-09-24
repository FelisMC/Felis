// Package operator reconciles MinecraftServer objects (spec §4, §5, §7). The
// CRD is the lifecycle source-of-truth; this controller renders the
// StatefulSet/Service/PVC from it, gates readiness on an RCON probe, and injects
// graceful shutdown. It never reads or writes business-layer (Postgres) fields.
package operator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/metrics"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
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
	// requeueStarting is the cadence of the RCON readiness re-probe while a server
	// is Starting. It bounds the window where the container is Ready but the API
	// still answers 409 not_running: at 5s the observed lag after container-ready
	// was 6~10s; 2s keeps wake-to-usable snappy without hammering a booting Java
	// process (the probe only runs on this cadence while the server is unreachable).
	requeueStarting            = 2 * time.Second
	requeueStopping            = 5 * time.Second
	requeueSecret              = 10 * time.Second
	requeueIdleProbe           = 30 * time.Second
	requeueMaintenance         = 5 * time.Second
	defaultTimeoutSeconds      = 300
	defaultReadinessTimeoutSec = 300
)

// RconSecretAnnotation stamps the pod template with a fingerprint of the
// current RCON password. If the Secret is ever lost (its deletion destroys the
// password) and re-provisioned, the fingerprint changes and the StatefulSet
// rolls the pod onto the new password. Without it the running pod keeps
// authenticating with the old value while the operator probes with the new
// one, and the RCON gate fails until someone restarts the pod by hand.
const RconSecretAnnotation = "felis.lolicon.best/rcon-secret"

// rconStamp fingerprints an RCON password for RconSecretAnnotation. 64 bits of
// SHA-256: enough to never confuse two passwords, short enough to read.
func rconStamp(password []byte) string {
	sum := sha256.Sum256(password)
	return hex.EncodeToString(sum[:8])
}

// Reconciler reconciles a MinecraftServer with its managed children.
type Reconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Prober gates readiness on RCON reachability.
	Prober Prober
	// FelisImage is this operator's own image, used for the forwarding-config
	// initContainer injected into user servers. Empty (an operator Deployment
	// without FELIS_IMAGE) disables that injection rather than failing.
	FelisImage string
	// Now is injectable for deterministic timestamps in tests; defaults to
	// metav1.Now.
	Now func() metav1.Time
	// Jobs reads the minecraft namespace's Jobs for the world-volume lock
	// (internal/maintenance). It is the manager's uncached API reader, so the
	// operator needs jobs:list and no Job informer. Nil skips the check.
	Jobs client.Reader
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
		// Owns the Secrets too: the per-server RCON password is managed here,
		// and a watch is what lets a deleted Secret be noticed at all (a quiet
		// Running server otherwise produces no events).
		Owns(&corev1.Secret{}).
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
	if kind, held, err := r.maintenanceHold(ctx, server); err != nil {
		return ctrl.Result{}, err
	} else if held {
		// Leave phase and the start anchor alone: nothing is starting yet, and a
		// long restore must not be charged to the startup timeout.
		server.Status.ObservedGeneration = server.Generation
		r.setCondition(server, v1alpha1.ConditionReady, metav1.ConditionFalse, "MaintenanceInProgress",
			"waiting for the "+kind+" on this server's world to finish before starting")
		if err := r.patchStatus(ctx, server); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueMaintenance}, nil
	}

	endpointAddress, err := r.ensureServices(ctx, server)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Before the StatefulSet, not after: buildEnv wires RCON_PASSWORD as a
	// secretKeyRef, so a pod created ahead of its Secret never starts — it sits in
	// CreateContainerConfigError, which reads like a broken image rather than a
	// missing key.
	passwordStamp, err := r.ensureRconSecret(ctx, server)
	if err != nil {
		r.markStarting(server, "RconSecretUnavailable", err.Error())
		if perr := r.patchStatus(ctx, server); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{RequeueAfter: requeueSecret}, nil
	}

	desired, err := buildStatefulSet(server, 1, r.FelisImage)
	if err != nil {
		// A malformed spec (e.g. bad storage quantity) is terminal until edited.
		r.markFailed(server, "InvalidSpec", err.Error())
		return ctrl.Result{}, r.patchStatus(ctx, server)
	}
	if passwordStamp != "" {
		if desired.Spec.Template.Annotations == nil {
			desired.Spec.Template.Annotations = map[string]string{}
		}
		desired.Spec.Template.Annotations[RconSecretAnnotation] = passwordStamp
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
		// A tally that could not be read pauses idle auto-stop (below) instead of
		// counting as an empty server; the condition says so, so a server that
		// never stops idle shows why.
		if pc.Known {
			r.setCondition(server, v1alpha1.ConditionPlayersCounted, metav1.ConditionTrue, "Counted", "RCON list reply read")
		} else {
			r.setCondition(server, v1alpha1.ConditionPlayersCounted, metav1.ConditionFalse, "ListUnreadable",
				"RCON list failed or its reply matched no known format; idle auto-stop is paused until the player count can be read")
		}
	}

	// Idle auto-stop (spec §8): when enabled, the server is Running, and the
	// player tally is zero, track the empty duration and auto-stop when the
	// configured timeout expires. The existing RCON probe already supplies
	// the player count — no extra network cost.
	// players.Known gates the whole branch: an unread tally (RCON off, `list`
	// failed, or a reply format the parser does not know) must never read as
	// "empty" and stop a server full of people. It neither stamps nor clears
	// EmptySince, so a flaky read does not restart the countdown either; the
	// stop itself only ever follows a sample that really said zero.
	if players.Known && server.Spec.Idle.AutoStopEnabled && server.Spec.Idle.EmptySecondsBeforeStop > 0 {
		if players.Online == 0 {
			if server.Status.EmptySince == nil {
				t := r.now()
				server.Status.EmptySince = &t
			} else if r.now().Time.Sub(server.Status.EmptySince.Time).Seconds() >=
				float64(server.Spec.Idle.EmptySecondsBeforeStop) {
				// Merge patch, not Update: an unrelated reconcile writes status
				// concurrently, and shipping the whole object back risks
				// clobbering it (the reaper's Stop uses the same pattern for
				// the same reason). EmptySince is deliberately left for
				// markStopped to clear once the scale-down completes.
				patch := client.MergeFrom(server.DeepCopy())
				server.Spec.DesiredState = v1alpha1.DesiredStopped
				if err := r.Patch(ctx, server, patch); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			}
		} else if server.Status.EmptySince != nil {
			server.Status.EmptySince = nil
		}
	}

	r.markRunningReady(server, players, endpointAddress)
	if err := r.patchStatus(ctx, server); err != nil {
		return ctrl.Result{}, err
	}
	// Idle auto-stop has no natural wake-up: player joins/leaves never touch
	// this CRD and RCON is only probed here, so without a requeue the
	// empty-duration counter would be stamped once and then never revisited
	// (observed live: the stamp sat unexamined for minutes). Wake at the exact
	// deadline while the tally says empty, or on a slow cadence while players
	// are online, to notice the moment the last one leaves.
	if server.Spec.Rcon.Enabled && server.Spec.Idle.AutoStopEnabled && server.Spec.Idle.EmptySecondsBeforeStop > 0 {
		if server.Status.EmptySince != nil {
			deadline := server.Status.EmptySince.Time.Add(time.Duration(server.Spec.Idle.EmptySecondsBeforeStop) * time.Second)
			if wait := deadline.Sub(r.now().Time); wait > 0 {
				return ctrl.Result{RequeueAfter: wait}, nil
			}
		}
		return ctrl.Result{RequeueAfter: requeueIdleProbe}, nil
	}
	return ctrl.Result{}, nil
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

// maintenanceHold reports whether a restore, backup or file write holds the
// server's world volume (internal/maintenance) while its pod is about to be
// created. felis-api already refuses a wake in that state; this is the same rule
// for a desiredState flipped by anything else (kubectl, a script), since a game
// pod scheduled beside a restore Job boots on a half-extracted world. A server
// whose StatefulSet is already scaled up is never held: its pod exists, and
// stopping it here would only lose the players on it.
func (r *Reconciler) maintenanceHold(ctx context.Context, server *v1alpha1.MinecraftServer) (string, bool, error) {
	if r.Jobs == nil {
		return "", false, nil
	}
	var sts appsv1.StatefulSet
	err := r.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: server.Name}, &sts)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return "", false, err
	case sts.Spec.Replicas == nil || *sts.Spec.Replicas > 0 || sts.Status.Replicas > 0:
		return "", false, nil
	}
	var jobs batchv1.JobList
	if err := r.Jobs.List(ctx, &jobs, client.InNamespace(server.Namespace),
		client.MatchingLabels{maintenance.LabelServer: server.Name}); err != nil {
		return "", false, err
	}
	kind, held := maintenance.Holder(server.Name, server.Annotations, jobs.Items, r.now().Time)
	return kind, held, nil
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
// CreateServer for three reasons: it is declarative (a deleted Secret is
// re-minted on the next pass), the controller reference makes Kubernetes
// garbage-collect the Secret with the server so no delete path has to remember
// it, and it backfills — a server created before RCON existed only needs
// spec.rcon filled in, and the password appears without anyone handling it.
// felis-api never mints the password and never needs to: it reads the Secret at
// command time (internal/api/console.go rconPassword).
//
// The password is 32 hex chars from crypto/rand. It is generated once and never
// rotated here: rewriting it would leave the running server authenticating with
// the old value until its pod restarts, so rotation belongs to an explicit
// operation, not to a reconcile that runs every few seconds. Re-creation after
// a deletion is the one case where a fresh password must reach the pod — the
// caller stamps the returned fingerprint onto the pod template, so the
// StatefulSet rolls exactly when the password underneath it changes.
func (r *Reconciler) ensureRconSecret(ctx context.Context, server *v1alpha1.MinecraftServer) (string, error) {
	if !server.Spec.Rcon.Enabled {
		return "", nil
	}
	ref := server.Spec.Rcon.SecretRef
	if ref.Name == "" || ref.Key == "" {
		return "", fmt.Errorf("rcon.secretRef.name and .key are required when rcon is enabled")
	}
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: ref.Name}, &existing)
	if err == nil {
		if b, ok := existing.Data[ref.Key]; ok {
			return rconStamp(b), nil
		}
		return "", fmt.Errorf("secret %s exists but has no key %q", ref.Name, ref.Key)
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}

	password, err := randomRconPassword()
	if err != nil {
		return "", err
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
		return "", err
	}
	if err := r.Create(ctx, secret); err != nil {
		// Another reconcile (or a racing replica) won: that Secret is as good as
		// this one, but the stamp must match what the next pass will read, so
		// fetch the winner — a stale cache here just requeues (the caller's
		// RconSecretUnavailable path retries in 10s) instead of stamping a
		// value that would flip on the next reconcile and roll the pod twice.
		if apierrors.IsAlreadyExists(err) {
			var winner corev1.Secret
			if gerr := r.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: ref.Name}, &winner); gerr != nil {
				return "", gerr
			}
			return rconStamp(winner.Data[ref.Key]), nil
		}
		return "", err
	}
	return rconStamp([]byte(password)), nil
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
	// reports live occupancy instead of the 0/0 markStopped leaves behind. An
	// unread tally keeps the last one shown.
	if players.Known {
		server.Status.Players = v1alpha1.PlayersStatus{Online: players.Online, Max: players.Max}
	}
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
	// The start attempt is over the moment it succeeds: clear the anchor so a
	// later pod blip runs on a fresh startup budget instead of inheriting a
	// stale one. Found live: a server that had already recovered was marked
	// StartupTimeout minutes later because the old anchor was still ticking
	// underneath, and the Failed condition outlived the recovery.
	server.Status.StartRequestedAt = nil
	server.Status.Endpoint = v1alpha1.EndpointStatus{Mode: v1alpha1.EndpointDirect, Address: endpointAddress}
	server.Status.LiveMotd = server.Spec.Motd.Running
	r.setCondition(server, v1alpha1.ConditionRconReached, metav1.ConditionTrue, "Probed", "RCON probe succeeded")
	r.setCondition(server, v1alpha1.ConditionReady, metav1.ConditionTrue, "RconReached", "server is accepting RCON")
	// markFailed flips Provisioned to False; a recovered server must flip it
	// back, or every consumer of the conditions sees a permanent failure flag.
	r.setCondition(server, v1alpha1.ConditionProvisioned, metav1.ConditionTrue, "Provisioned", "server is provisioned and accepting RCON")
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
