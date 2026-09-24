package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Label and annotation keys owned by the operator (spec §4, §5). All keys are
// scoped under the software-identity group so they never collide with the
// deployment domain.
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
)

// DesiredState is the operator-facing intent toggle (spec §4 spec.desiredState).
type DesiredState string

const (
	// DesiredRunning asks the operator to bring the server up.
	DesiredRunning DesiredState = "Running"
	// DesiredStopped asks the operator to scale the server down to zero.
	DesiredStopped DesiredState = "Stopped"
)

// Phase is the observed lifecycle phase (spec §4 status.phase).
type Phase string

const (
	PhaseUnknown  Phase = "Unknown"
	PhaseStopped  Phase = "Stopped"
	PhaseStarting Phase = "Starting"
	PhaseRunning  Phase = "Running"
	PhaseStopping Phase = "Stopping"
	PhaseFailed   Phase = "Failed"
)

// AutostartPolicy controls who may wake a stopped server (spec §4, §8). It only
// has meaning when the Velocity proxy runs with online-mode=true.
type AutostartPolicy string

const (
	// AutostartPublic lets any authenticated player wake the server.
	AutostartPublic AutostartPolicy = "public"
	// AutostartAllowlist restricts waking to entries in server_allowlist.
	AutostartAllowlist AutostartPolicy = "allowlist"
	// AutostartOwnerOnly restricts waking to the claimed owner.
	AutostartOwnerOnly AutostartPolicy = "ownerOnly"
)

// EndpointMode describes how the proxy reaches a Running server (spec §4
// status.endpoint.mode).
type EndpointMode string

const (
	// EndpointDirect means the proxy dials the Service ClusterIP directly.
	EndpointDirect EndpointMode = "direct"
	// EndpointFallback means traffic is routed to the configured fallback.
	EndpointFallback EndpointMode = "fallback"
)

// Condition type strings surfaced on status.conditions (spec §4).
const (
	ConditionReady       = "Ready"
	ConditionRconReached = "RconReached"
	ConditionProvisioned = "Provisioned"
	// ConditionPlayersCounted is False while the RCON `list` reply cannot be
	// read; idle auto-stop waits for a real count (spec §8).
	ConditionPlayersCounted = "PlayersCounted"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// MinecraftServer is the lifecycle source-of-truth for a single managed
// Minecraft server (spec §4). The operator reconciles the StatefulSet, Service
// and PVC from this object; readiness is gated exclusively on an RCON probe.
type MinecraftServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MinecraftServerSpec   `json:"spec,omitempty"`
	Status MinecraftServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MinecraftServerList is a list of MinecraftServer objects.
type MinecraftServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MinecraftServer `json:"items"`
}

// MinecraftServerSpec is the desired state (spec §4 spec.*).
type MinecraftServerSpec struct {
	// Subdomain is the per-server label under the deployment zone. It is the
	// only routing identity; the operator never hardcodes the parent domain.
	Subdomain string `json:"subdomain"`
	// DisplayName is the human-facing name shown in the panel and MOTD.
	DisplayName string `json:"displayName,omitempty"`
	// Description is free-form operator/owner notes.
	Description string `json:"description,omitempty"`
	// ReaperExempt opts this server out of the world reaper entirely (spec §18).
	ReaperExempt bool `json:"reaperExempt,omitempty"`

	// DesiredState toggles the server up or down (default Stopped).
	// +kubebuilder:validation:Enum=Running;Stopped
	DesiredState DesiredState `json:"desiredState,omitempty"`
	// AutostartPolicy controls who may wake the server (spec §8).
	// +kubebuilder:validation:Enum=public;allowlist;ownerOnly
	AutostartPolicy AutostartPolicy `json:"autostartPolicy,omitempty"`
	// FallbackServer is the Velocity server name traffic routes to while this
	// server is stopped or starting.
	FallbackServer string `json:"fallbackServer,omitempty"`

	// Image is the fully-qualified container image (loader-agnostic).
	Image string `json:"image"`
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

	// OnlineMode mirrors server.properties online-mode. Wake/claim semantics
	// only hold when the Velocity proxy enforces online-mode=true (spec §8).
	OnlineMode bool `json:"onlineMode,omitempty"`
	// Motd holds the per-phase MOTD strings surfaced to status pings.
	Motd MotdSpec `json:"motd,omitempty"`

	// Rcon configures the RCON endpoint the operator probes for readiness and
	// uses for graceful shutdown (spec §5, §7).
	Rcon RconSpec `json:"rcon,omitempty"`
	// Storage configures the world PVC.
	Storage StorageSpec `json:"storage,omitempty"`
	// Resources are the container resource requests/limits.
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
	// Lifecycle tunes graceful shutdown (spec §7).
	Lifecycle LifecycleSpec `json:"lifecycle,omitempty"`
	// Startup bounds how long Starting may last before Failed (spec §5).
	Startup StartupSpec `json:"startup,omitempty"`
	// Idle configures empty-server auto-stop (spec §8).
	Idle IdleSpec `json:"idle,omitempty"`
}

// EnvVar is a name/value pair injected into the server container. It is a
// deliberately narrow subset of corev1.EnvVar (no valueFrom) so the CRD cannot
// be used to exfiltrate arbitrary cluster secrets.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

// MotdSpec holds per-phase MOTD strings (spec §4 spec.motd).
type MotdSpec struct {
	Running  string `json:"running,omitempty"`
	Stopped  string `json:"stopped,omitempty"`
	Starting string `json:"starting,omitempty"`
	Failed   string `json:"failed,omitempty"`
}

// RconSpec configures RCON (spec §4 spec.rcon).
type RconSpec struct {
	// Enabled must be true for readiness probing and graceful shutdown.
	Enabled bool `json:"enabled,omitempty"`
	// Port is the RCON TCP port (default 25575).
	Port int32 `json:"port,omitempty"`
	// SecretRef points at the Secret holding the RCON password.
	SecretRef SecretKeyRef `json:"secretRef,omitempty"`
}

// SecretKeyRef references a single key within a Secret in the same namespace.
type SecretKeyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// StorageSpec configures the world PVC (spec §4 spec.storage).
type StorageSpec struct {
	// Size is the requested PVC capacity (e.g. "10Gi").
	Size string `json:"size,omitempty"`
	// StorageClassName selects the StorageClass; empty uses the default.
	StorageClassName string `json:"storageClassName,omitempty"`
}

// LifecycleSpec tunes graceful shutdown (spec §7). Before scaling a server with
// RCON to zero the operator runs "save-all flush" over RCON; the grace period is
// then the time the server has to finish its own shutdown save after SIGTERM.
type LifecycleSpec struct {
	// TerminationGracePeriodSeconds is the pod grace period (default 300).
	TerminationGracePeriodSeconds int64 `json:"terminationGracePeriodSeconds,omitempty"`
}

// StartupSpec bounds the Starting phase (spec §5).
type StartupSpec struct {
	// TimeoutSeconds is the overall budget before the server is marked Failed.
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
	// ReadinessTimeoutSeconds is the budget for the first successful RCON probe.
	ReadinessTimeoutSeconds int32 `json:"readinessTimeoutSeconds,omitempty"`
	// HealthHTTPPort, when > 0, switches the pod readiness probe from the default
	// plain-TCP check on the game port to an HTTP GET on this container port. It
	// exists for RCON-less loaders (notably LOOHP/Limbo) where "the socket is
	// bound" is a weaker signal than the server itself reporting it has finished
	// starting: the felis-limbo plugin serves such an endpoint and flips it to 200
	// only after the first server tick. The operator's readiness path is otherwise
	// unchanged — with rcon disabled, passing this probe (readyReplicas >= 1) is
	// what marks the server Ready.
	HealthHTTPPort int32 `json:"healthHTTPPort,omitempty"`
	// HealthHTTPPath is the path for the HTTP readiness probe (default "/healthz"
	// when HealthHTTPPort is set).
	HealthHTTPPath string `json:"healthHTTPPath,omitempty"`
}

// DefaultEmptySecondsBeforeStop is how long a user server may sit empty before
// idle auto-stop scales it down, when nobody chose another value (spec §8).
const DefaultEmptySecondsBeforeStop int32 = 600

// DefaultIdle is the idle policy every user server is created with: stop after
// DefaultEmptySecondsBeforeStop of an empty server. System servers (the login
// gate, the lobby) never take it; they must stay up.
func DefaultIdle() IdleSpec {
	return IdleSpec{AutoStopEnabled: true, EmptySecondsBeforeStop: DefaultEmptySecondsBeforeStop}
}

// IdleSpec configures empty-server auto-stop (spec §8).
//
// The two fields together tell a choice from its absence: turning auto-stop
// off keeps EmptySecondsBeforeStop set (AutoStopEnabled=false, seconds > 0),
// while a server that predates the default has both zero, and only that one
// is filled in by `felis converge`.
type IdleSpec struct {
	// AutoStopEnabled turns on idle auto-stop.
	AutoStopEnabled bool `json:"autoStopEnabled,omitempty"`
	// EmptySecondsBeforeStop is how long the server may sit empty before the
	// operator scales it down.
	EmptySecondsBeforeStop int32 `json:"emptySecondsBeforeStop,omitempty"`
}

// MinecraftServerStatus is the observed state (spec §4 status.*).
type MinecraftServerStatus struct {
	// Phase is the coarse lifecycle phase.
	Phase Phase `json:"phase,omitempty"`
	// Ready is true only after a successful RCON probe (loader-agnostic; a
	// status ping is never sufficient — spec §5).
	Ready bool `json:"ready,omitempty"`
	// Endpoint is where the proxy should route traffic.
	Endpoint EndpointStatus `json:"endpoint,omitempty"`
	// Players is the last observed player count.
	Players PlayersStatus `json:"players,omitempty"`
	// LiveMotd is the MOTD currently advertised for the active phase.
	LiveMotd string `json:"liveMotd,omitempty"`
	// ReadySignalAt is when the first RCON probe succeeded.
	ReadySignalAt *metav1.Time `json:"readySignalAt,omitempty"`
	// StartRequestedAt is when the current start attempt was first observed
	// (the first Starting reconcile after desiredState=Running). It anchors the
	// felis_start_duration_seconds histogram (spec §23): the operator observes
	// ReadySignalAt-StartRequestedAt the moment readiness is first reached, then
	// clears this on stop so the next start re-anchors. Persisted in status
	// because the two endpoints fall in different reconcile passes.
	StartRequestedAt *metav1.Time `json:"startRequestedAt,omitempty"`
	// EmptySince is when the operator first observed 0 online players during a
	// Running phase (spec §8 idle auto-stop). It is reset when a player joins
	// or the server stops, so the empty-duration counter starts fresh each time
	// the server becomes unoccupied.
	EmptySince *metav1.Time `json:"emptySince,omitempty"`
	// ObservedGeneration is the spec generation this status reflects.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions are the standard metav1 conditions (Ready, RconReached, ...).
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// EndpointStatus is the resolved routing target (spec §4 status.endpoint).
type EndpointStatus struct {
	// Mode is "direct" or "fallback".
	Mode EndpointMode `json:"mode,omitempty"`
	// Address is the host:port the proxy should dial.
	Address string `json:"address,omitempty"`
}

// PlayersStatus is the last observed player count (spec §4 status.players).
type PlayersStatus struct {
	Online int32 `json:"online"`
	Max    int32 `json:"max"`
}
