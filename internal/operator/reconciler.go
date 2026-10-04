// Package operator reconciles MinecraftServer objects (spec §4, §5, §7). The
// CRD is the lifecycle source-of-truth; this controller renders the
// StatefulSet/Service/PVC from it, gates readiness on an RCON probe, and flushes
// the world over RCON before scaling a server down. It never reads or writes
// business-layer (Postgres) fields.
package operator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/metrics"
	"felis.lolicon.best/internal/placement"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Requeue cadences for the transient phases.
const (
	// requeueStarting is the cadence of the RCON readiness re-probe while a server
	// is Starting. It bounds the window where the container is Ready but the API
	// still answers 409 not_running: at 5s the observed lag after container-ready
	// was 6~10s; 2s keeps wake-to-usable snappy without hammering a booting Java
	// process (the probe only runs on this cadence while the server is unreachable).
	requeueStarting     = 2 * time.Second
	requeueStopping     = 5 * time.Second
	requeueSecret       = 10 * time.Second
	requeueIdleProbe    = 30 * time.Second
	requeueRunningProbe = 60 * time.Second
	// requeueProbeRetry re-probes a Running server soon after a miss;
	// runningProbeMissesBeforeDegrade consecutive misses demote it to Starting.
	requeueProbeRetry               = 10 * time.Second
	runningProbeMissesBeforeDegrade = 3
	// requeueFailed paces a Failed server whose auto-restarts are spent; a
	// StatefulSet or CR event still wakes it at once.
	requeueFailed         = 5 * time.Minute
	requeueMaintenance    = 5 * time.Second
	defaultTimeoutSeconds = 300
	// maxAutoRestarts bounds how often a timed-out start is retried by
	// recreating its pod; autoRestartBaseBackoff is the first wait, doubling
	// per attempt.
	maxAutoRestarts            = v1alpha1.MaxAutoRestarts
	autoRestartBaseBackoff     = time.Minute
	defaultReadinessTimeoutSec = 300
	// arrivalWindow is how long after a run's first ready probe a zero tally starts
	// no idle countdown. A server usually comes up because someone asked for it: the
	// player who woke it is still being moved in from the lobby, players a restart
	// dropped are reconnecting, and a modpack client sits in its configuration phase,
	// missing from `list`, for a minute or more. Sampled before they arrived, the
	// zero stopped a server with a short idle timeout just as they got in.
	arrivalWindow = 3 * time.Minute
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

// PodTemplateAnnotation stamps the StatefulSet with a fingerprint of the pod
// template the operator last wrote, taken with the felis image left out of the
// init containers. The installer tags that image by release, so every platform
// upgrade hands the operator a new one; rolling every running server onto it
// would restart each world under its players for an init step that has already
// run. While a server runs and the fingerprint still matches, its template is
// left as it is and the new image arrives with its next start (stop scales to
// zero, and a start writes the whole template). Any other change — a spec edit,
// a new RCON password, a builder change in a new release — moves the
// fingerprint and rolls the pod as before.
const PodTemplateAnnotation = "felis.lolicon.best/pod-template"

// podTemplateStamp fingerprints tmpl for PodTemplateAnnotation. encoding/json
// writes map keys sorted, so the same template always hashes the same.
func podTemplateStamp(tmpl *corev1.PodTemplateSpec, felisImage string) (string, error) {
	t := tmpl.DeepCopy()
	for i := range t.Spec.InitContainers {
		if felisImage != "" && t.Spec.InitContainers[i].Image == felisImage {
			t.Spec.InitContainers[i].Image = ""
		}
	}
	b, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("fingerprint pod template: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]), nil
}

// Reconciler reconciles a MinecraftServer with its managed children.
type Reconciler struct {
	Nodes          client.Reader
	EgressProbe    string
	ControllerNode string
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
	// Secrets reads the RCON password Secrets, each by name. It is the manager's
	// uncached API reader, so the operator holds secrets:get and no list or watch:
	// a Secret informer would cache every Secret in the namespace (the felis-config
	// mirror with the database URL among them) and grow with them. Nil falls back
	// to the embedded client.
	Secrets client.Reader
	// Pods reads a server's pod-0 by name, to replace one left on an old template
	// (replaceStalePod). The manager's uncached API reader as well, so the
	// operator holds pods:get and no pod informer. Nil falls back to the embedded
	// client.
	Pods client.Reader
	// Recorder puts an Event on the MinecraftServer at each phase change, pod
	// recreation, idle stop and RCON Secret provision, so `kubectl describe`
	// shows the timeline the status alone overwrites. Nil records none.
	Recorder record.EventRecorder
	// Watch records the passes in flight for the liveness probe. Nil skips it.
	Watch *ReconcileWatch

	// probeFailures counts consecutive failed RCON probes of a Running server,
	// by namespaced name. In memory: an operator restart forgets them, which only
	// delays a degrade by a few probes. A count goes when a probe succeeds, when
	// the server is meant to stop and when it is deleted. A later server of the
	// same name inherits nothing that matters: it reaches Running only through a
	// probe that succeeds, and that clears the count.
	probeMu       sync.Mutex
	probeFailures map[types.NamespacedName]int
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
		// No Secret watch: a Running server is re-reconciled every
		// requeueRunningProbe anyway, which recreates a deleted RCON Secret, and
		// a stopped one gets it back on its next start.
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		Complete(r)
}

// maxConcurrentReconciles lets that many servers reconcile at once. A reconcile
// blocks on RCON (up to 5s for a probe, and up to defaultSaveTimeout for the
// save ahead of a stop), so with controller-runtime's default of one, a single
// large world saving would stall every other server's start, stop and readiness.
// The same server is never reconciled twice at once regardless.
const maxConcurrentReconciles = 4

// Reconcile drives a single MinecraftServer toward spec.desiredState. One pass
// is bounded by reconcileTimeout and reported to r.Watch while it runs.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	defer cancel()
	if r.Watch != nil {
		defer r.Watch.begin(req.Name)()
	}
	return r.reconcile(ctx, req)
}

func (r *Reconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var server v1alpha1.MinecraftServer
	if err := r.Get(ctx, req.NamespacedName, &server); err != nil {
		// Deletion is handled by owner references on the children; the probe-miss
		// count is the one part of a server kept in memory.
		if apierrors.IsNotFound(err) {
			r.clearProbeFailures(req.NamespacedName)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	desired := server.Spec.DesiredState
	if desired == "" {
		desired = v1alpha1.DesiredStopped
	}
	prevPhase, prevRestarts := server.Status.Phase, server.Status.AutoRestarts
	for _, annotation := range []string{v1alpha1.AnnotationStartRetry, v1alpha1.AnnotationRestart} {
		if _, ok := server.Annotations[annotation]; ok {
			if err := r.takeRestartRequest(ctx, &server, desired, annotation); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	var res ctrl.Result
	var err error
	if desired == v1alpha1.DesiredStopped {
		// Misses count only against a Running server; the next run starts afresh.
		r.clearProbeFailures(req.NamespacedName)
		res, err = r.reconcileStopped(ctx, &server)
	} else {
		res, err = r.reconcileRunning(ctx, &server)
	}
	if err == nil {
		r.recordTransition(ctx, &server, prevPhase, prevRestarts)
	}
	return res, err
}

// recordTransition logs and records a pass that moved the server to another
// phase, with the Ready condition's reason and message. Failing, a recreated
// pod and a Running server falling back to Starting are Warnings.
func (r *Reconciler) recordTransition(ctx context.Context, server *v1alpha1.MinecraftServer, prevPhase v1alpha1.Phase, prevRestarts int32) {
	phase := server.Status.Phase
	if phase == prevPhase {
		return
	}
	reason, msg := string(phase), ""
	if c := meta.FindStatusCondition(server.Status.Conditions, v1alpha1.ConditionReady); c != nil {
		reason, msg = c.Reason, c.Message
	}
	eventType := corev1.EventTypeNormal
	if phase == v1alpha1.PhaseFailed || server.Status.AutoRestarts > prevRestarts ||
		(prevPhase == v1alpha1.PhaseRunning && phase == v1alpha1.PhaseStarting) {
		eventType = corev1.EventTypeWarning
	}
	from := string(prevPhase)
	if from == "" {
		from = "New"
	}
	log.FromContext(ctx).Info("phase changed", "from", from, "to", phase, "reason", reason, "message", msg)
	r.event(server, eventType, reason, fmt.Sprintf("%s → %s: %s", from, phase, msg))
}

// takeRestartRequest recreates a Failed pod for a start retry, or a Running pod
// for an explicit restart. A request overtaken by a stop is only removed.
// The fresh status is written before the request is
// removed, so a pass that fails in between leaves the request to be taken again
// (at the cost of one more pod recreate), never a removed request with the old
// spent budget still in place.
func (r *Reconciler) takeRestartRequest(ctx context.Context, server *v1alpha1.MinecraftServer, desired v1alpha1.DesiredState, annotation string) error {
	explicit := annotation == v1alpha1.AnnotationRestart
	if desired == v1alpha1.DesiredRunning && (server.Status.Phase == v1alpha1.PhaseFailed || (explicit && server.Status.Phase == v1alpha1.PhaseRunning)) {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: server.Name + "-0", Namespace: server.Namespace}}
		if err := r.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
			return err
		}
		server.Status.AutoRestarts = 0
		server.Status.StartRequestedAt = nil
		reason, message := "StartRetried", "start requested again after it failed; recreated the pod"
		if explicit {
			reason, message = "RestartRequested", "restart requested; recreated the pod"
		}
		r.markStarting(server, reason, message)
		if err := r.patchStatus(ctx, server); err != nil {
			return err
		}
		log.FromContext(ctx).Info(message)
		r.event(server, corev1.EventTypeNormal, reason, message)
	}
	patch := client.MergeFrom(server.DeepCopy())
	delete(server.Annotations, annotation)
	return r.Patch(ctx, server, patch)
}

func (r *Reconciler) event(server *v1alpha1.MinecraftServer, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(server, eventType, reason, msg)
	}
}

func (r *Reconciler) reconcileRunning(ctx context.Context, server *v1alpha1.MinecraftServer) (ctrl.Result, error) {
	if server.Status.StopNoticeAt != nil {
		if err := r.callOffStop(ctx, server); err != nil {
			return ctrl.Result{}, err
		}
	}
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

	if r.Nodes != nil {
		node := server.Spec.NodeName
		var pod corev1.Pod
		if err := r.podReader().Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: server.Name + "-0"}, &pod); err == nil {
			server.Status.NodeName = pod.Spec.NodeName
			if node == "" {
				node = pod.Spec.NodeName
			}
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if node == "" {
			node = r.ControllerNode
		}
		if node != "" {
			var n corev1.Node
			err := r.Nodes.Get(ctx, types.NamespacedName{Name: node}, &n)
			if err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			if err != nil || !placement.Admitted(&n, r.ControllerNode) {
				var svc corev1.Service
				if err := r.Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: server.Name}, &svc); err == nil {
					if svc.Spec.Selector == nil {
						svc.Spec.Selector = map[string]string{}
					}
					svc.Spec.Selector["felis.lolicon.best/node-online"] = "true"
					if err := r.Update(ctx, &svc); err != nil {
						return ctrl.Result{}, err
					}
				} else if !apierrors.IsNotFound(err) {
					return ctrl.Result{}, err
				}
				server.Status.Ready = false
				server.Status.Endpoint = v1alpha1.EndpointStatus{Mode: v1alpha1.EndpointFallback}
				r.setCondition(server, v1alpha1.ConditionReady, metav1.ConditionFalse, "NodeUnavailable", "execution node is offline; automatic relocation is disabled")
				return ctrl.Result{RequeueAfter: 10 * time.Second}, r.patchStatus(ctx, server)
			}
		}
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

	placed := server.DeepCopy()
	if placed.Spec.NodeName == "" {
		placed.Spec.NodeName = r.ControllerNode
	}
	desired, err := buildStatefulSet(placed, 1, r.FelisImage, r.EgressProbe)
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
		if replaced, err := r.replaceStalePod(ctx, server, &current); err != nil {
			return ctrl.Result{}, err
		} else if replaced {
			// A new spec is a new start: its own timeout and restart budget.
			server.Status.AutoRestarts = 0
			server.Status.StartRequestedAt = nil
			r.markStarting(server, "SpecChanged", "the spec changed while the pod was not ready; recreated the pod from the new spec")
			if err := r.patchStatus(ctx, server); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: requeueStarting}, nil
		}
		r.markStarting(server, "PodNotReady", "waiting for pod TCP readiness")
		if r.startupTimedOut(server) {
			r.markFailed(server, v1alpha1.ReasonStartupTimeout, "pod did not become ready within startup timeout")
			if err := r.recoverFailedStart(ctx, server, startupTimeout(server)); err != nil {
				return ctrl.Result{}, err
			}
		}
		if err := r.patchStatus(ctx, server); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.startingRequeue(server, startupTimeout(server))}, nil
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
			// One missed probe of a Running server is noise (a lag spike, a save);
			// it keeps its status and endpoint until the misses run consecutive.
			if server.Status.Phase == v1alpha1.PhaseRunning && r.noteProbeFailure(client.ObjectKeyFromObject(server)) < runningProbeMissesBeforeDegrade {
				return ctrl.Result{RequeueAfter: requeueProbeRetry}, nil
			}
			r.markStarting(server, "RconNotReachable", err.Error())
			if r.readinessTimedOut(server) {
				r.markFailed(server, v1alpha1.ReasonReadinessTimeout, "RCON probe did not succeed within readiness timeout")
				if err := r.recoverFailedStart(ctx, server, readinessTimeout(server)); err != nil {
					return ctrl.Result{}, err
				}
			}
			if perr := r.patchStatus(ctx, server); perr != nil {
				return ctrl.Result{}, perr
			}
			return ctrl.Result{RequeueAfter: r.startingRequeue(server, readinessTimeout(server))}, nil
		}
		r.clearProbeFailures(client.ObjectKeyFromObject(server))
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
	if players.Known && idleStopApplies(server) {
		if players.Online == 0 {
			if server.Status.EmptySince == nil {
				if now := r.now(); !arriving(server, now) {
					server.Status.EmptySince = &now
				}
			} else if r.now().Time.Sub(server.Status.EmptySince.Time).Seconds() >=
				float64(server.Spec.Idle.EmptySecondsBeforeStop) {
				// Merge patch, not Update: an unrelated reconcile writes status
				// concurrently, and shipping the whole object back risks
				// clobbering it (the reaper's Stop uses the same pattern for
				// the same reason). EmptySince is deliberately left for
				// markStopping to clear once the scale-down starts.
				patch := client.MergeFrom(server.DeepCopy())
				server.Spec.DesiredState = v1alpha1.DesiredStopped
				if err := r.Patch(ctx, server, patch); err != nil {
					return ctrl.Result{}, err
				}
				msg := fmt.Sprintf("no players online for %ds; set desiredState to Stopped", server.Spec.Idle.EmptySecondsBeforeStop)
				log.FromContext(ctx).Info("idle stop", "emptySeconds", server.Spec.Idle.EmptySecondsBeforeStop)
				r.event(server, corev1.EventTypeNormal, "IdleStop", msg)
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
	if server.Spec.Rcon.Enabled && idleStopApplies(server) {
		if server.Status.EmptySince != nil {
			deadline := server.Status.EmptySince.Time.Add(time.Duration(server.Spec.Idle.EmptySecondsBeforeStop) * time.Second)
			if wait := deadline.Sub(r.now().Time); wait > 0 {
				return ctrl.Result{RequeueAfter: wait}, nil
			}
		}
		return ctrl.Result{RequeueAfter: requeueIdleProbe}, nil
	}
	// Without idle auto-stop nothing else wakes a Running server either: the
	// player tally and the RCON check would only refresh on a StatefulSet or CR
	// event. A slow cadence keeps Status.Players and readiness current.
	return ctrl.Result{RequeueAfter: requeueRunningProbe}, nil
}

// idleStopApplies reports whether idle auto-stop is configured for server. A
// system server (the login gate, the lobby) never idles out whatever its spec
// says: stopping the gate locks every player out, and nothing would wake it.
func idleStopApplies(server *v1alpha1.MinecraftServer) bool {
	return server.Labels[v1alpha1.LabelSystemRole] == "" &&
		server.Spec.Idle.AutoStopEnabled && server.Spec.Idle.EmptySecondsBeforeStop > 0
}

// arriving reports whether a zero tally at now may only mean the players this run
// came up for are not in yet: it is the run's first ready probe (ReadySignalAt is
// stamped after it) or within arrivalWindow of that probe.
func arriving(server *v1alpha1.MinecraftServer, now metav1.Time) bool {
	ready := server.Status.ReadySignalAt
	return ready == nil || now.Time.Before(ready.Add(arrivalWindow))
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

	// Graceful shutdown (spec §7): flush the world over RCON, then scale to zero,
	// which sends the server SIGTERM and so its own shutdown save within the
	// grace period.
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 {
		if wait, err := r.noticeStop(ctx, server, &sts); err != nil || wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, err
		}
		r.saveBeforeStop(ctx, server, &sts)
		server.Status.StopNoticeAt = nil
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

// StopNoticeWindow is how long the players on a server get between the warning
// and the stop: long enough to finish a fight or get off a boat, short enough that
// the owner's stop (and the backup or restore waiting behind it) is not held up.
const StopNoticeWindow = 30 * time.Second

// The lines players see around a stop. The operator does not know a player's
// language, so each carries both.
const (
	stopNoticeText    = "[Felis] 服务器将在 30 秒后关闭，请尽快找安全的地方 / This server stops in 30 seconds"
	stopNowText       = "[Felis] 正在保存世界并关闭服务器 / Saving the world and stopping now"
	stopCalledOffText = "[Felis] 关闭已取消 / The stop was called off"
)

// noticeStop warns the players on a server that is about to be scaled down, and
// returns how long the stop must still wait for them. The first pass tells everyone
// online and stamps StopNoticeAt; the stop goes ahead once StopNoticeWindow has
// passed since, with a last line as it does.
//
// There is no wait when nobody can be told or nobody is there: RCON off, no ready
// pod, a probe or broadcast that fails, or a tally that says zero players. A tally
// the server did not give (an unfamiliar `list` reply) still gets the warning,
// since players may well be on it. Idle auto-stop therefore never waits: it only
// fires on an empty server.
func (r *Reconciler) noticeStop(ctx context.Context, server *v1alpha1.MinecraftServer, sts *appsv1.StatefulSet) (time.Duration, error) {
	if at := server.Status.StopNoticeAt; at != nil {
		if wait := at.Time.Add(StopNoticeWindow).Sub(r.now().Time); wait > 0 {
			return wait, nil
		}
		r.broadcast(ctx, server, stopNowText)
		return 0, nil
	}
	if !server.Spec.Rcon.Enabled || sts.Status.ReadyReplicas == 0 {
		return 0, nil
	}
	password, err := r.rconPassword(ctx, server)
	if err != nil {
		return 0, nil
	}
	addr := rconAddress(server)
	players, err := r.Prober.Probe(ctx, addr, password)
	if err != nil || (players.Known && players.Online == 0) {
		return 0, nil
	}
	if err := r.Prober.Broadcast(ctx, addr, password, stopNoticeText); err != nil {
		ctrl.LoggerFrom(ctx).Info("could not warn players before the stop; stopping now", "error", err.Error())
		return 0, nil
	}
	now := r.now()
	server.Status.StopNoticeAt = &now
	if err := r.patchStatus(ctx, server); err != nil {
		return 0, err
	}
	r.event(server, corev1.EventTypeNormal, "StopNotice",
		fmt.Sprintf("players warned; stopping in %s", StopNoticeWindow))
	return StopNoticeWindow, nil
}

// callOffStop answers a desiredState that went back to Running inside the notice
// window: the players who were warned hear that the stop is off.
func (r *Reconciler) callOffStop(ctx context.Context, server *v1alpha1.MinecraftServer) error {
	r.broadcast(ctx, server, stopCalledOffText)
	server.Status.StopNoticeAt = nil
	return r.patchStatus(ctx, server)
}

// broadcast shows text to everyone on server, best-effort: a server that cannot be
// reached has nobody listening either.
func (r *Reconciler) broadcast(ctx context.Context, server *v1alpha1.MinecraftServer, text string) {
	if !server.Spec.Rcon.Enabled {
		return
	}
	password, err := r.rconPassword(ctx, server)
	if err != nil {
		return
	}
	if err := r.Prober.Broadcast(ctx, rconAddress(server), password, text); err != nil {
		ctrl.LoggerFrom(ctx).Info("player broadcast failed", "error", err.Error())
	}
}

// saveBeforeStop runs `save-all flush` on a server that is about to be scaled to
// zero. The SIGTERM that follows makes the server save again on its way out, but
// that save races the grace period: a large world killed mid-save rolls back to
// whatever was last flushed. Flushing first leaves the shutdown save with almost
// nothing to write.
//
// It is best-effort and never holds up the stop. A server without RCON, or with no
// ready pod (still booting, or already terminating), has nothing to flush it with,
// and a failed save still leaves the shutdown save; holding a stop the user asked
// for over it would only keep the server up. A conflict on the Update that follows
// re-runs it on the next reconcile, which is harmless: a second flush right after
// the first writes nothing.
func (r *Reconciler) saveBeforeStop(ctx context.Context, server *v1alpha1.MinecraftServer, sts *appsv1.StatefulSet) {
	if !server.Spec.Rcon.Enabled || sts.Status.ReadyReplicas == 0 {
		return
	}
	logger := ctrl.LoggerFrom(ctx)
	password, err := r.rconPassword(ctx, server)
	if err != nil {
		logger.Info("skipping pre-stop world save: RCON password unavailable", "error", err.Error())
		return
	}
	start := time.Now()
	if err := r.Prober.Save(ctx, rconAddress(server), password); err != nil {
		logger.Info("pre-stop world save failed; stopping anyway, the server saves again on SIGTERM",
			"error", err.Error(), "elapsed", time.Since(start).Round(time.Millisecond).String())
		return
	}
	logger.Info("pre-stop world save done", "elapsed", time.Since(start).Round(time.Millisecond).String())
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
	err := r.secretReader().Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: ref.Name}, &existing)
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
			if gerr := r.secretReader().Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: ref.Name}, &winner); gerr != nil {
				return "", gerr
			}
			return rconStamp(winner.Data[ref.Key]), nil
		}
		return "", err
	}
	log.FromContext(ctx).Info("provisioned the RCON password Secret", "secret", ref.Name)
	r.event(server, corev1.EventTypeNormal, "RconSecretCreated", "created the RCON password Secret "+ref.Name)
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

func (r *Reconciler) secretReader() client.Reader {
	if r.Secrets != nil {
		return r.Secrets
	}
	return r.Client
}

func (r *Reconciler) podReader() client.Reader {
	if r.Pods != nil {
		return r.Pods
	}
	return r.Client
}

// replaceStalePod deletes a pod-0 that is not ready and was made from an older
// template than the StatefulSet's current one, for the StatefulSet to recreate
// from the new spec. The StatefulSet controller does not do it: under
// OrderedReady it rolls a pod only once that pod is Running and Ready, so a
// server that cannot start keeps crash-looping on the image or settings that
// broke it however its spec is corrected, until the next timed-out retry
// deletes the pod, or forever once the retries are spent. A ready pod is left
// to the controller's own rolling update, and one already terminating to its
// deletion. It acts only on a StatefulSet status that has observed the current
// template, so the update revision it compares against is the current one.
func (r *Reconciler) replaceStalePod(ctx context.Context, server *v1alpha1.MinecraftServer, sts *appsv1.StatefulSet) (bool, error) {
	update := sts.Status.UpdateRevision
	if update == "" || sts.Status.ObservedGeneration < sts.Generation {
		return false, nil
	}
	var pod corev1.Pod
	if err := r.podReader().Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: server.Name + "-0"}, &pod); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if pod.DeletionTimestamp != nil || pod.Labels[appsv1.ControllerRevisionHashLabelKey] == update || podReady(&pod) {
		return false, nil
	}
	// The UID precondition: a pod the StatefulSet already recreated is not
	// deleted in its place.
	if err := r.Delete(ctx, &pod, client.Preconditions{UID: &pod.UID}); err != nil {
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			return false, nil
		}
		return false, err
	}
	log.FromContext(ctx).Info("recreated a pod left on an old spec", "pod", pod.Name, "revision", pod.Labels[appsv1.ControllerRevisionHashLabelKey], "updateRevision", update)
	r.event(server, corev1.EventTypeNormal, "PodReplaced", "the spec changed while the pod was not ready; recreated the pod from the new spec")
	return true, nil
}

func podReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (r *Reconciler) rconPassword(ctx context.Context, server *v1alpha1.MinecraftServer) (string, error) {
	ref := server.Spec.Rcon.SecretRef
	if ref.Name == "" || ref.Key == "" {
		return "", fmt.Errorf("rcon.secretRef.name and .key are required when rcon is enabled")
	}
	var secret corev1.Secret
	if err := r.secretReader().Get(ctx, types.NamespacedName{Namespace: server.Namespace, Name: ref.Name}, &secret); err != nil {
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
// immutable and must not be re-sent). A running server whose template changed
// only in the felis image keeps its template (PodTemplateAnnotation).
func (r *Reconciler) applyStatefulSet(ctx context.Context, desired *appsv1.StatefulSet) error {
	stamp, err := podTemplateStamp(&desired.Spec.Template, r.FelisImage)
	if err != nil {
		return err
	}
	if desired.Annotations == nil {
		desired.Annotations = map[string]string{}
	}
	desired.Annotations[PodTemplateAnnotation] = stamp

	var existing appsv1.StatefulSet
	err = r.Get(ctx, client.ObjectKeyFromObject(desired), &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	running := existing.Spec.Replicas != nil && *existing.Spec.Replicas > 0
	existing.Labels = desired.Labels
	existing.Spec.Replicas = desired.Spec.Replicas
	if running && existing.Annotations[PodTemplateAnnotation] == stamp {
		return r.Update(ctx, &existing)
	}
	existing.Spec.Template = desired.Spec.Template
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	existing.Annotations[PodTemplateAnnotation] = stamp
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
	// A server back in Starting is on its way to a new ready: a pod that dropped
	// out under a Running server, a retry after Failed, an auto-restart. Keeping
	// the last run's ready time would make markRunningReady treat the start as
	// already observed, and every start after the first would go unmeasured.
	server.Status.ReadySignalAt = nil
	// The empty clock belongs to a run too: the last run's would stop the new one at
	// its first zero, before anyone it came up for is in.
	server.Status.EmptySince = nil
	server.Status.Endpoint = v1alpha1.EndpointStatus{Mode: v1alpha1.EndpointFallback, Address: server.Spec.FallbackServer}
	server.Status.LiveMotd = server.Spec.Motd.Starting
	r.setCondition(server, v1alpha1.ConditionReady, metav1.ConditionFalse, reason, msg)
	r.setCondition(server, v1alpha1.ConditionRconReached, metav1.ConditionFalse, reason, msg)
}

func (r *Reconciler) markRunningReady(server *v1alpha1.MinecraftServer, players PlayerCount, endpointAddress string) {
	server.Status.AutoRestarts = 0
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
	// The run ends here. A wake can land before the scale-down completes and bring the
	// pod straight back: the idle stop's stale clock would then stop it again at its
	// first zero, and the players the wake is for get their arrival window.
	server.Status.EmptySince = nil
	server.Status.ReadySignalAt = nil
	server.Status.Ready = false
	server.Status.ObservedGeneration = server.Generation
	server.Status.Endpoint = v1alpha1.EndpointStatus{Mode: v1alpha1.EndpointFallback, Address: server.Spec.FallbackServer}
	server.Status.LiveMotd = server.Spec.Motd.Stopped
	r.setCondition(server, v1alpha1.ConditionReady, metav1.ConditionFalse, "Stopping", "scaling down")
}

func (r *Reconciler) markStopped(server *v1alpha1.MinecraftServer) {
	server.Status.AutoRestarts = 0
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
	server.Status.LiveMotd = server.Spec.Motd.Failed
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
	return r.now().Time.Sub(server.Status.StartRequestedAt.Time) >= startupTimeout(server)
}

func startupTimeout(server *v1alpha1.MinecraftServer) time.Duration {
	if t := time.Duration(server.Spec.Startup.TimeoutSeconds) * time.Second; t > 0 {
		return t
	}
	return defaultTimeoutSeconds * time.Second
}

func (r *Reconciler) readinessTimedOut(server *v1alpha1.MinecraftServer) bool {
	if server.Status.StartRequestedAt == nil {
		return false
	}
	return r.now().Time.Sub(server.Status.StartRequestedAt.Time) >= readinessTimeout(server)
}

func readinessTimeout(server *v1alpha1.MinecraftServer) time.Duration {
	if t := time.Duration(server.Spec.Startup.ReadinessTimeoutSeconds) * time.Second; t > 0 {
		return t
	}
	return defaultReadinessTimeoutSec * time.Second
}

// startingRequeue paces a start: every 2s while Starting; once Failed, only
// when the next auto-restart falls due, or every requeueFailed when the
// attempts are spent.
func (r *Reconciler) startingRequeue(server *v1alpha1.MinecraftServer, timeout time.Duration) time.Duration {
	if server.Status.Phase != v1alpha1.PhaseFailed {
		return requeueStarting
	}
	n := server.Status.AutoRestarts
	if n >= maxAutoRestarts || server.Status.StartRequestedAt == nil {
		return requeueFailed
	}
	wait := server.Status.StartRequestedAt.Add(timeout + autoRestartBaseBackoff<<n).Sub(r.now().Time)
	return max(wait, requeueStarting)
}

func (r *Reconciler) noteProbeFailure(key types.NamespacedName) int {
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	if r.probeFailures == nil {
		r.probeFailures = map[types.NamespacedName]int{}
	}
	r.probeFailures[key]++
	return r.probeFailures[key]
}

func (r *Reconciler) clearProbeFailures(key types.NamespacedName) {
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	delete(r.probeFailures, key)
}

// recoverFailedStart retries a start that timed out. Once a backoff past the
// timeout has elapsed (1m, then 2m, then 4m), it deletes the pod so the
// StatefulSet recreates it, counts the attempt in Status.AutoRestarts and
// re-anchors the start timeouts. After maxAutoRestarts the server stays Failed
// for a human; reaching Ready or stopping resets the count.
func (r *Reconciler) recoverFailedStart(ctx context.Context, server *v1alpha1.MinecraftServer, timeout time.Duration) error {
	n := server.Status.AutoRestarts
	if n >= maxAutoRestarts || server.Status.StartRequestedAt == nil {
		return nil
	}
	due := server.Status.StartRequestedAt.Add(timeout + autoRestartBaseBackoff<<n)
	if r.now().Time.Before(due) {
		return nil
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: server.Name + "-0", Namespace: server.Namespace}}
	if err := r.Delete(ctx, pod); client.IgnoreNotFound(err) != nil {
		return err
	}
	now := r.now()
	server.Status.AutoRestarts = n + 1
	server.Status.LastAutoRestartAt = &now
	server.Status.StartRequestedAt = nil
	r.markStarting(server, "AutoRestart", fmt.Sprintf("start timed out; recreated the pod (attempt %d of %d)", n+1, maxAutoRestarts))
	return nil
}
