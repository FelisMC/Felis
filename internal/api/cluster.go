package api

import (
	"context"

	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

// ServerInfo is the lifecycle view read from the MinecraftServer CRD + status
// (spec §4). The CRD is the source-of-truth; the API mutates its spec only via
// the app-tier desiredState lever (wake/stop, spec §9.1) and the admin-tier spec
// patch (spec §7 PATCH /servers/{name}) — never the business-layer fields, which
// live in Postgres (spec §22).
type ServerInfo struct {
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
	StorageSize     string `json:"storageSize,omitempty"`
	CPU             string `json:"cpu,omitempty"`
}

// CreateServerInput is the validated, structured create-server form (spec §15).
// felis-api has already enforced naming, reservation, image whitelist, quota
// policy, and the §22 memory ceiling before this reaches the cluster: there is
// no free-form YAML path — every field is a typed, validated value. A created
// server starts DesiredState=Stopped and unowned (claimed later, spec §9.3).
type CreateServerInput struct {
	Name            string
	Subdomain       string
	DisplayName     string
	Image           string
	JavaMemory      string
	StorageSize     string
	AutostartPolicy v1alpha1.AutostartPolicy
	// Resources is the fully-resolved pod resource block. Its memory limit is the
	// §22 ceiling — felis-api guarantees it is non-zero (the operator does not
	// derive a cgroup limit from JavaMemory).
	Resources corev1.ResourceRequirements
}

// ServerSpecPatch is the resolved, validated set of admin-tier mutations applied
// to one MinecraftServer spec (spec §7 PATCH /servers/{name}). felis-api has
// already validated naming, admitted any new image against the whitelist, parsed
// the policy, and derived the §22 memory ceiling before this reaches the cluster:
// there is no free-form YAML path. Every field is a pointer — nil means "leave
// unchanged", so the patch touches only the fields the admin explicitly set. It
// deliberately omits the dual-write routing identity (subdomain, the object name)
// and the world PVC size, which felis-api rejects before constructing this so the
// CRD and Postgres never desync (spec §22).
type ServerSpecPatch struct {
	DisplayName     *string
	AutostartPolicy *v1alpha1.AutostartPolicy
	Image           *string
	// JavaMemory is the re-derived JVM heap string; Resources carries the matching
	// pod block whose memory limit is the non-zero §22 ceiling. They move together
	// (felis-api resolves both from the same form) or both stay nil.
	JavaMemory *string
	Resources  *corev1.ResourceRequirements
}

// Cluster is the lifecycle-layer access the API depends on: reads of the
// MinecraftServer CRD, the app-tier desiredState lever (spec §9.1), the admin-tier
// create (spec §15), and the admin-tier spec patch (spec §7). It is an interface
// so handlers are tested against a fake; the controller-runtime implementation
// (k8sCluster) is integration-tested only — it requires a live cluster.
type Cluster interface {
	// GetServer reads one MinecraftServer's lifecycle view, or ErrNotFound.
	GetServer(ctx context.Context, name string) (*ServerInfo, error)
	// GetBySubdomain finds the MinecraftServer whose spec.subdomain matches, or
	// ErrNotFound.
	GetBySubdomain(ctx context.Context, subdomain string) (*ServerInfo, error)
	// ListServers returns the lifecycle view of every MinecraftServer, for the
	// velocity registration pull (spec §7 GET /servers).
	ListServers(ctx context.Context) ([]ServerInfo, error)
	// SetDesiredState flips spec.desiredState — the only write the API performs
	// against the CRD (spec §9.1). It is idempotent.
	SetDesiredState(ctx context.Context, name string, state v1alpha1.DesiredState) error
	// CreateServer creates a MinecraftServer CRD from the validated form (spec
	// §15). It returns ErrConflict if a server of that name already exists.
	CreateServer(ctx context.Context, in CreateServerInput) error
	// PatchServerSpec applies an admin-tier spec mutation (spec §7 PATCH
	// /servers/{name}): only the non-nil fields of the patch are written, via a
	// merge patch so a concurrent operator status write is never clobbered. It
	// returns ErrNotFound if no server of that name exists.
	PatchServerSpec(ctx context.Context, name string, patch ServerSpecPatch) error
}
