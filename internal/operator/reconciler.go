// Package operator reconciles MinecraftServer lifecycle state into managed workloads.
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
	networkingv1 "k8s.io/api/networking/v1"
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

const (
	requeueStarting                 = 2 * time.Second // readiness probe interval while Starting
	requeueStopping                 = 5 * time.Second
	requeueSecret                   = 10 * time.Second
	requeueIdleProbe                = 30 * time.Second
	requeueRunningProbe             = 60 * time.Second
	requeueProbeRetry               = 10 * time.Second // retry interval after a Running probe miss
	runningProbeMissesBeforeDegrade = 3                // consecutive misses before readiness demotion
	requeueFailed                   = 5 * time.Minute
	requeueMaintenance              = 5 * time.Second
	defaultTimeoutSeconds           = 300
	maxAutoRestarts                 = v1alpha1.MaxAutoRestarts
	autoRestartBaseBackoff          = time.Minute // initial restart backoff, doubled per attempt
	defaultReadinessTimeoutSec      = 300
	// Ignore empty samples during arrivalWindow so initial joining players do not trigger idle shutdown.
	arrivalWindow = 3 * time.Minute
)

// RconSecretAnnotation fingerprints the password to restart Pods when the Secret is reprovisioned.
const RconSecretAnnotation = "felis.lolicon.best/rcon-secret"

func rconStamp(password []byte) string {
	sum := sha256.Sum256(password)
	return hex.EncodeToString(sum[:8])
}

// PodTemplateAnnotation excludes platform image and stdin changes so upgrades wait for the next start.
const PodTemplateAnnotation = "felis.lolicon.best/pod-template"

func podTemplateStamp(tmpl *corev1.PodTemplateSpec, felisImage string) (string, error) {
	t := tmpl.DeepCopy()
	for i := range t.Spec.InitContainers {
		if felisImage != "" && t.Spec.InitContainers[i].Image == felisImage {
			t.Spec.InitContainers[i].Image = ""
		}
	}
	for i := range t.Spec.Containers {
		if t.Spec.Containers[i].Name == containerName {
			t.Spec.Containers[i].Stdin = false
		}
	}
	b, err := json.Marshal(t)
	if err != nil {
		return "", fmt.Errorf("fingerprint pod template: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]), nil
}

type Reconciler struct {
	Nodes          client.Reader
	EgressProbe    string
	ControllerNode string
	// ControllerHA allows unpinned replicated system servers to move between controllers.
	ControllerHA bool
	client.Client
	Scheme        *runtime.Scheme
	Prober        Prober
	FelisImage    string
	ArtifactCache string
	Now           func() metav1.Time // nil uses metav1.Now
	// Jobs must be uncached for maintenance checks; nil skips them.
	Jobs client.Reader
	// Secrets avoids a Secret informer; nil falls back to the embedded client.
	Secrets client.Reader
	// Pods avoids a Pod informer; nil falls back to the embedded client.
	Pods    client.Reader
	Volumes client.Reader
	// ExtraResources confirms port cleanup against the API server before ownership can be released.
	ExtraResources client.Reader
	// BootID tells host restarts from crashes; nil leaves every restart a crash.
	BootID   BootIDFunc
	Recorder record.EventRecorder // nil disables Events
	Watch    *ReconcileWatch      // nil disables liveness tracking

	// probeFailures is process-local and resets on success, stop or deletion.
	probeMu       sync.Mutex
	probeFailures map[types.NamespacedName]int
}

func (r *Reconciler) now() metav1.Time {
	if r.Now != nil {
		return r.Now()
	}
	return metav1.Now()
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.ExtraResources == nil {
		r.ExtraResources = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.MinecraftServer{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.NetworkPolicy{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		Complete(r)
}

const maxConcurrentReconciles = 4

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
	prevGaveUp := v1alpha1.StartGaveUp(&server.Status)
	if _, killing := server.Annotations[v1alpha1.AnnotationKill]; killing {
		if err := r.takeKillRequest(ctx, &server, desired); err != nil {
			return ctrl.Result{}, err
		}
	}
	if blocked, err := r.reconcileDiskQuota(ctx, &server); err != nil {
		return ctrl.Result{}, err
	} else if blocked && desired == v1alpha1.DesiredRunning {
		return ctrl.Result{RequeueAfter: requeueStopping}, nil
	}
	for _, annotation := range []string{v1alpha1.AnnotationStartRetry, v1alpha1.AnnotationRestart} {
		if _, ok := server.Annotations[annotation]; ok {
			if wait, err := r.takeRestartRequest(ctx, &server, desired, annotation); err != nil || wait {
				return ctrl.Result{RequeueAfter: requeueStarting}, err
			}
		}
	}
	var res ctrl.Result
	var err error
	if desired == v1alpha1.DesiredStopped {
		r.clearProbeFailures(req.NamespacedName)
		res, err = r.reconcileStopped(ctx, &server)
	} else {
		res, err = r.reconcileRunning(ctx, &server)
	}
	if err == nil {
		r.recordTransition(ctx, &server, prevPhase, prevRestarts)
		if r.noteLifecycle(&server, prevPhase, prevGaveUp) {
			err = r.patchStatus(ctx, &server)
		}
	}
	if extraErr := r.reconcileExtraPorts(ctx, &server); extraErr != nil {
		log.FromContext(ctx).Error(extraErr, "could not synchronize extra ports")
		if res.RequeueAfter == 0 || res.RequeueAfter > extraPortsRetry {
			res.RequeueAfter = extraPortsRetry
		}
	}
	return res, err
}

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

func (r *Reconciler) takeKillRequest(ctx context.Context, server *v1alpha1.MinecraftServer, desired v1alpha1.DesiredState) error {
	if desired == v1alpha1.DesiredStopped {
		var sts appsv1.StatefulSet
		err := r.Get(ctx, client.ObjectKeyFromObject(server), &sts)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		// Scale down before deletion so the StatefulSet cannot replace the killed pod.
		if err == nil && (sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0) {
			zero := int32(0)
			sts.Spec.Replicas = &zero
			if err := r.Update(ctx, &sts); err != nil {
				return err
			}
		}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: server.Name + "-0", Namespace: server.Namespace}}
		if err := r.Delete(ctx, pod, client.GracePeriodSeconds(0)); client.IgnoreNotFound(err) != nil {
			return err
		}
		if server.Status.StopNoticeAt != nil {
			server.Status.StopNoticeAt = nil
			if err := r.patchStatus(ctx, server); err != nil {
				return err
			}
		}
	}
	patch := client.MergeFromWithOptions(server.DeepCopy(), client.MergeFromWithOptimisticLock{})
	delete(server.Annotations, v1alpha1.AnnotationKill)
	return r.Patch(ctx, server, patch)
}

func (r *Reconciler) event(server *v1alpha1.MinecraftServer, eventType, reason, msg string) {
	if r.Recorder != nil {
		r.Recorder.Event(server, eventType, reason, msg)
	}
}

func (r *Reconciler) reconcileRunning(ctx context.Context, server *v1alpha1.MinecraftServer) (ctrl.Result, error) {
	if a := server.Status.AutoStop; a != nil {
		if server.Generation < a.Generation {
			return ctrl.Result{}, r.applyAutoStop(ctx, server)
		}
		server.Status.AutoStop = nil
		server.Status.Crashes = 0
	}
	if server.Status.StopNoticeAt != nil {
		if err := r.callOffStop(ctx, server); err != nil {
			return ctrl.Result{}, err
		}
	}
	if kind, held, err := r.maintenanceHold(ctx, server); err != nil {
		return ctrl.Result{}, err
	} else if held {
		// Keep the start anchor unchanged during restore so maintenance time cannot exhaust startup timeout.
		server.Status.ObservedGeneration = server.Generation
		r.setCondition(server, v1alpha1.ConditionReady, metav1.ConditionFalse, "MaintenanceInProgress",
			"waiting for the "+kind+" on this server's world to finish before starting")
		if err := r.patchStatus(ctx, server); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeueMaintenance}, nil
	}

	pod, err := r.gamePod(ctx, server)
	if err != nil {
		return ctrl.Result{}, err
	}
	if stopping, err := r.observeRun(ctx, server, pod); err != nil || stopping {
		return ctrl.Result{}, err
	}

	if r.Nodes != nil {
		node := server.Spec.NodeName
		if pod != nil {
			server.Status.NodeName = pod.Spec.NodeName
			if node == "" {
				node = pod.Spec.NodeName
			}
		}
		if node == "" {
			node = r.ControllerNode
		}
		if node != "" && !r.floats(server) {
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

	// Create the RCON Secret before the StatefulSet to avoid CreateContainerConfigError from an unresolved secretKeyRef.
	passwordStamp, err := r.ensureRconSecret(ctx, server)
	if err != nil {
		r.markStarting(server, "RconSecretUnavailable", err.Error())
		if perr := r.patchStatus(ctx, server); perr != nil {
			return ctrl.Result{}, perr
		}
		return ctrl.Result{RequeueAfter: requeueSecret}, nil
	}

	desired, err := r.desiredStatefulSet(server, passwordStamp)
	if err != nil {
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

	if current.Status.ReadyReplicas < 1 || (pod != nil && pod.DeletionTimestamp != nil) {
		if replaced, err := r.replaceStalePod(ctx, server, &current); err != nil {
			return ctrl.Result{}, err
		} else if replaced {
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
			// Require consecutive probe misses before demoting a Running server.
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
		if pc.Known {
			r.setCondition(server, v1alpha1.ConditionPlayersCounted, metav1.ConditionTrue, "Counted", "RCON list reply read")
		} else {
			r.setCondition(server, v1alpha1.ConditionPlayersCounted, metav1.ConditionFalse, "ListUnreadable",
				"RCON list failed or its reply matched no known format; idle auto-stop is paused until the player count can be read")
		}
	}

	// Unknown player counts must neither start nor clear the empty-server timer.
	if players.Known && idleStopApplies(server) {
		if players.Online == 0 {
			if server.Status.EmptySince == nil {
				if now := r.now(); !arriving(server, now) {
					server.Status.EmptySince = &now
				}
			} else if r.now().Time.Sub(server.Status.EmptySince.Time).Seconds() >=
				float64(server.Spec.Idle.EmptySecondsBeforeStop) {
				if liveBackupFreezing(server, r.now().Time) {
					// Preserve EmptySince while live backup delays stopping so shutdown follows thaw.
					return ctrl.Result{RequeueAfter: liveBackupPoll}, nil
				}
				if err := r.stopFor(ctx, server, v1alpha1.StopIdle); err != nil {
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
	// Player joins do not modify the CRD; requeue at idle expiry or poll while players are online.
	if server.Spec.Rcon.Enabled && idleStopApplies(server) {
		if server.Status.EmptySince != nil {
			deadline := server.Status.EmptySince.Time.Add(time.Duration(server.Spec.Idle.EmptySecondsBeforeStop) * time.Second)
			if wait := deadline.Sub(r.now().Time); wait > 0 {
				return ctrl.Result{RequeueAfter: wait}, nil
			}
		}
		return ctrl.Result{RequeueAfter: requeueIdleProbe}, nil
	}
	return ctrl.Result{RequeueAfter: requeueRunningProbe}, nil
}

// System servers cannot idle-stop because shutting down the login gate would prevent player ingress.
func idleStopApplies(server *v1alpha1.MinecraftServer) bool {
	return server.Labels[v1alpha1.LabelSystemRole] == "" &&
		server.Spec.Idle.AutoStopEnabled && server.Spec.Idle.EmptySecondsBeforeStop > 0
}

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

	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 {
		if wait, err := r.noticeStop(ctx, server, &sts); err != nil || wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, err
		}
		// Wait for live-backup thaw so shutdown saving cannot tear the staged copy.
		if liveBackupFreezing(server, r.now().Time) {
			return ctrl.Result{RequeueAfter: liveBackupPoll}, nil
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

const StopNoticeWindow = 30 * time.Second

const (
	stopNoticeText    = "[Felis] 服务器将在 30 秒后关闭，请尽快找安全的地方 / This server stops in 30 seconds"
	stopNowText       = "[Felis] 正在保存世界并关闭服务器 / Saving the world and stopping now"
	stopCalledOffText = "[Felis] 关闭已取消 / The stop was called off"
)

// noticeStop returns zero when no players can be notified; idle stops do not wait for an undeliverable warning.
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

func (r *Reconciler) callOffStop(ctx context.Context, server *v1alpha1.MinecraftServer) error {
	r.broadcast(ctx, server, stopCalledOffText)
	server.Status.StopNoticeAt = nil
	return r.patchStatus(ctx, server)
}

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

func (r *Reconciler) saveBeforeStop(ctx context.Context, server *v1alpha1.MinecraftServer, sts *appsv1.StatefulSet) {
	if !server.Spec.Rcon.Enabled || sts.Status.ReadyReplicas == 0 {
		return
	}
	logger := ctrl.LoggerFrom(ctx)
	password, err := r.rconPassword(ctx, server)
	if err != nil {
		logger.Info("skipping pre-stop world save: RCON password unavailable")
		return
	}
	start := time.Now()
	if err := r.Prober.Save(ctx, rconAddress(server), password); err != nil {
		logger.Info("pre-stop world save failed; stopping anyway, the server saves again on SIGTERM",
			"elapsed", time.Since(start).Round(time.Millisecond).String())
	} else {
		logger.Info("pre-stop world save done", "elapsed", time.Since(start).Round(time.Millisecond).String())
	}
	if len(server.Spec.Lifecycle.StopCommands) == 0 {
		return
	}
	start = time.Now()
	if err := r.Prober.Commands(ctx, rconAddress(server), password, server.Spec.Lifecycle.StopCommands); err != nil {
		logger.Info("pre-stop commands failed; stopping anyway", "count", len(server.Spec.Lifecycle.StopCommands),
			"elapsed", time.Since(start).Round(time.Millisecond).String())
	}
}

// maintenanceHold gates only Pods about to be created, not workloads already scaled up.
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

// ensureRconSecret creates once and fingerprints the stored value; it never rotates existing credentials.
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
		// Use Data because StringData is write-only and reads back empty.
		Data: map[string][]byte{ref.Key: []byte(password)},
	}
	if err := controllerutil.SetControllerReference(server, secret, r.Scheme); err != nil {
		return "", err
	}
	if err := r.Create(ctx, secret); err != nil {
		// Read the concurrent creator's value before fingerprinting to avoid a redundant second Pod restart.
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

// replaceStalePod recreates not-ready Pods because OrderedReady cannot roll them to a repaired template on its own.
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
	// Require the checked UID so a concurrent StatefulSet replacement cannot be deleted.
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

// applyStatefulSet defers startup settings and platform defaults until the next start.
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
	if running {
		deferred := existing.Annotations[PodTemplateAnnotation] == stamp
		if !deferred {
			deferred, err = deferStartupTemplate(&existing, desired, r.FelisImage)
			if err != nil {
				return err
			}
		}
		if deferred {
			return r.Update(ctx, &existing)
		}
	}
	existing.Spec.Template = desired.Spec.Template
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	existing.Annotations[PodTemplateAnnotation] = stamp
	return r.Update(ctx, &existing)
}

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

func (r *Reconciler) markStarting(server *v1alpha1.MinecraftServer, reason, msg string) {
	server.Status.Phase = v1alpha1.PhaseStarting
	server.Status.Ready = false
	server.Status.ObservedGeneration = server.Generation
	// Set once per start attempt and clear on stop to anchor startup-duration measurement.
	if server.Status.StartRequestedAt == nil {
		t := r.now()
		server.Status.StartRequestedAt = &t
	}
	// Clear ready time so each new start is measured.
	server.Status.ReadySignalAt = nil
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
	// Retain the last displayed count when sampling fails.
	if players.Known {
		server.Status.Players = v1alpha1.PlayersStatus{Online: players.Online, Max: players.Max}
	}
	if server.Status.ReadySignalAt == nil {
		t := r.now()
		server.Status.ReadySignalAt = &t
		// A nil StartRequestedAt means the server never entered Starting.
		if server.Status.StartRequestedAt != nil {
			metrics.StartDurationSeconds.Observe(t.Sub(server.Status.StartRequestedAt.Time).Seconds())
		}
	}
	// Clear the anchor so later Pod failures receive a fresh startup budget.
	server.Status.StartRequestedAt = nil
	server.Status.Endpoint = v1alpha1.EndpointStatus{Mode: v1alpha1.EndpointDirect, Address: endpointAddress}
	server.Status.LiveMotd = server.Spec.Motd.Running
	r.setCondition(server, v1alpha1.ConditionRconReached, metav1.ConditionTrue, "Probed", "RCON probe succeeded")
	r.setCondition(server, v1alpha1.ConditionReady, metav1.ConditionTrue, "RconReached", "server is accepting RCON")
	r.setCondition(server, v1alpha1.ConditionProvisioned, metav1.ConditionTrue, "Provisioned", "server is provisioned and accepting RCON")
}

func (r *Reconciler) markStopping(server *v1alpha1.MinecraftServer) {
	server.Status.Phase = v1alpha1.PhaseStopping
	// A wake during scale-down must not inherit the previous run's idle timer.
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
	server.Status.StartRequestedAt = nil
	server.Status.EmptySince = nil
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

// recoverFailedStart uses bounded exponential backoff and reanchors timeouts; exhausted attempts wait for explicit recovery.
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

// liveBackupWait exceeds the API freeze cap so abandoned freezes cannot block shutdown indefinitely.
const liveBackupWait = 6 * time.Minute

const liveBackupPoll = 2 * time.Second

func liveBackupFreezing(server *v1alpha1.MinecraftServer, now time.Time) bool {
	raw := server.Annotations[v1alpha1.AnnotationLiveBackup]
	if raw == "" {
		return false
	}
	var f struct {
		Started time.Time `json:"started"`
	}
	if err := json.Unmarshal([]byte(raw), &f); err != nil || f.Started.IsZero() {
		return false
	}
	return now.Sub(f.Started) < liveBackupWait
}

func (r *Reconciler) floats(server *v1alpha1.MinecraftServer) bool {
	return r.ControllerHA && server.Labels[v1alpha1.LabelSystemRole] != "" && server.Spec.NodeName == ""
}
