package api

import (
	"context"
	"time"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// ServerInfo projects CRD lifecycle state; business ownership and quotas remain in Postgres.
type ServerInfo struct {
	UID             string `json:"-"`
	NodeName        string `json:"nodeName,omitempty"`
	Name            string `json:"name"`
	Subdomain       string `json:"subdomain"`
	Phase           string `json:"phase"`
	Ready           bool   `json:"ready"`
	AutostartPolicy string `json:"autostartPolicy,omitempty"`
	DesiredState    string `json:"desiredState,omitempty"`
	EndpointMode    string `json:"endpointMode,omitempty"`
	EndpointAddress string `json:"endpointAddress,omitempty"`
	PlayersOnline   int32  `json:"playersOnline"`
	PlayersMax      int32  `json:"playersMax"`
	DisplayName     string `json:"displayName,omitempty"`
	Image           string `json:"image,omitempty"`
	JavaMemory      string `json:"javaMemory,omitempty"`
	// Memory is the pod limit; JavaMemory is the heap derived from it.
	Memory            string            `json:"memory,omitempty"`
	StorageSize       string            `json:"storageSize,omitempty"`
	CPU               string            `json:"cpu,omitempty"`
	CPUMilli          *int64            `json:"cpuMilli,omitempty"`
	MemoryBytes       *int64            `json:"memoryBytes,omitempty"`
	Disk              *ServerDiskUsage  `json:"disk,omitempty"`
	DiskFull          bool              `json:"diskFull,omitempty"`
	ExtraPorts        []ServerExtraPort `json:"extraPorts,omitempty"`
	ExtraPortsPending bool              `json:"extraPortsPending,omitempty"`
	// IdleStopSeconds=0 disables idle shutdown.
	IdleStopSeconds int32 `json:"idleStopSeconds"`
	// IdleAction is hibernate or stop; the public projection leaves it out.
	IdleAction string `json:"idleAction,omitempty"`
	// Hibernation is set while the server's process is frozen, being frozen or waking.
	Hibernation *ServerHibernation `json:"hibernation,omitempty"`
	// PlayerCountUnknown suspends idle shutdown until RCON can read the count.
	PlayerCountUnknown bool `json:"playerCountUnknown,omitempty"`
	// LegacyForwarding enables player-identity forwarding in the handshake address.
	LegacyForwarding bool `json:"legacyForwarding,omitempty"`
	// StartGaveUp means no automatic retry remains; Failed without it may still be in restart backoff.
	AutoRestarts int32 `json:"autoRestarts,omitempty"`
	StartGaveUp  bool  `json:"startGaveUp,omitempty"`
	// ReaperExempt system servers cannot be released or deleted.
	ReaperExempt bool `json:"reaperExempt,omitempty"`
	// Retiring is joined from Postgres; the cluster does not set it.
	Retiring *RetireState `json:"retiring,omitempty"`
	// Tags are joined from Postgres for the owner and staff; the cluster does not set them.
	Tags []string `json:"tags,omitempty"`
	// Resources stays off the wire and supplies unchanged values when patching the spec.
	Resources corev1.ResourceRequirements `json:"-"`
	// Software is nil for a server image.
	Software *ServerSoftware `json:"software,omitempty"`
	Startup  *StartupStatus  `json:"startup,omitempty"`
	// System servers always resume and restart, whatever these say.
	ResumeAfterHostRestart bool         `json:"resumeAfterHostRestart"`
	RestartOnCrash         bool         `json:"restartOnCrash"`
	MaxCrashRestarts       int32        `json:"maxCrashRestarts"`
	Notify                 ServerNotify `json:"notify"`
	// RecentCrashes counts crashes in a row, the latest within the last hour.
	RecentCrashes int32      `json:"recentCrashes,omitempty"`
	LastCrash     *CrashInfo `json:"lastCrash,omitempty"`
	// AutoStop says why the platform stopped the server and clears on start.
	AutoStop *AutoStopInfo `json:"autoStop,omitempty"`
	// CrashStopped servers start only by their owner or an admin, never by a join.
	CrashStopped bool `json:"crashStopped,omitempty"`
	// Events, oldest first, are filled in for a single server only.
	Events []ServerEvent `json:"events,omitempty"`
}

type ServerNotify struct {
	Started  bool `json:"started"`
	Stopped  bool `json:"stopped"`
	Problems bool `json:"problems"`
}

type CrashInfo struct {
	At       time.Time `json:"at"`
	Reason   string    `json:"reason,omitempty"`
	ExitCode int32     `json:"exitCode"`
}

type AutoStopInfo struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

type ServerEvent struct {
	Seq      int64     `json:"seq"`
	Type     string    `json:"type"`
	At       time.Time `json:"at"`
	Reason   string    `json:"reason,omitempty"`
	ExitCode int32     `json:"exitCode,omitempty"`
	Crashes  int32     `json:"crashes,omitempty"`
	Limit    int32     `json:"limit,omitempty"`
	Stopping bool      `json:"stopping,omitempty"`
}

// ServerEventFeed.Mailed is the last event seq already mailed or skipped.
type ServerEventFeed struct {
	Name        string
	DisplayName string
	System      bool
	Notify      ServerNotify
	Mailed      int64
	Events      []ServerEvent
}

// CreateServerInput is already validated; new servers start stopped and unowned.
type CreateServerInput struct {
	NodeName        string
	Name            string
	Subdomain       string
	DisplayName     string
	Image           string
	JavaMemory      string
	StorageSize     string
	AutostartPolicy v1alpha1.AutostartPolicy
	// Resources must include a nonzero pod memory limit; the operator does not derive one from JavaMemory.
	Resources corev1.ResourceRequirements
	// Software makes Image the Java runtime image, with server software stored on the world volume.
	Software *v1alpha1.SoftwareSpec
}

// ServerSpecPatch uses nil for unchanged fields and excludes routing identity to avoid cross-store drift.
type ServerSpecPatch struct {
	DisplayName     *string
	AutostartPolicy *v1alpha1.AutostartPolicy
	Image           *string
	// Resources is the merged pod block; JavaMemory changes only when the memory limit changes.
	JavaMemory      *string
	Resources       *corev1.ResourceRequirements
	IdleStopSeconds *int32
	JavaFlags       *[]string
	Args            *[]string
	Env             *[]v1alpha1.EnvVar
	StopCommands    *[]string
	ExtraPorts      *[]v1alpha1.ExtraPort
	// ClearSoftware removes the runtime-software selection when moving to a server image.
	Software      *v1alpha1.SoftwareSpec
	ClearSoftware bool
	// StorageSize moves only the world quota; the claim keeps the size it was created with.
	StorageSize *string

	ResumeAfterHostRestart *bool
	RestartOnCrash         *bool
	MaxCrashRestarts       *int32
	NotifyStarted          *bool
	NotifyStopped          *bool
	NotifyProblems         *bool
	IdleAction             *v1alpha1.IdleAction
}

func (s *ServerInfo) hibernated() bool {
	return s.DesiredState == string(v1alpha1.DesiredHibernated)
}

// ServerHibernation describes a frozen server process.
type ServerHibernation struct {
	Stage        string    `json:"stage"`
	Since        time.Time `json:"since"`
	SwappedBytes int64     `json:"swappedBytes,omitempty"`
	// Message explains why a wake is waiting.
	Message string `json:"message,omitempty"`
}

type Cluster interface {
	// Ping requires both Kubernetes reachability and a fully synced MinecraftServer cache.
	Ping(ctx context.Context) error

	GetServer(ctx context.Context, name string) (*ServerInfo, error)
	GetStartupSettings(ctx context.Context, name string) (*ServerStartupSettings, error)
	WorldVolumeExists(ctx context.Context, name string) (bool, error)
	// EnsureWorldVolume creates a missing PVC with the operator's size and class so its StatefulSet can adopt it.
	EnsureWorldVolume(ctx context.Context, name string) error
	GetBySubdomain(ctx context.Context, subdomain string) (*ServerInfo, error)
	ListServers(ctx context.Context) ([]ServerInfo, error)
	// SetDesiredState is idempotent and refuses Running with MaintenanceBusyError while the volume is held.
	SetDesiredState(ctx context.Context, name string, state v1alpha1.DesiredState) error
	// RetryStart also resets the automatic restart budget; maintenance still blocks startup.
	RetryStart(ctx context.Context, name string) error
	RestartServer(ctx context.Context, name string) error
	KillServer(ctx context.Context, name string) error
	CheckFileAccess(ctx context.Context, name string) error
	// AcquireMaintenance atomically checks the stopped state and locks against wakes; only system-config writes may run live.
	AcquireMaintenance(ctx context.Context, name, kind string) error
	// ReleaseMaintenance is idempotent and runs after Job creation succeeds or fails.
	ReleaseMaintenance(ctx context.Context, name string) error
	CreateServer(ctx context.Context, in CreateServerInput) error
	// PatchServerSpec merge-patches non-nil fields so concurrent operator status updates survive.
	PatchServerSpec(ctx context.Context, name string, patch ServerSpecPatch) error
	ClearExtraPorts(ctx context.Context, name, uid string) error
	ServerEventFeeds(ctx context.Context) ([]ServerEventFeed, error)
	MarkEventsMailed(ctx context.Context, name string, seq int64) error
}

type ServerSoftware struct {
	Type             string `json:"type"`
	MinecraftVersion string `json:"minecraftVersion,omitempty"`
	Build            string `json:"build,omitempty"`
	Java             int32  `json:"java"`
	Jar              string `json:"jar,omitempty"`
	// Forwarding=none gives direct players offline-mode UUIDs; velocity preserves proxy-authenticated identity.
	Forwarding string `json:"forwarding"`
}

func serverSoftware(s *v1alpha1.SoftwareSpec) *ServerSoftware {
	if s == nil {
		return nil
	}
	fwd := s.Forwarding
	if fwd == "" {
		fwd = "none"
	}
	return &ServerSoftware{Type: s.Type, MinecraftVersion: s.MinecraftVersion, Build: s.Build, Java: s.Java, Jar: s.Jar, Forwarding: fwd}
}
