package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	"felis.lolicon.best/internal/maintenance"
	"felis.lolicon.best/internal/naming"
	"felis.lolicon.best/internal/placement"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// K8sCluster caches fleet reads but uses direct reads before mutations so writes do not act on stale informer state.
type K8sCluster struct {
	NodeMaintenanceGuard func(context.Context) error
	DiskWorldResolver    placement.Resolver
	ExtraPortsPublicIP   string
	distributed          bool
	controller           string
	c                    client.Client
	namespace            string
	// nil uses the direct client
	servers client.Reader
	// nil means no cache
	synced func() bool
	// nil uses time.Now
	now func() time.Time
}

// SubdomainIndex must be registered in the cache with SubdomainOf.
const SubdomainIndex = "spec.subdomain"

func SubdomainOf(o client.Object) []string {
	ms, ok := o.(*v1alpha1.MinecraftServer)
	if !ok || ms.Spec.Subdomain == "" {
		return nil
	}
	return []string{ms.Spec.Subdomain}
}

func NewK8sCluster(c client.Client, namespace string) *K8sCluster {
	return &K8sCluster{c: c, namespace: namespace}
}

// WithServerCache requires SubdomainIndex; Ping refuses readiness until synced reports the initial list.
func (k *K8sCluster) WithServerCache(servers client.Reader, synced func() bool) *K8sCluster {
	k.servers, k.synced = servers, synced
	return k
}

func (k *K8sCluster) fleet() client.Reader {
	if k.servers != nil {
		return k.servers
	}
	return k.c
}

func (k *K8sCluster) Ping(ctx context.Context) error {
	if k.synced != nil && !k.synced() {
		return errors.New("MinecraftServer cache has not synced")
	}
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
	info := serverInfo(&ms, k.clock())
	k.extraPortAddresses(ctx, &ms, info)
	info.Events = serverEvents(ms.Status.Events)
	if ms.Spec.DesiredState != v1alpha1.DesiredStopped && ms.Status.Phase != v1alpha1.PhaseStarting && ms.Status.Phase != v1alpha1.PhaseFailed && ms.Status.Phase != v1alpha1.PhaseRunning {
		return info, nil
	}
	var pods corev1.PodList
	if err := k.c.List(ctx, &pods, client.InNamespace(k.namespace), client.MatchingLabels{v1alpha1.LabelServer: name, v1alpha1.LabelComponent: gamePodComponent}); err != nil {
		return nil, err
	}
	if ms.Spec.DesiredState == v1alpha1.DesiredStopped {
		info.Phase, info.Ready = string(v1alpha1.PhaseStopped), false
		var sts appsv1.StatefulSet
		err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: name}, &sts)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		if len(pods.Items) > 0 || (err == nil && (sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0)) {
			info.Phase = string(v1alpha1.PhaseStopping)
		}
		return info, nil
	}
	diagnostic := startupStatus(&ms, pods.Items)
	if ms.Status.Phase != v1alpha1.PhaseRunning || diagnostic.Stage == "failed" || diagnostic.Stage == "scheduling" || diagnostic.Stage == "creating" {
		info.Startup = diagnostic
		if ms.Status.Phase == v1alpha1.PhaseRunning {
			info.Ready, info.Phase = false, string(v1alpha1.PhaseFailed)
		}
	}
	return info, nil
}

func (k *K8sCluster) GetStartupSettings(ctx context.Context, name string) (*ServerStartupSettings, error) {
	var ms v1alpha1.MinecraftServer
	if err := k.getServer(ctx, name, &ms); err != nil {
		return nil, err
	}
	return serverStartupSettings(&ms.Spec), nil
}

// WorldVolumeExists returns (false, nil) for a missing PVC.
func (k *K8sCluster) WorldVolumeExists(ctx context.Context, name string) (bool, error) {
	var ms v1alpha1.MinecraftServer
	err := k.getServer(ctx, name, &ms)
	claim := naming.WorldPVCName(name)
	if err == nil {
		claim = ms.WorldPVC()
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	var pvc corev1.PersistentVolumeClaim
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: claim}, &pvc); err == nil {
		return true, nil
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	if ms.Name == "" {
		var retained corev1.PersistentVolumeClaimList
		if err := k.c.List(ctx, &retained, client.InNamespace(k.namespace), client.MatchingLabels{v1alpha1.LabelServer: name}); err != nil {
			return false, err
		}
		return len(retained.Items) > 0, nil
	}
	return false, nil
}

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

// GetBySubdomain uses the cache index or lists the namespace because Kubernetes cannot select this CRD field directly.
func (k *K8sCluster) GetBySubdomain(ctx context.Context, subdomain string) (*ServerInfo, error) {
	var list v1alpha1.MinecraftServerList
	opts := []client.ListOption{client.InNamespace(k.namespace)}
	if k.servers != nil {
		opts = append(opts, client.MatchingFields{SubdomainIndex: subdomain})
	}
	if err := k.fleet().List(ctx, &list, opts...); err != nil {
		return nil, err
	}
	for i := range list.Items {
		if list.Items[i].Spec.Subdomain == subdomain {
			return serverInfo(&list.Items[i], k.clock()), nil
		}
	}
	return nil, ErrNotFound
}

func (k *K8sCluster) ListServers(ctx context.Context) ([]ServerInfo, error) {
	var list v1alpha1.MinecraftServerList
	if err := k.fleet().List(ctx, &list, client.InNamespace(k.namespace)); err != nil {
		return nil, err
	}
	out := make([]ServerInfo, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, *serverInfo(&list.Items[i], k.clock()))
	}
	return out, nil
}

// CreateServer routes stopped or starting servers through login, never directly to the lobby, to preserve authentication.
func (k *K8sCluster) CreateServer(ctx context.Context, in CreateServerInput) error {
	ms := &v1alpha1.MinecraftServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      in.Name,
			Namespace: k.namespace,
		},
		Spec: v1alpha1.MinecraftServerSpec{
			NodeName:        in.NodeName,
			Subdomain:       in.Subdomain,
			DisplayName:     in.DisplayName,
			Image:           in.Image,
			JavaMemory:      in.JavaMemory,
			DesiredState:    v1alpha1.DesiredStopped,
			AutostartPolicy: in.AutostartPolicy,
			FallbackServer:  naming.SystemLoginServer,
			Storage:         v1alpha1.StorageSpec{Size: in.StorageSize},
			Resources:       in.Resources,
			Software:        in.Software,
			Idle:            v1alpha1.DefaultIdle(),
			// Enable RCON for readiness and commands; the operator supplies the default port and creates the password Secret.
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

// start patches with the checked resourceVersion so a racing maintenance admission conflicts and is re-read.
func (k *K8sCluster) start(ctx context.Context, name string) error {
	return k.startWith(ctx, name, "")
}

func (k *K8sCluster) RetryStart(ctx context.Context, name string) error {
	return k.startWith(ctx, name, v1alpha1.AnnotationStartRetry)
}

func (k *K8sCluster) RestartServer(ctx context.Context, name string) error {
	return k.startWith(ctx, name, v1alpha1.AnnotationRestart)
}

func (k *K8sCluster) KillServer(ctx context.Context, name string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ms v1alpha1.MinecraftServer
		if err := k.getServer(ctx, name, &ms); err != nil {
			return err
		}
		patch := client.MergeFromWithOptions(ms.DeepCopy(), client.MergeFromWithOptimisticLock{})
		ms.Spec.DesiredState = v1alpha1.DesiredStopped
		if ms.Annotations == nil {
			ms.Annotations = map[string]string{}
		}
		if _, pending := ms.Annotations[v1alpha1.AnnotationKill]; !pending {
			ms.Annotations[v1alpha1.AnnotationKill] = k.clock().UTC().Format(time.RFC3339Nano)
		}
		delete(ms.Annotations, v1alpha1.AnnotationRestart)
		delete(ms.Annotations, v1alpha1.AnnotationStartRetry)
		return k.c.Patch(ctx, &ms, patch)
	})
}

func (k *K8sCluster) startWith(ctx context.Context, name, annotation string) error {
	if k.NodeMaintenanceGuard != nil {
		if err := k.NodeMaintenanceGuard(ctx); err != nil {
			return err
		}
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ms v1alpha1.MinecraftServer
		if err := k.getServer(ctx, name, &ms); err != nil {
			return err
		}
		if _, pending := ms.Annotations[v1alpha1.AnnotationKill]; pending {
			return newError(http.StatusConflict, "conflict", "wait for force stop to finish before starting")
		}
		if annotation == v1alpha1.AnnotationRestart && (ms.Spec.DesiredState != v1alpha1.DesiredRunning || ms.Status.Phase != v1alpha1.PhaseRunning) {
			return newError(http.StatusConflict, "not_running", "start the server before restarting it")
		}
		node := ms.Spec.NodeName
		if node == "" {
			node = ms.Status.NodeName
		}
		if node == "" {
			node = k.controller
		}
		if k.distributed && node != "" {
			var n corev1.Node
			if err := k.c.Get(ctx, types.NamespacedName{Name: node}, &n); err != nil {
				return err
			}
			if !placement.Admitted(&n, k.controller) || n.Spec.Unschedulable {
				return newError(503, "node_unavailable", "execution node is offline")
			}
		}
		kind, held, err := k.maintenanceHolder(ctx, &ms)
		if err != nil {
			return err
		}
		if held {
			return &MaintenanceBusyError{Kind: kind}
		}
		if err := k.checkDiskQuota(ctx, &ms); err != nil {
			return err
		}
		if ms.Spec.DesiredState == v1alpha1.DesiredRunning &&
			(ms.Annotations[v1alpha1.AnnotationRestart] != "" || ms.Annotations[v1alpha1.AnnotationStartRetry] != "") {
			return nil
		}
		patch := client.MergeFromWithOptions(ms.DeepCopy(), client.MergeFromWithOptimisticLock{})
		ms.Spec.DesiredState = v1alpha1.DesiredRunning
		delete(ms.Annotations, maintenance.Annotation)
		if annotation != "" {
			if ms.Annotations == nil {
				ms.Annotations = map[string]string{}
			}
			ms.Annotations[annotation] = k.clock().UTC().Format(time.RFC3339Nano)
		}
		if k.NodeMaintenanceGuard != nil {
			if err := k.NodeMaintenanceGuard(ctx); err != nil {
				return err
			}
		}
		return k.c.Patch(ctx, &ms, patch)
	})
}

// AcquireMaintenance persists the admission lock; after Job creation and ReleaseMaintenance, the Job holds the volume.
func (k *K8sCluster) AcquireMaintenance(ctx context.Context, name, kind string) error {
	if k.NodeMaintenanceGuard != nil {
		if err := k.NodeMaintenanceGuard(ctx); err != nil {
			return err
		}
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var ms v1alpha1.MinecraftServer
		if err := k.getServer(ctx, name, &ms); err != nil {
			return err
		}
		desired := ms.Spec.DesiredState
		if desired == "" {
			desired = v1alpha1.DesiredStopped
		}
		if kind != maintenance.KindConfigWrite || !naming.IsSystemServer(name) {
			if desired != v1alpha1.DesiredStopped || ms.Status.Ready || ms.Status.Phase != v1alpha1.PhaseStopped {
				return ErrNotStopped
			}
			if up, err := k.gamePodExists(ctx, name); err != nil {
				return err
			} else if up {
				return ErrNotStopped
			}
		}
		if ms.Annotations[v1alpha1.AnnotationRestart] != "" {
			return ErrMaintenanceInProgress
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

// ReleaseMaintenance drops admission locks, which also expire after maintenance.Grace if abandoned.
func (k *K8sCluster) ReleaseMaintenance(ctx context.Context, name string) error {
	var ms v1alpha1.MinecraftServer
	if err := k.getServer(ctx, name, &ms); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	if value := ms.Annotations[maintenance.Annotation]; strings.HasPrefix(value, maintenance.KindMigration+"@") {
		return nil
	}
	if _, ok := ms.Annotations[maintenance.Annotation]; !ok {
		return nil
	}
	patch := client.MergeFrom(ms.DeepCopy())
	delete(ms.Annotations, maintenance.Annotation)
	return k.c.Patch(ctx, &ms, patch)
}

func (k *K8sCluster) CheckFileAccess(ctx context.Context, name string) error {
	var ms v1alpha1.MinecraftServer
	if err := k.getServer(ctx, name, &ms); err != nil {
		return err
	}
	var jobs batchv1.JobList
	if err := k.c.List(ctx, &jobs, client.InNamespace(k.namespace), client.MatchingLabels{maintenance.LabelServer: name}); err != nil {
		return err
	}
	blocksFiles := func(kind string) bool {
		return kind != maintenance.KindFileWrite && kind != maintenance.KindConfigWrite && kind != maintenance.KindExport
	}
	for i := range jobs.Items {
		if kind, held := maintenance.Holder(name, nil, jobs.Items[i:i+1], k.clock()); held && blocksFiles(kind) {
			return &MaintenanceBusyError{Kind: kind}
		}
	}
	if kind, held := maintenance.Holder(name, ms.Annotations, nil, k.clock()); held && blocksFiles(kind) {
		return &MaintenanceBusyError{Kind: kind}
	}
	return nil
}

// maintenanceHolder uses direct Job reads so a Job created before lock release is visible.
func (k *K8sCluster) maintenanceHolder(ctx context.Context, ms *v1alpha1.MinecraftServer) (string, bool, error) {
	var jobs batchv1.JobList
	if err := k.c.List(ctx, &jobs, client.InNamespace(k.namespace),
		client.MatchingLabels{maintenance.LabelServer: ms.Name}); err != nil {
		return "", false, err
	}
	kind, held := maintenance.Holder(ms.Name, ms.Annotations, jobs.Items, k.clock())
	return kind, held, nil
}

// gamePodComponent must match internal/operator.ComponentValue.
const gamePodComponent = "server"

// gamePodExists includes terminating pods because preStop may still be saving after the CR phase becomes Stopped.
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

func (k *K8sCluster) PatchServerSpec(ctx context.Context, name string, p ServerSpecPatch) error {
	startup := p.setsStartupSettings()
	checked := startup || p.ExtraPorts != nil
	apply := func() error {
		var ms v1alpha1.MinecraftServer
		if err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: name}, &ms); err != nil {
			if apierrors.IsNotFound(err) {
				return ErrNotFound
			}
			return err
		}
		original := ms.DeepCopy()
		patch := client.MergeFrom(original)
		if checked {
			patch = client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})
		}
		if startup {
			if err := applyStartupSettings(&ms.Spec, p); err != nil {
				return err
			}
		}
		if p.DisplayName != nil {
			ms.Spec.DisplayName = *p.DisplayName
		}
		if p.AutostartPolicy != nil {
			ms.Spec.AutostartPolicy = *p.AutostartPolicy
		}
		if p.Image != nil {
			ms.Spec.Image = *p.Image
		}
		if p.Software != nil {
			s := *p.Software
			ms.Spec.Software = &s
		} else if p.ClearSoftware {
			ms.Spec.Software = nil
		}
		if p.JavaMemory != nil {
			ms.Spec.JavaMemory = *p.JavaMemory
		}
		if p.Resources != nil {
			ms.Spec.Resources = *p.Resources
		}
		if p.ResumeAfterHostRestart != nil {
			ms.Spec.Recovery.ResumeAfterHostRestart = p.ResumeAfterHostRestart
		}
		if p.RestartOnCrash != nil {
			ms.Spec.Recovery.RestartOnCrash = p.RestartOnCrash
		}
		if p.MaxCrashRestarts != nil {
			ms.Spec.Recovery.MaxCrashRestarts = *p.MaxCrashRestarts
		}
		if p.NotifyStarted != nil {
			ms.Spec.Notify.Started = *p.NotifyStarted
		}
		if p.NotifyStopped != nil {
			ms.Spec.Notify.Stopped = *p.NotifyStopped
		}
		if p.NotifyProblems != nil {
			ms.Spec.Notify.Problems = p.NotifyProblems
		}
		if p.IdleStopSeconds != nil {
			// Retain a disabled idle duration to distinguish an explicit choice from an unset setting during convergence.
			if *p.IdleStopSeconds == 0 {
				ms.Spec.Idle.AutoStopEnabled = false
				if ms.Spec.Idle.EmptySecondsBeforeStop <= 0 {
					ms.Spec.Idle.EmptySecondsBeforeStop = v1alpha1.DefaultEmptySecondsBeforeStop
				}
			} else {
				ms.Spec.Idle = v1alpha1.IdleSpec{AutoStopEnabled: true, EmptySecondsBeforeStop: *p.IdleStopSeconds}
			}
		}
		if p.ExtraPorts != nil {
			ms.Spec.ExtraPorts = append([]v1alpha1.ExtraPort(nil), (*p.ExtraPorts)...)
			if err := v1alpha1.ValidateExtraPorts(ms.Spec); err != nil {
				return newError(http.StatusBadRequest, "bad_extra_ports", "%s", err)
			}
		}
		return k.c.Patch(ctx, &ms, patch)
	}
	if !checked {
		return apply()
	}
	err := retry.RetryOnConflict(retry.DefaultRetry, apply)
	if apierrors.IsConflict(err) {
		return ErrConflict
	}
	return err
}

func serverInfo(ms *v1alpha1.MinecraftServer, now time.Time) *ServerInfo {
	var cpuStr, memStr string
	if limit, ok := ms.Spec.Resources.Limits[corev1.ResourceCPU]; ok {
		cpuStr = limit.String()
	}
	if limit, ok := ms.Spec.Resources.Limits[corev1.ResourceMemory]; ok {
		memStr = limit.String()
	}

	node := ms.Spec.NodeName
	if node == "" {
		node = ms.Status.NodeName
	}
	disk, diskFull := serverDiskUsage(ms, now)
	extraPorts, extraPortsPending := serverExtraPorts(ms)
	return &ServerInfo{
		UID:               string(ms.UID),
		Name:              ms.Name,
		NodeName:          node,
		Subdomain:         ms.Spec.Subdomain,
		Phase:             string(ms.Status.Phase),
		Ready:             ms.Status.Ready,
		AutostartPolicy:   string(ms.Spec.AutostartPolicy),
		DesiredState:      string(ms.Spec.DesiredState),
		EndpointMode:      string(ms.Status.Endpoint.Mode),
		EndpointAddress:   ms.Status.Endpoint.Address,
		PlayersOnline:     ms.Status.Players.Online,
		PlayersMax:        ms.Status.Players.Max,
		DisplayName:       ms.Spec.DisplayName,
		Image:             ms.Spec.Image,
		JavaMemory:        ms.Spec.JavaMemory,
		Memory:            memStr,
		StorageSize:       ms.Spec.Storage.Size,
		Disk:              disk,
		DiskFull:          diskFull,
		ExtraPorts:        extraPorts,
		ExtraPortsPending: extraPortsPending,
		CPU:               cpuStr,
		IdleStopSeconds:   idleStopSeconds(ms),
		PlayerCountUnknown: ms.Status.Phase == v1alpha1.PhaseRunning &&
			meta.IsStatusConditionFalse(ms.Status.Conditions, v1alpha1.ConditionPlayersCounted),
		LegacyForwarding: ms.Labels[v1alpha1.LabelForwarding] == v1alpha1.ForwardingLegacy,
		AutoRestarts:     ms.Status.AutoRestarts,
		StartGaveUp:      v1alpha1.StartGaveUp(&ms.Status),
		ReaperExempt:     ms.Spec.ReaperExempt,
		Resources:        *ms.Spec.Resources.DeepCopy(),
		Software:         serverSoftware(ms.Spec.Software),

		ResumeAfterHostRestart: ms.Spec.Recovery.ResumesAfterHostRestart(),
		RestartOnCrash:         ms.Spec.Recovery.RestartsOnCrash(),
		MaxCrashRestarts:       maxCrashRestarts(ms.Spec.Recovery),
		Notify:                 serverNotify(ms.Spec.Notify),
		RecentCrashes:          recentCrashes(&ms.Status),
		LastCrash:              crashInfo(ms.Status.LastCrash),
		AutoStop:               autoStopInfo(ms.Status.AutoStop),
		CrashStopped:           v1alpha1.CrashStopped(&ms.Status),
	}
}

// maxCrashRestarts keeps the limit visible while restarts are off, so turning them back on shows it.
func maxCrashRestarts(r v1alpha1.RecoverySpec) int32 {
	if r.MaxCrashRestarts > 0 {
		return r.MaxCrashRestarts
	}
	return v1alpha1.DefaultMaxCrashRestarts
}

func serverNotify(n v1alpha1.NotifySpec) ServerNotify {
	return ServerNotify{Started: n.Started, Stopped: n.Stopped, Problems: n.ProblemsOn()}
}

func recentCrashes(s *v1alpha1.MinecraftServerStatus) int32 {
	if s.LastCrash == nil || time.Since(s.LastCrash.At.Time) > v1alpha1.CrashWindow {
		return 0
	}
	return s.Crashes
}

func crashInfo(c *v1alpha1.CrashRecord) *CrashInfo {
	if c == nil {
		return nil
	}
	return &CrashInfo{At: c.At.Time, Reason: c.Reason, ExitCode: c.ExitCode}
}

func autoStopInfo(a *v1alpha1.AutoStop) *AutoStopInfo {
	if a == nil {
		return nil
	}
	return &AutoStopInfo{Reason: a.Reason, At: a.At.Time}
}

func serverEvents(in []v1alpha1.ServerEvent) []ServerEvent {
	if len(in) == 0 {
		return nil
	}
	out := make([]ServerEvent, len(in))
	for i, e := range in {
		out[i] = ServerEvent{Seq: e.Seq, Type: e.Type, At: e.At.Time, Reason: e.Reason, ExitCode: e.ExitCode, Crashes: e.Crashes, Limit: e.Limit, Stopping: e.Stopping}
	}
	return out
}

// AnnotationEventsMailed holds the last lifecycle event seq felis-api has mailed.
const AnnotationEventsMailed = v1alpha1.GroupName + "/events-mailed"

func (k *K8sCluster) ServerEventFeeds(ctx context.Context) ([]ServerEventFeed, error) {
	var list v1alpha1.MinecraftServerList
	if err := k.fleet().List(ctx, &list, client.InNamespace(k.namespace)); err != nil {
		return nil, err
	}
	out := make([]ServerEventFeed, 0, len(list.Items))
	for i := range list.Items {
		ms := &list.Items[i]
		if len(ms.Status.Events) == 0 {
			continue
		}
		mailed, _ := strconv.ParseInt(ms.Annotations[AnnotationEventsMailed], 10, 64)
		out = append(out, ServerEventFeed{
			Name: ms.Name, DisplayName: ms.Spec.DisplayName, System: ms.Labels[v1alpha1.LabelSystemRole] != "",
			Notify: serverNotify(ms.Spec.Notify), Mailed: mailed, Events: serverEvents(ms.Status.Events),
		})
	}
	return out, nil
}

func (k *K8sCluster) MarkEventsMailed(ctx context.Context, name string, seq int64) error {
	body, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{AnnotationEventsMailed: strconv.FormatInt(seq, 10)}}})
	if err != nil {
		return err
	}
	ms := &v1alpha1.MinecraftServer{ObjectMeta: metav1.ObjectMeta{Namespace: k.namespace, Name: name}}
	if err := k.c.Patch(ctx, ms, client.RawPatch(types.MergePatchType, body)); err != nil {
		if apierrors.IsNotFound(err) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func idleStopSeconds(ms *v1alpha1.MinecraftServer) int32 {
	if !ms.Spec.Rcon.Enabled || ms.Labels[v1alpha1.LabelSystemRole] != "" ||
		!ms.Spec.Idle.AutoStopEnabled || ms.Spec.Idle.EmptySecondsBeforeStop <= 0 {
		return 0
	}
	return ms.Spec.Idle.EmptySecondsBeforeStop
}

func (k *K8sCluster) WithDistributed(enabled bool, controller ...string) *K8sCluster {
	k.distributed = enabled
	if len(controller) > 0 {
		k.controller = controller[0]
	}
	return k
}

func (k *K8sCluster) EnsureWorldVolume(ctx context.Context, name string) error {
	var ms v1alpha1.MinecraftServer
	if err := k.getServer(ctx, name, &ms); err != nil {
		return err
	}
	var pvc corev1.PersistentVolumeClaim
	err := k.c.Get(ctx, types.NamespacedName{Namespace: k.namespace, Name: ms.WorldPVC()}, &pvc)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	size := ms.Spec.Storage.Size
	if size == "" {
		size = defaultWorldSize
	}
	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return fmt.Errorf("world size %q: %w", size, err)
	}
	pvc = corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: ms.WorldPVC(), Namespace: k.namespace, Labels: map[string]string{v1alpha1.LabelServer: name}},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: qty}},
		},
	}
	if sc := ms.Spec.Storage.StorageClassName; sc != "" {
		pvc.Spec.StorageClassName = &sc
	}
	return client.IgnoreAlreadyExists(k.c.Create(ctx, &pvc))
}

// defaultWorldSize must match the operator's defaultStorageSize.
const defaultWorldSize = v1alpha1.DefaultWorldStorageSize
