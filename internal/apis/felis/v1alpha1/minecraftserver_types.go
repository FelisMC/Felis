package v1alpha1

import (
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
	// AnnotationStartRetry asks the operator to restart a Failed server with a fresh restart budget; its value is the RFC 3339 request time, removed once taken.
	AnnotationStartRetry = GroupName + "/start-retry"
	// AnnotationRestart requests one pod recreation while keeping desiredState Running.
	AnnotationRestart = GroupName + "/restart"
	// AnnotationLiveBackup holds the Job and frozen Pod identity; defer idle stop and scale-down while saving is disabled.
	AnnotationLiveBackup = GroupName + "/live-backup"
)

const ForwardingLegacy = "legacy"

// DesiredState is the operator-facing intent: Running or Stopped.
type DesiredState string

const (
	// DesiredRunning asks the operator to bring the server up.
	DesiredRunning DesiredState = "Running"
	// DesiredStopped asks the operator to scale the server down to zero.
	DesiredStopped DesiredState = "Stopped"
)

type Phase string

const (
	PhaseUnknown  Phase = "Unknown"
	PhaseStopped  Phase = "Stopped"
	PhaseStarting Phase = "Starting"
	PhaseRunning  Phase = "Running"
	PhaseStopping Phase = "Stopping"
	PhaseFailed   Phase = "Failed"
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
	// +kubebuilder:validation:Enum=Running;Stopped
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

// LifecycleSpec tunes graceful shutdown: the operator runs save-all flush over RCON first, then the grace period bounds the server's own shutdown save after SIGTERM.
type LifecycleSpec struct {
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

// IdleSpec configures empty-server auto-stop. Both fields zero means unset: `felis converge` fills in only that case, so auto-stop off keeps EmptySecondsBeforeStop set.
type IdleSpec struct {
	// AutoStopEnabled turns on idle auto-stop.
	AutoStopEnabled bool `json:"autoStopEnabled,omitempty"`
	// EmptySecondsBeforeStop is how long the server may sit empty before scale-down; the API caps panel values below the schema's week.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=604800
	EmptySecondsBeforeStop int32 `json:"emptySecondsBeforeStop,omitempty"`
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
