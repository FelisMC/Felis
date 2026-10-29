package v1alpha1

import (
	"time"

	"felis.lolicon.best/internal/naming"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Label and annotation keys owned by the operator; all are scoped under the API group.
const (
	// LabelServer marks every operator-managed object with its owning
	// MinecraftServer name (used as the StatefulSet/Service/PVC selector).
	LabelServer = GroupName + "/server"
	// LabelManagedBy marks objects reconciled by felis-operator.
	LabelManagedBy = GroupName + "/managed-by"
	// LabelComponent distinguishes the workload role (server, rcon, ...).
	LabelComponent = GroupName + "/component"
	// LabelSystemRole identifies setup-owned system servers. Its value is the
	// reserved role name (for example, "login" or "lobby").
	LabelSystemRole = GroupName + "/system-role"
	// Legacy forwarding trusts the game port; its label affects routing without restarting the backend.
	LabelForwarding = GroupName + "/forwarding"
	// AnnotationStartRetry resets the Failed-start budget and pins the Pod until consumed.
	AnnotationStartRetry = GroupName + "/start-retry"
	// AnnotationRestart requests one pod recreation while keeping desiredState Running.
	AnnotationRestart = GroupName + "/restart"
	// AnnotationKill requests immediate pod deletion without saving or termination grace.
	AnnotationKill = GroupName + "/kill"
	// AnnotationLiveBackup holds the Job and frozen Pod identity; defer idle stop and scale-down while saving is disabled.
	AnnotationLiveBackup = GroupName + "/live-backup"
)

const ForwardingLegacy = "legacy"

// DesiredState is the operator-facing intent: Running, Stopped or Hibernated.
type DesiredState string

const (
	// DesiredRunning asks the operator to bring the server up.
	DesiredRunning DesiredState = "Running"
	// DesiredStopped asks the operator to scale the server down to zero.
	DesiredStopped DesiredState = "Stopped"
	// DesiredHibernated keeps the process frozen with its memory in swap; only the operator sets it, when an idle server hibernates.
	DesiredHibernated DesiredState = "Hibernated"
)

type Phase string

const (
	PhaseUnknown  Phase = "Unknown"
	PhaseStopped  Phase = "Stopped"
	PhaseStarting Phase = "Starting"
	PhaseRunning  Phase = "Running"
	PhaseStopping Phase = "Stopping"
	PhaseFailed   Phase = "Failed"
	// PhaseHibernating covers saving, freezing and swapping out an idle server.
	PhaseHibernating Phase = "Hibernating"
	// PhaseHibernated is a frozen process whose memory sits in swap; waking resumes it at the same tick.
	PhaseHibernated Phase = "Hibernated"
)

// AutostartPolicy controls who may wake a stopped server; it only applies when the proxy runs with online-mode=true.
type AutostartPolicy string

const (
	// AutostartPublic lets any authenticated player wake the server.
	AutostartPublic AutostartPolicy = "public"
	// AutostartAllowlist restricts waking to entries in server_allowlist.
	AutostartAllowlist AutostartPolicy = "allowlist"
	// AutostartOwnerOnly restricts waking to the claimed owner.
	AutostartOwnerOnly AutostartPolicy = "ownerOnly"
)

// EndpointMode is how the proxy reaches a Running server.
type EndpointMode string

const (
	// EndpointDirect means the proxy dials the Service ClusterIP directly.
	EndpointDirect EndpointMode = "direct"
	// EndpointFallback means traffic is routed to the configured fallback.
	EndpointFallback EndpointMode = "fallback"
)

// Condition type strings surfaced on status.conditions.
const (
	ConditionReady       = "Ready"
	ConditionRconReached = "RconReached"
	ConditionProvisioned = "Provisioned"
	// ConditionPlayersCounted is False while the RCON list reply cannot be read; idle auto-stop waits for a real count.
	ConditionPlayersCounted = "PlayersCounted"
)

// Reasons for a start that timed out: no TCP readiness, or RCON never answered. Both are retried by recreating the pod, up to MaxAutoRestarts with backoff.
const (
	ReasonStartupTimeout   = "StartupTimeout"
	ReasonReadinessTimeout = "ReadinessTimeout"
)

// MaxAutoRestarts bounds how often the operator retries a timed-out start.
const MaxAutoRestarts = 3

// StartGaveUp reports a Failed server no automatic retry will bring up: retries spent, or a failure the operator never retries. Only a person moves it on.
func StartGaveUp(s *MinecraftServerStatus) bool {
	if s.Phase != PhaseFailed {
		return false
	}
	c := meta.FindStatusCondition(s.Conditions, ConditionReady)
	timedOut := c != nil && (c.Reason == ReasonStartupTimeout || c.Reason == ReasonReadinessTimeout)
	return !timedOut || s.AutoRestarts >= MaxAutoRestarts || s.StartRequestedAt == nil
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// MinecraftServer is the lifecycle source of truth for one managed server; the operator reconciles its StatefulSet, Service and PVC from it.
type MinecraftServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MinecraftServerSpec   `json:"spec,omitempty"`
	Status MinecraftServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

type MinecraftServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MinecraftServer `json:"items"`
}

// MinecraftServerSpec is the desired state.
type MinecraftServerSpec struct {
	// Subdomain is the per-server label under the deployment zone. It is the
	// only routing identity; the operator never hardcodes the parent domain.
	Subdomain string `json:"subdomain"`
	// DisplayName is the human-facing name shown in the panel and MOTD.
	DisplayName string `json:"displayName,omitempty"`
	// Description is free-form operator/owner notes.
	Description  string `json:"description,omitempty"`
	ReaperExempt bool   `json:"reaperExempt,omitempty"`

	// NodeName is an administrator-approved execution node; empty preserves legacy placement.
	NodeName string `json:"nodeName,omitempty"`

	// DesiredState toggles the server up or down (default Stopped).
	// +kubebuilder:validation:Enum=Running;Stopped;Hibernated
	DesiredState DesiredState `json:"desiredState,omitempty"`
	// AutostartPolicy controls who may wake the server.
	// +kubebuilder:validation:Enum=public;allowlist;ownerOnly
	AutostartPolicy AutostartPolicy `json:"autostartPolicy,omitempty"`
	// FallbackServer is the Velocity server name traffic routes to while this
	// server is stopped or starting.
	FallbackServer string `json:"fallbackServer,omitempty"`

	// Image is the fully-qualified container image (loader-agnostic).
	Image string `json:"image"`
	// Software installs onto the world volume before each start; absent Software means a complete server image.
	Software *SoftwareSpec `json:"software,omitempty"`
	// Jar is the server jar path/name inside the image, if the entrypoint
	// needs it explicitly.
	Jar string `json:"jar,omitempty"`
	// JavaMemory is the maximum heap sizing passed as -Xmx (e.g. "4G").
	JavaMemory string `json:"javaMemory,omitempty"`
	// JavaFlags are additional JVM flags (e.g. Aikar's flags).
	JavaFlags []string `json:"javaFlags,omitempty"`
	// Args are extra arguments appended after the jar.
	Args []string `json:"args,omitempty"`
	// Env are extra environment variables injected into the server container.
	Env []EnvVar `json:"env,omitempty"`
	// +kubebuilder:validation:MaxItems=5
	// +listType=map
	// +listMapKey=protocol
	// +listMapKey=port
	ExtraPorts []ExtraPort `json:"extraPorts,omitempty"`

	// OnlineMode mirrors server.properties online-mode; wake and claim rules hold only when the proxy enforces online-mode=true.
	OnlineMode bool `json:"onlineMode,omitempty"`
	// Motd holds the per-phase MOTD strings surfaced to status pings.
	Motd MotdSpec `json:"motd,omitempty"`

	// Rcon configures the RCON endpoint used for readiness probes and graceful shutdown.
	Rcon RconSpec `json:"rcon,omitempty"`
	// Storage configures the world PVC.
	Storage StorageSpec `json:"storage,omitempty"`
	// Resources are the container resource requests/limits.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	Lifecycle LifecycleSpec               `json:"lifecycle,omitempty"`
	// Startup bounds how long Starting may last before Failed.
	Startup StartupSpec `json:"startup,omitempty"`
	Idle    IdleSpec    `json:"idle,omitempty"`
	// Recovery is ignored by system servers, which always resume and restart.
	Recovery RecoverySpec `json:"recovery,omitempty"`
	Notify   NotifySpec   `json:"notify,omitempty"`
}

// SoftwareSpec is the server software of a runtime-image server (see
// internal/software, whose Spec mirrors it field for field).
// +kubebuilder:validation:XValidation:rule="self.type == 'custom' ? has(self.jar) && !has(self.minecraftVersion) && !has(self.build) : has(self.minecraftVersion) && !has(self.jar)",message="a custom server names a jar and no version; every other type names a Minecraft version and no jar"
type SoftwareSpec struct {
	// Type is the kind of server software.
	// +kubebuilder:validation:Enum=paper;purpur;fabric;quilt;forge;neoforge;vanilla;custom
	Type string `json:"type"`
	// MinecraftVersion is the game version ("1.21.4", "26.3").
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[0-9A-Za-z][0-9A-Za-z.+_-]*$`
	MinecraftVersion string `json:"minecraftVersion,omitempty"`
	// Build is the Paper/Purpur build or the Fabric/Quilt/Forge/NeoForge
	// loader version.
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[0-9A-Za-z][0-9A-Za-z.+_-]*$`
	Build string `json:"build,omitempty"`
	// Java is the major version of the runtime image the server starts on.
	// +kubebuilder:validation:Enum=8;17;21;25
	Java int32 `json:"java"`
	// Jar is a custom server's jar, relative to the server directory.
	// +kubebuilder:validation:MaxLength=255
	Jar string `json:"jar,omitempty"`
	// Forwarding is how the proxy passes player identity to the server: velocity (modern forwarding) or none (offline-mode UUIDs).
	// +kubebuilder:validation:Enum=velocity;none
	Forwarding string `json:"forwarding,omitempty"`
}

// EnvVar is a name/value pair for the server container; unlike corev1.EnvVar it has no valueFrom, so the CRD cannot pull cluster secrets.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

// MotdSpec holds per-phase MOTD strings.
type MotdSpec struct {
	Running  string `json:"running,omitempty"`
	Stopped  string `json:"stopped,omitempty"`
	Starting string `json:"starting,omitempty"`
	Failed   string `json:"failed,omitempty"`
}

// RconSpec configures RCON. Keep the default port: the allow-rcon NetworkPolicy admits only 25575, so another port leaves the server stuck in Starting.
// +kubebuilder:validation:XValidation:rule="!has(self.port) || self.port == 0 || self.port == 25575",message="the allow-rcon NetworkPolicy admits only port 25575; leave port unset"
type RconSpec struct {
	// Enabled must be true for readiness probing and graceful shutdown.
	Enabled bool `json:"enabled,omitempty"`
	// Port is the RCON TCP port (default 25575).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
	// SecretRef points at the Secret holding the RCON password.
	SecretRef SecretKeyRef `json:"secretRef,omitempty"`
}

// SecretKeyRef references a single key within a Secret in the same namespace.
type SecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// StorageSpec configures the world PVC.
type StorageSpec struct {
	// ClaimName is managed internally by stopped-world migration, never by a public spec patch.
	ClaimName string `json:"claimName,omitempty"`
	Size      string `json:"size,omitempty"`
	// StorageClassName selects the StorageClass; empty uses the default.
	StorageClassName string `json:"storageClassName,omitempty"`
}

// LifecycleSpec runs a save and StopCommands over RCON before sending SIGTERM.
type LifecycleSpec struct {
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MaxLength=1024
	StopCommands []string `json:"stopCommands,omitempty"`
	// TerminationGracePeriodSeconds is the pod grace period (default 300).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=3600
	TerminationGracePeriodSeconds int64 `json:"terminationGracePeriodSeconds,omitempty"`
}

// StartupSpec bounds the Starting phase.
type StartupSpec struct {
	// TimeoutSeconds is the overall budget before the server is marked Failed.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=86400
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
	// ReadinessTimeoutSeconds is the budget for the first successful RCON probe.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=86400
	ReadinessTimeoutSeconds int32 `json:"readinessTimeoutSeconds,omitempty"`
	// HTTP readiness replaces TCP for configured ports and marks RCON-less servers ready only after a passing probe.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=65535
	HealthHTTPPort int32 `json:"healthHTTPPort,omitempty"`
	// HealthHTTPPath is the path for the HTTP readiness probe (default "/healthz"
	// when HealthHTTPPort is set).
	HealthHTTPPath string `json:"healthHTTPPath,omitempty"`
}

// DefaultEmptySecondsBeforeStop is the empty time before idle auto-stop, used when no value is chosen.
const DefaultEmptySecondsBeforeStop int32 = 600

// DefaultIdle is the idle policy for new user servers; system servers (login gate, lobby) never take it.
func DefaultIdle() IdleSpec {
	return IdleSpec{AutoStopEnabled: true, EmptySecondsBeforeStop: DefaultEmptySecondsBeforeStop}
}

// IdleAction is what a server does once it has been empty for its idle time.
type IdleAction string

const (
	// IdleHibernate freezes the process and swaps its memory to disk, so waking resumes the same tick.
	IdleHibernate IdleAction = "hibernate"
	// IdleStop shuts the server down.
	IdleStop IdleAction = "stop"
)

// IdleSpec configures empty-server auto-stop. Both fields zero means unset: `felis converge` fills in only that case, so auto-stop off keeps EmptySecondsBeforeStop set.
type IdleSpec struct {
	// AutoStopEnabled turns on idle auto-stop.
	AutoStopEnabled bool `json:"autoStopEnabled,omitempty"`
	// EmptySecondsBeforeStop is how long the server may sit empty before scale-down; the API caps panel values below the schema's week.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=604800
	EmptySecondsBeforeStop int32 `json:"emptySecondsBeforeStop,omitempty"`
	// Action is what the idle server does; empty means hibernate.
	// +kubebuilder:validation:Enum=hibernate;stop
	Action IdleAction `json:"action,omitempty"`
}

// Hibernates reports whether an idle server freezes rather than stops.
func (i IdleSpec) Hibernates() bool { return i.Action != IdleStop }

// HibernationStage is where a freeze or a wake stands.
type HibernationStage string

const (
	HibernationFreezing HibernationStage = "Freezing"
	HibernationFrozen   HibernationStage = "Frozen"
	// HibernationGrowing waits for the node to grant the pod its memory back before thawing.
	HibernationGrowing HibernationStage = "Growing"
	HibernationThawing HibernationStage = "Thawing"
	// HibernationKilling ends the frozen process from its node before its pod goes, for a stop or a failed wake.
	HibernationKilling HibernationStage = "Killing"
)

// HibernationStatus tracks a frozen server process.
type HibernationStatus struct {
	Stage HibernationStage `json:"stage"`
	// Since is when the freeze began.
	Since metav1.Time `json:"since"`
	// Job names the node Job carrying out the current freeze or thaw.
	Job string `json:"job,omitempty"`
	// Node is where the frozen process lives.
	Node string `json:"node,omitempty"`
	// Container is the ID of the frozen container, so a replaced pod is noticed.
	Container string `json:"container,omitempty"`
	// MemoryRequest, MemoryLimit and CPURequest are what the pod ran with before the freeze shrank it; waking restores them.
	MemoryRequest string `json:"memoryRequest,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
	CPURequest    string `json:"cpuRequest,omitempty"`
	// SwappedBytes is the memory moved to swap.
	SwappedBytes int64 `json:"swappedBytes,omitempty"`
	// Message explains why a wake is waiting.
	Message string `json:"message,omitempty"`
}

const DefaultMaxCrashRestarts int32 = 3

const MaxCrashRestartsLimit int32 = 20

// CrashWindow is how long without a crash starts the count over.
const CrashWindow = time.Hour

type RecoverySpec struct {
	// ResumeAfterHostRestart defaults to true when nil.
	ResumeAfterHostRestart *bool `json:"resumeAfterHostRestart,omitempty"`
	// RestartOnCrash also covers hangs and defaults to true when nil.
	RestartOnCrash *bool `json:"restartOnCrash,omitempty"`
	// MaxCrashRestarts is the consecutive crash limit, DefaultMaxCrashRestarts if zero.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=20
	MaxCrashRestarts int32 `json:"maxCrashRestarts,omitempty"`
}

func (r RecoverySpec) ResumesAfterHostRestart() bool {
	return r.ResumeAfterHostRestart == nil || *r.ResumeAfterHostRestart
}

func (r RecoverySpec) RestartsOnCrash() bool {
	return r.RestartOnCrash == nil || *r.RestartOnCrash
}

func (r RecoverySpec) CrashRestartLimit() int32 {
	switch {
	case !r.RestartsOnCrash():
		return 0
	case r.MaxCrashRestarts > 0:
		return r.MaxCrashRestarts
	}
	return DefaultMaxCrashRestarts
}

type NotifySpec struct {
	Started bool `json:"started,omitempty"`
	Stopped bool `json:"stopped,omitempty"`
	// Problems, on when nil, covers crashes, failed starts, host restarts and disk alerts.
	Problems *bool `json:"problems,omitempty"`
}

func (n NotifySpec) ProblemsOn() bool {
	return n.Problems == nil || *n.Problems
}

const (
	StopIdle        = "Idle"
	StopCrashed     = "Crashed"
	StopCrashLimit  = "CrashLimit"
	StopHostRestart = "HostRestarted"
)

type AutoStop struct {
	Reason string      `json:"reason"`
	At     metav1.Time `json:"at"`
	// Running at a generation past Generation means a person started the server.
	Generation int64 `json:"generation"`
}

// A CrashStopped server starts again only when a person starts it.
func CrashStopped(s *MinecraftServerStatus) bool {
	return s.AutoStop != nil && (s.AutoStop.Reason == StopCrashed || s.AutoStop.Reason == StopCrashLimit)
}

type RunStatus struct {
	PodUID string `json:"podUID,omitempty"`
	// Restarts is the container restart count of PodUID.
	Restarts int32  `json:"restarts,omitempty"`
	Node     string `json:"node,omitempty"`
	// BootID is the boot ID of Node, which changes when the host restarts.
	BootID string `json:"bootID,omitempty"`
}

type CrashRecord struct {
	At metav1.Time `json:"at"`
	// Reason is the container termination reason, such as OOMKilled or Error.
	Reason   string `json:"reason,omitempty"`
	ExitCode int32  `json:"exitCode"`
}

const (
	EventStarted       = "Started"
	EventStopped       = "Stopped"
	EventCrashed       = "Crashed"
	EventHostRestarted = "HostRestarted"
	EventStartFailed   = "StartFailed"
	EventHibernated    = "Hibernated"
)

// Lifecycle event reasons of a hibernation that did not end in a resumed process.
const (
	ReasonWoke            = "Woke"
	ReasonWakeFailed      = "WakeFailed"
	ReasonHibernationLost = "HibernationLost"
)

const MaxServerEvents = 20

type ServerEvent struct {
	Seq  int64       `json:"seq"`
	Type string      `json:"type"`
	At   metav1.Time `json:"at"`
	// Reason is the stop, termination or failure reason for Stopped, Crashed or StartFailed.
	Reason   string `json:"reason,omitempty"`
	ExitCode int32  `json:"exitCode,omitempty"`
	// Crashes is this crash's streak count and Limit the restart limit at the time.
	Crashes int32 `json:"crashes,omitempty"`
	Limit   int32 `json:"limit,omitempty"`
	// Stopping marks an event that made the operator stop the server.
	Stopping bool `json:"stopping,omitempty"`
}

// MinecraftServerStatus is the observed state.
type MinecraftServerStatus struct {
	NodeName string `json:"nodeName,omitempty"`
	// Phase is the coarse lifecycle phase.
	Phase Phase `json:"phase,omitempty"`
	// Ready is true only after a successful RCON probe; a status ping is never sufficient.
	Ready    bool           `json:"ready,omitempty"`
	Endpoint EndpointStatus `json:"endpoint,omitempty"`
	// Players is the last observed player count.
	Players PlayersStatus `json:"players,omitempty"`
	// LiveMotd is the MOTD currently advertised for the active phase.
	LiveMotd string `json:"liveMotd,omitempty"`
	// ReadySignalAt is when the first RCON probe of the current run succeeded.
	// Every Starting or Stopping pass clears it, so each start is measured once.
	ReadySignalAt *metav1.Time `json:"readySignalAt,omitempty"`
	// Persist the start timestamp across reconcile passes for duration metrics; clear it on stop.
	StartRequestedAt *metav1.Time `json:"startRequestedAt,omitempty"`
	// Reset empty time on joins, restarts, or stops; ignore the first three minutes after readiness.
	EmptySince *metav1.Time `json:"emptySince,omitempty"`
	// StopNoticeAt is when players were told the server is stopping; the stop waits StopNoticeWindow after it. Cleared on scale-down or when desiredState returns to Running.
	StopNoticeAt *metav1.Time `json:"stopNoticeAt,omitempty"`
	// AutoRestarts counts pod recreations after a timed-out start, at most MaxAutoRestarts; reaching Ready or stopping resets it.
	AutoRestarts int32 `json:"autoRestarts,omitempty"`
	// LastAutoRestartAt is when the operator last recreated the pod.
	LastAutoRestartAt  *metav1.Time `json:"lastAutoRestartAt,omitempty"`
	ObservedGeneration int64        `json:"observedGeneration,omitempty"`
	Run                RunStatus    `json:"run,omitempty"`
	// Crashes counts consecutive crashes, reset after a quiet CrashWindow or a manual start.
	Crashes   int32        `json:"crashes,omitempty"`
	LastCrash *CrashRecord `json:"lastCrash,omitempty"`
	AutoStop  *AutoStop    `json:"autoStop,omitempty"`
	// Hibernation tracks a frozen process; nil when the server is not hibernating.
	Hibernation *HibernationStatus `json:"hibernation,omitempty"`
	// Events are oldest first, at most MaxServerEvents.
	Events []ServerEvent `json:"events,omitempty"`
	// +kubebuilder:validation:MaxItems=5
	ExtraPorts []AllocatedExtraPort `json:"extraPorts,omitempty"`
	// Conditions are the standard metav1 conditions (Ready, RconReached, ...).
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// EndpointStatus is the resolved routing target.
type EndpointStatus struct {
	Mode    EndpointMode `json:"mode,omitempty"`
	Address string       `json:"address,omitempty"`
}

// PlayersStatus is the last observed player count.
type PlayersStatus struct {
	Online int32 `json:"online"`
	Max    int32 `json:"max"`
}

// WorldPVC resolves the active world, including servers created before migration support.
func (s *MinecraftServer) WorldPVC() string {
	if s.Spec.Storage.ClaimName != "" {
		return s.Spec.Storage.ClaimName
	}
	return naming.WorldPVCName(s.Name)
}
