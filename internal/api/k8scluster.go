package api

import (
	"context"
	"errors"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/naming"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// K8sCluster is the production Cluster backed by a controller-runtime client
// (spec §4). It reads the MinecraftServer CRD and performs the API's spec writes
// — the app-tier desiredState lever, the admin-tier create, and the admin-tier
// spec patch — each via a merge patch. It is integration-tested against a live
// cluster, not the hermetic api_test.go suite.
type K8sCluster struct {
	c         client.Client
	namespace string
	// now is injectable for the maintenance-lock tests; nil means time.Now.
	now func() time.Time
}

// NewK8sCluster builds a Cluster over c, scoped to namespace.
func NewK8sCluster(c client.Client, namespace string) *K8sCluster {
	return &K8sCluster{c: c, namespace: namespace}
}

func (k *K8sCluster) Ping(ctx context.Context) error {
	var list v1alpha1.MinecraftServerList
	return k.c.List(ctx, &list, client.InNamespace(k.namespace), client.Limit(1))
}

func (k *K8sCluster) GetServer(ctx context.Context, name string) (*ServerInfo, error) {
	var ms v1alpha1.MinecraftServer
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: name}, &ms); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return serverInfo(&ms), nil
}

// WorldVolumeExists reads the world PVC the operator's StatefulSet
// volumeClaimTemplate creates (naming.WorldPVCName — the same name the backup
// and restore Jobs mount), so existence here is exactly existence at Job mount
// time. NotFound is (false, nil): the caller refuses with a specific 409.
func (k *K8sCluster) WorldVolumeExists(ctx context.Context, name string) (bool, error) {
	var pvc corev1.PersistentVolumeClaim
	err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: naming.WorldPVCName(name)}, &pvc)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// PodImages lists the image of every container and init container of every pod
// in the namespace, for the registry pruner (cmd/felis inUseImageRefs). It is
// one list call on the pods:list grant the console already holds.
func (k *K8sCluster) PodImages(ctx context.Context) ([]string, error) {
	var pods corev1.PodList
	if err := k.c.List(ctx, &pods, client.InNamespace(k.namespace)); err != nil {
		return nil, err
	}
	var images []string
	for _, p := range pods.Items {
		for _, c := range p.Spec.InitContainers {
			images = append(images, c.Image)
		}
		for _, c := range p.Spec.Containers {
			images = append(images, c.Image)
		}
	}
	return images, nil
}

func (k *K8sCluster) GetBySubdomain(ctx context.Context, subdomain string) (*ServerInfo, error) {
	var list v1alpha1.MinecraftServerList
	if err := k.c.List(ctx, &list, client.InNamespace(k.namespace)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Spec.Subdomain == subdomain {
			return serverInfo(&list.Items[i]), nil
		}
	}
	return nil, ErrNotFound
}

func (k *K8sCluster) ListServers(ctx context.Context) ([]ServerInfo, error) {
	var list v1alpha1.MinecraftServerList
	if err := k.c.List(ctx, &list, client.InNamespace(k.namespace)); err != nil {
		return nil, err
	}
	out := make([]ServerInfo, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, *serverInfo(&list.Items[i]))
	}
	return out, nil
}

// CreateServer creates a MinecraftServer CRD from the validated §15 form. The
// server starts DesiredState=Stopped (created cold, woken later) and unowned —
// ownership is established by a later claim (spec §9.3). felis-api has already
// guaranteed the §22 memory ceiling lives in in.Resources, so the operator
// never has to derive a cgroup limit from JavaMemory. An existing name maps to
// ErrConflict so the handler returns 409.
//
// Every user server falls back to the login gate while it is stopped or starting
// — never to the lobby. Routing a fresh connection to the lobby would drop the
// player past authentication; falling back to login keeps the gate in front of
// them (and if login itself is down the proxy refuses, which is the intended
// "rather unreachable than unauthenticated" trade-off).
func (k *K8sCluster) CreateServer(ctx context.Context, in CreateServerInput) error {
	ms := &v1alpha1.MinecraftServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      in.Name,
			Namespace: k.namespace,
		},
		Spec: v1alpha1.MinecraftServerSpec{
			Subdomain:       in.Subdomain,
			DisplayName:     in.DisplayName,
			Image:           in.Image,
			JavaMemory:      in.JavaMemory,
			DesiredState:    v1alpha1.DesiredStopped,
			AutostartPolicy: in.AutostartPolicy,
			FallbackServer:  naming.SystemLoginServer,
			Storage:         v1alpha1.StorageSpec{Size: in.StorageSize},
			Resources:       in.Resources,
			// A server nobody plays on stops itself; the next join wakes it.
			Idle: v1alpha1.DefaultIdle(),
			// RCON is what makes a server manageable at all: the operator gates
			// phase=Running on the probe and samples the player tally from it (spec
			// §5), and every write — console commands, the LuckPerms grants behind the
			// permissions UI — travels over it (spec §8 写=RCON). Leaving it unset
			// produced a server that looked Running, reported nobody online, and
			// answered the console with 503; enabling it here is the fix for all
			// three. Port stays 0 so the operator applies its own default rather than
			// this package pinning a second copy of it. The password is not set (and
			// felis-api could not set it — it holds secrets:get, not create): the
			// operator mints it into this Secret on first reconcile, and felis-api
			// only ever reads it back at command time.
			Rcon: v1alpha1.RconSpec{
				Enabled: true,
				SecretRef: v1alpha1.SecretKeyRef{
					Name: naming.RconSecretName(in.Name),
					Key:  naming.RconSecretKey,
				},
			},
		},
	}
	if err := k.c.Create(ctx, ms); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return ErrConflict
		}
		return err
	}
	return nil
}

// SetDesiredState patches spec.desiredState with a merge patch so concurrent
// status writes by the operator are never clobbered (spec §9.1). A stop always
// goes through. A start goes through start, which refuses while a maintenance
// operation holds the world volume.
func (k *K8sCluster) SetDesiredState(ctx context.Context, name string, state v1alpha1.DesiredState) error {
	if state == v1alpha1.DesiredRunning {
		return k.start(ctx, name)
	}
	var ms v1alpha1.MinecraftServer
	if err := k.getServer(ctx, name, &ms); err != nil {
		return err
	}
	patch := client.MergeFrom(ms.DeepCopy())
	ms.Spec.DesiredState = state
	return k.c.Patch(ctx, &ms, patch)
}

// start flips desiredState to Running unless a restore, backup or file write
// holds the world volume (internal/maintenance), in which case it returns a
// *MaintenanceBusyError. The patch carries the resourceVersion it checked
// against, the same as AcquireMaintenance's: whichever of a racing wake and
// admission writes second gets a conflict, re-reads, and sees the other.
func (k *K8sCluster) start(ctx context.Context, name string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ms v1alpha1.MinecraftServer
		if err := k.getServer(ctx, name, &ms); err != nil {
			return err
		}
		kind, held, err := k.maintenanceHolder(ctx, &ms)
		if err != nil {
			return err
		}
		if held {
			return &MaintenanceBusyError{Kind: kind}
		}
		patch := client.MergeFromWithOptions(ms.DeepCopy(), client.MergeFromWithOptimisticLock{})
		ms.Spec.DesiredState = v1alpha1.DesiredRunning
		// A lock still on the object here no longer holds anything (Holder said
		// so): drop it in the same write.
		delete(ms.Annotations, maintenance.Annotation)
		return k.c.Patch(ctx, &ms, patch)
	})
}

// AcquireMaintenance admits one world-volume operation of the given kind: the
// server must be fully stopped (desiredState Stopped, phase Stopped, and no game
// pod left, so a pod still saving on its way down is waited out) and nothing else
// may hold the volume. Admission writes the maintenance lock under the resourceVersion it
// checked; the caller creates its Job and then calls ReleaseMaintenance, after
// which the Job itself is the lock.
func (k *K8sCluster) AcquireMaintenance(ctx context.Context, name, kind string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ms v1alpha1.MinecraftServer
		if err := k.getServer(ctx, name, &ms); err != nil {
			return err
		}
		desired := ms.Spec.DesiredState
		if desired == "" {
			desired = v1alpha1.DesiredStopped
		}
		if desired != v1alpha1.DesiredStopped || ms.Status.Ready || ms.Status.Phase != v1alpha1.PhaseStopped {
			return ErrNotStopped
		}
		if up, err := k.gamePodExists(ctx, name); err != nil {
			return err
		} else if up {
			return ErrNotStopped
		}
		holder, held, err := k.maintenanceHolder(ctx, &ms)
		if err != nil {
			return err
		}
		if held {
			return &MaintenanceBusyError{Kind: holder}
		}
		patch := client.MergeFromWithOptions(ms.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if ms.Annotations == nil {
			ms.Annotations = map[string]string{}
		}
		ms.Annotations[maintenance.Annotation] = maintenance.LockValue(kind, k.clock())
		return k.c.Patch(ctx, &ms, patch)
	})
}

// ReleaseMaintenance drops the admission lock. It is called once the Job exists
// (or failed to be created); a lock that is never released stops holding after
// maintenance.Grace on its own.
func (k *K8sCluster) ReleaseMaintenance(ctx context.Context, name string) error {
	var ms v1alpha1.MinecraftServer
	if err := k.getServer(ctx, name, &ms); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if _, ok := ms.Annotations[maintenance.Annotation]; !ok {
		return nil
	}
	patch := client.MergeFrom(ms.DeepCopy())
	delete(ms.Annotations, maintenance.Annotation)
	return k.c.Patch(ctx, &ms, patch)
}

// maintenanceHolder reads what holds ms's world volume right now. The Jobs are
// listed through the same direct client as the object, so a Job created before
// the lock was released is always visible here.
func (k *K8sCluster) maintenanceHolder(ctx context.Context, ms *v1alpha1.MinecraftServer) (string, bool, error) {
	var jobs batchv1.JobList
	if err := k.c.List(ctx, &jobs, client.InNamespace(k.namespace),
		client.MatchingLabels{maintenance.LabelServer: ms.Name}); err != nil {
		return "", false, err
	}
	kind, held := maintenance.Holder(ms.Name, ms.Annotations, jobs.Items, k.clock())
	return kind, held, nil
}

// gamePodComponent is the operator's component label value on a game server's
// pod (internal/operator.ComponentValue; k8scluster_test pins the two).
const gamePodComponent = "server"

// gamePodExists reports whether the server's game pod still exists, terminating
// or not. Phase Stopped is the operator's reading of the StatefulSet's replica
// counts; the pod object itself is the ground truth for "is anything of the
// server still running its preStop save against the volume".
func (k *K8sCluster) gamePodExists(ctx context.Context, name string) (bool, error) {
	var pods corev1.PodList
	if err := k.c.List(ctx, &pods, client.InNamespace(k.namespace), client.MatchingLabels{
		v1alpha1.LabelServer: name, v1alpha1.LabelComponent: gamePodComponent,
	}); err != nil {
		return false, err
	}
	return len(pods.Items) > 0, nil
}

func (k *K8sCluster) getServer(ctx context.Context, name string, ms *v1alpha1.MinecraftServer) error {
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: name}, ms); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func (k *K8sCluster) clock() time.Time {
	if k.now != nil {
		return k.now()
	}
	return time.Now()
}

// PatchServerSpec applies the admin-tier spec mutation (spec §7) with the same
// merge-patch discipline as SetDesiredState: read, copy, mutate only the fields
// the admin set, patch — so an operator status write racing in parallel survives.
// felis-api has already validated every field and resolved the §22 ceiling, so
// here we only translate the non-nil patch fields onto the live spec.
func (k *K8sCluster) PatchServerSpec(ctx context.Context, name string, p ServerSpecPatch) error {
	var ms v1alpha1.MinecraftServer
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: name}, &ms); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	patch := client.MergeFrom(ms.DeepCopy())
	if p.DisplayName != nil {
		ms.Spec.DisplayName = *p.DisplayName
	}
	if p.AutostartPolicy != nil {
		ms.Spec.AutostartPolicy = *p.AutostartPolicy
	}
	if p.Image != nil {
		ms.Spec.Image = *p.Image
	}
	if p.JavaMemory != nil {
		ms.Spec.JavaMemory = *p.JavaMemory
	}
	if p.Resources != nil {
		ms.Spec.Resources = *p.Resources
	}
	if p.IdleStopSeconds != nil {
		// Off keeps the duration (or the default) on the spec, which is what
		// marks it as a choice: converge only fills a server with none at all.
		if *p.IdleStopSeconds == 0 {
			ms.Spec.Idle.AutoStopEnabled = false
			if ms.Spec.Idle.EmptySecondsBeforeStop <= 0 {
				ms.Spec.Idle.EmptySecondsBeforeStop = v1alpha1.DefaultEmptySecondsBeforeStop
			}
		} else {
			ms.Spec.Idle = v1alpha1.IdleSpec{AutoStopEnabled: true, EmptySecondsBeforeStop: *p.IdleStopSeconds}
		}
	}
	return k.c.Patch(ctx, &ms, patch)
}

// serverInfo projects a MinecraftServer onto the API's lifecycle view.
func serverInfo(ms *v1alpha1.MinecraftServer) *ServerInfo {
	var cpuStr string
	if ms.Spec.Resources.Limits != nil {
		if limit, ok := ms.Spec.Resources.Limits[corev1.ResourceCPU]; ok {
			cpuStr = limit.String()
		}
	}

	return &ServerInfo{
		Name:            ms.Name,
		Subdomain:       ms.Spec.Subdomain,
		Phase:           string(ms.Status.Phase),
		Ready:           ms.Status.Ready,
		AutostartPolicy: string(ms.Spec.AutostartPolicy),
		DesiredState:    string(ms.Spec.DesiredState),
		EndpointMode:    string(ms.Status.Endpoint.Mode),
		EndpointAddress: ms.Status.Endpoint.Address,
		PlayersOnline:   ms.Status.Players.Online,
		PlayersMax:      ms.Status.Players.Max,
		DisplayName:     ms.Spec.DisplayName,
		Image:           ms.Spec.Image,
		JavaMemory:      ms.Spec.JavaMemory,
		StorageSize:     ms.Spec.Storage.Size,
		CPU:             cpuStr,
		IdleStopSeconds: idleStopSeconds(ms),
		PlayerCountUnknown: ms.Status.Phase == v1alpha1.PhaseRunning &&
			meta.IsStatusConditionFalse(ms.Status.Conditions, v1alpha1.ConditionPlayersCounted),
	}
}

// idleStopSeconds is the effective idle auto-stop duration, 0 when the server
// never idles out. It mirrors the operator's own rule: RCON must be on (the
// count comes from it) and system servers are exempt.
func idleStopSeconds(ms *v1alpha1.MinecraftServer) int32 {
	if !ms.Spec.Rcon.Enabled || ms.Labels[v1alpha1.LabelSystemRole] != "" ||
		!ms.Spec.Idle.AutoStopEnabled || ms.Spec.Idle.EmptySecondsBeforeStop <= 0 {
		return 0
	}
	return ms.Spec.Idle.EmptySecondsBeforeStop
}
