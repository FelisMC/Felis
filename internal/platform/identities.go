package platform

import (
	"fmt"
	"net"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Identity constants and the Params that parameterise the install bundle.
//
// The label and SA-name constants are PINNED here because three independent
// things must agree on them and there is no cluster to catch a disagreement at
// runtime: (1) the control-plane Deployments that carry the pod labels and run
// as these SAs, (2) the NetworkPolicy peers that select the RCON callers by those
// labels, and (3) the RoleBindings whose subjects name those SAs. Changing a
// value here is a deliberate, test-guarded act.

// Recommended-label keys (the app.kubernetes.io/* set) applied to control-plane
// objects. We select on Component + PartOf in the RCON NetworkPolicy, so these
// must stay stable: a NetworkPolicy peer match is a plain string compare, and a
// renamed value silently stops matching the pods it is meant to admit.
const (
	LabelName      = "app.kubernetes.io/name"
	LabelComponent = "app.kubernetes.io/component"
	LabelPartOf    = "app.kubernetes.io/part-of"

	appName            = "felis"
	controlPlanePartOf = "felis-control-plane"

	// Component values distinguish the three control-plane workloads. The RCON
	// policy admits only {api, operator}; reaper never opens an RCON connection,
	// so it is deliberately excluded.
	ComponentAPI      = "api"
	ComponentOperator = "operator"
	ComponentReaper   = "reaper"

	// ComponentRegistry labels the in-cluster image registry. It is deliberately
	// NOT part-of=felis-control-plane: the registry is a supporting workload, not
	// a control-plane identity, so the RCON NetworkPolicy peer (which requires
	// part-of=felis-control-plane) can never select it.
	ComponentRegistry = "registry"

	// ComponentPostgres labels the control-plane database. Like the registry it
	// is a supporting workload and NOT part-of=felis-control-plane, so no
	// control-plane peer selector (RCON, the internal API) can select it.
	ComponentPostgres = "postgres"
)

// Service-account names. The control-plane SAs (api/operator/reaper) are bound to
// the namespaced Roles in this package; the weak Job SAs (build/restore) have NO
// Role anywhere — their isolation is the absence of any binding (spec §16, §21).
const (
	SAAPI      = "felis-api"
	SAOperator = "felis-operator"
	SAReaper   = "felis-reaper"
	SABuild    = "felis-build"
	SARestore  = "felis-restore"
)

// Default namespaces. They match the defaults used elsewhere in the tree
// (config.defaultNamespace = "minecraft", build/restore package defaults) so an
// unconfigured deployment is internally consistent.
const (
	DefaultControlNamespace   = "felis"
	DefaultMinecraftNamespace = "minecraft"
	DefaultBuildNamespace     = "felis-build"
	DefaultPanelNodePort      = int32(30443)

	defaultRegistryPort int32 = 5000

	// defaultRegistryImage is the upstream CNCF Distribution registry. It is an
	// official, stable image and the only registry implementation the build/restore
	// subsystems are exercised against (registry.<ns>.svc:5000). Pinned by digest so
	// a re-tag upstream cannot change what holds every image on the box; the tag is
	// for humans. deploy/bootstrap.sh's REGISTRY_IMAGE caches and GC-pins this exact
	// ref and must name the same one (TestBootstrapPinsTheRegistryImage).
	defaultRegistryImage = "docker.io/library/registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"

	// defaultPostgresImage is the official PostgreSQL image the control-plane
	// database runs (postgres.go). Pinned by digest for the same reason as the
	// registry, and more: a re-tag that moved the major would start an empty
	// cluster in a fresh <major>/docker directory beside the real one.
	// deploy/bootstrap.sh's POSTGRES_IMAGE caches and GC-pins this exact ref
	// (TestBootstrapPinsThePostgresImage) and refuses to start it over data
	// another major wrote.
	defaultPostgresImage = "docker.io/library/postgres:18.6-trixie@sha256:5a5a84b19854a9ffaa54082c166ff4ec27473a361e496e5ea167f298f2da9722"
)

// Params parameterises the install bundle. Namespaces and the registry location
// have safe defaults; VelocityCIDRs has none — see the field comment.
type Params struct {
	NodeControlSocket string
	NodeControlNode   string
	Distributed       bool
	ControllerNode    string
	EgressProbe       string
	RegistryNodeCIDRs []string
	// ControlNamespace is where felis-api/operator run; their SAs live here and the
	// RoleBindings' subjects reference them here, even though the Roles they bind to
	// live in the minecraft (and build) namespaces. The reaper alone runs — CronJob
	// and SA — in the Minecraft namespace, because a Pod can only mount a PVC and
	// use a ServiceAccount from its own namespace, and its backup PVC is there.
	ControlNamespace string
	// MinecraftNamespace is where MinecraftServer workloads, their RCON Secrets,
	// and their world PVCs live. All three identities' minecraft-scoped Roles, and
	// every server NetworkPolicy, are installed here.
	MinecraftNamespace string
	// BuildNamespace is where image-build Jobs run under the weak felis-build SA,
	// with the egress-locked NetworkPolicy.
	BuildNamespace string
	// VelocityCIDRs are the Velocity proxy source addresses permitted to reach
	// server game ports (25565) as ipBlock peers. The bootstrap proxy runs on the
	// k3s node; other deployments may use a separate host. Kubernetes always permits
	// resident-node traffic independently of NetworkPolicy, so this list constrains
	// non-node sources. It has NO default, and the `felis manifests` generator
	// refuses to emit a bundle without an explicit proxy placement.
	VelocityCIDRs []string
	// RegistryNamespace / RegistryPort locate the in-cluster image registry the
	// build egress policy may reach (spec §16). RegistryNamespace defaults to the
	// control namespace (registry co-located with the control plane).
	RegistryNamespace string
	RegistryPort      int32
	// PanelNodePort exposes the built-in HTTPS panel/API origin from the node.
	// It defaults to 30443 so a fresh setup can finish with a concrete browser URL.
	PanelNodePort int32
	// PackageSourceCIDRs is the explicit package-mirror egress allowlist for build
	// Pods (spec §16). Empty means no internet egress at all — the locked-down
	// default the build subsystem already enforces.
	PackageSourceCIDRs []string
	// ServerEgressDenyCIDRs are extra destinations game server pods may never
	// reach, on top of the private, link-local and loopback ranges the server
	// egress policy always excludes. The installer passes the node's own global
	// addresses: a node with a public IP would otherwise be reachable from a
	// tenant's plugin on every host port (PostgreSQL, the kube API, kubelet).
	ServerEgressDenyCIDRs []string
	// ServerEgressAllowCIDRs are private destinations game servers MAY reach
	// despite that exclusion, e.g. a LAN database a server's plugin uses. Empty by
	// default: a tenant's code has no business on the operator's network.
	ServerEgressAllowCIDRs []string
	// FelisImage is the container image the felis-api and felis-operator
	// Deployments run (the multi-call `felis` binary). It has NO default and no
	// safe guess: `felis manifests` REQUIRES --felis-image and refuses to render
	// without it, the same fail-loud contract as --velocity-cidr. The api pod also
	// passes this value through as FELIS_IMAGE so the restore executor launches its
	// `felis restore` Job using the very same image.
	FelisImage string
	// RegistryImage is the in-cluster registry image. Defaults to registry 2.8.3, by digest.
	RegistryImage string
	// PostgresImage is the control-plane database image. Defaults to PostgreSQL
	// 18.6, by digest.
	PostgresImage string
	// BackupPVC is the name of the world-archive PersistentVolumeClaim. The bundle
	// RENDERS this PVC (backupPVC in workloads.go, Minecraft namespace — where every
	// pod that mounts it runs) and felis-api advertises the name to its backup/restore
	// executors via FELIS_BACKUP_PVC. An empty name renders neither: no PVC, no env,
	// and the backup/restore endpoints degrade to 503 (cmd/felis/api.go) rather than
	// enqueuing a Job that cannot mount its backup. The PVC name itself carries no
	// path meaning; the in-pod mount path is felis.toml's [archive] local_path (the
	// Jobs mount the PVC there, and tarLocal writes archive refs as absolute paths
	// under it). The reaper CronJob (when rendered) mounts this same PVC read-write
	// to write archives into it — see WorldsHostPath / ArchiveLocalPath.
	BackupPVC string
	// WorldsHostPath is the node directory under which each server's world PVC is
	// visible as <WorldsHostPath>/<pvc> — the on-disk root the reaper CronJob mounts
	// (read-only) at /worlds to archive idle worlds before reclaiming them (spec §18,
	// §19 tarLocal-on-local-path starter). It has NO default and is the master switch
	// for retention: empty ⇒ the reaper CronJob is NOT rendered (fail-safe — no
	// CronJob is far safer than one that deletes PVCs while reading worlds from the
	// wrong place). A hostPath ties the reaper to a single node, which is exactly the
	// §19 starter topology (the operator provisions per-server ReadWriteOnce world
	// PVCs, so a shared RWX worlds mount would contradict it); multi-node retention is
	// a later storage evolution. Setting it REQUIRES BackupPVC and ArchiveLocalPath
	// too — `felis manifests` enforces the trio (fail-loud).
	//
	// ARRANGEMENT: the reaper's resolver (cmd/felis/reaper.resolveWorldDir) looks
	// for <root>/<pvc> first and then for the stock local-path-provisioner layout
	// <root>/<pv-name>_<ns>_<pvc-name> — the exact directory name k3s uses under
	// its storage root (/var/lib/rancher/k3s/storage), derived from the live PVC's
	// spec.volumeName. So pointing this at the k3s storage root is the supported
	// way to enable retention on a stock install; other provisioners work if they
	// expose volumes as <root>/<pvc> or are read through the same PVC. On a
	// multi-node cluster every node HAS the root directory, but a world's directory
	// only exists on the node holding its volume: the CronJob schedules anywhere,
	// so a world found nowhere on that node fails the archive and is preserved.
	// The rendered CronJob is the correct K8s object; the actual tar depends on the
	// hosting node, which is not provable without a cluster.
	WorldsHostPath string
	// ReaperNode, when non-empty, pins the rendered reaper CronJob's pod to one node
	// via nodeSelector kubernetes.io/hostname — the multi-node answer to "the
	// hostPath root exists on every node but a world's directory lives on exactly
	// one". On a multi-node cluster the operator passes the node that holds the
	// world volumes (for the local-path starter: the node whose
	// /var/lib/rancher/k3s/storage carries them); leaving it empty keeps the
	// single-node behaviour (no selector). It is reaper-only and only meaningful
	// with WorldsHostPath set.
	ReaperNode string
	// ArchiveLocalPath is the path the backup PVC is mounted at inside the reaper
	// CronJob's pod, and MUST equal felis.toml's [archive] local_path. tarLocal writes
	// archive refs as absolute paths under [archive] local_path (internal/backup), and
	// the restore Job mounts the backup PVC at that same path so the stored ref
	// resolves (cmd/felis/api.go restoreConfig). `felis manifests` cannot read the
	// out-of-band config Secret, so this path is supplied explicitly and documented as
	// must-match. It is reaper-only and has no default; empty (with WorldsHostPath set)
	// is rejected fail-loud by the generator.
	ArchiveLocalPath string
	// RegistryStorage, UploadsStorage and BackupStorage are the capacities the
	// registry, uploads and world-archive PVCs request ("20Gi"); empty keeps
	// 10Gi, 5Gi and 10Gi. A PVC's request can only grow, and only on a class that
	// allows expansion, so the installer keeps an existing PVC's size. On k3s
	// local-path the request is a label: the volume is a directory on the node's
	// disk and nothing stops it outgrowing the number; the registry pruner, the
	// uploads budget (user_uploads_max_bytes) and the archive cap
	// (max_local_bytes) are what bound them there.
	RegistryStorage string
	UploadsStorage  string
	BackupStorage   string
}

// Validate reports a Params the renderer cannot turn into objects.
func (p Params) Validate() error {
	if p.Distributed && (p.ControllerNode == "" || p.EgressProbe == "" || p.BackupPVC == "" || p.ArchiveLocalPath == "") {
		return fmt.Errorf("distributed mode requires controller node, egress probe, backup PVC and archive path")
	}
	if p.Distributed && len(p.ServerEgressAllowCIDRs) > 0 {
		return fmt.Errorf("distributed mode does not permit private game egress exceptions")
	}

	if p.Distributed {
		for _, cidr := range append(append([]string{}, p.VelocityCIDRs...), p.RegistryNodeCIDRs...) {
			_, network, err := net.ParseCIDR(cidr)
			if err != nil {
				return fmt.Errorf("invalid node source %q", cidr)
			}
			ones, bits := network.Mask.Size()
			if ones != bits {
				return fmt.Errorf("distributed node sources must be exact /32 or /128 addresses: %q", cidr)
			}
		}
	}
	for _, q := range []struct{ name, v string }{
		{"registry storage", p.RegistryStorage},
		{"uploads storage", p.UploadsStorage},
		{"backup storage", p.BackupStorage},
	} {
		if q.v == "" {
			continue
		}
		v, err := resource.ParseQuantity(q.v)
		if err != nil || v.Sign() <= 0 {
			return fmt.Errorf("%s %q is not a positive size such as 20Gi", q.name, q.v)
		}
	}
	return nil
}

// withDefaults returns a copy of p with zero namespace/registry fields filled.
// VelocityCIDRs and PackageSourceCIDRs are intentionally left as-is: their empty
// states are meaningful (fail-closed game policy, no-internet build policy).
func (p Params) withDefaults() Params {
	if p.ControlNamespace == "" {
		p.ControlNamespace = DefaultControlNamespace
	}
	if p.MinecraftNamespace == "" {
		p.MinecraftNamespace = DefaultMinecraftNamespace
	}
	if p.BuildNamespace == "" {
		p.BuildNamespace = DefaultBuildNamespace
	}
	if p.RegistryNamespace == "" {
		p.RegistryNamespace = p.ControlNamespace
	}
	if p.RegistryPort == 0 {
		p.RegistryPort = defaultRegistryPort
	}
	if p.PanelNodePort == 0 {
		p.PanelNodePort = DefaultPanelNodePort
	}
	if p.RegistryImage == "" {
		p.RegistryImage = defaultRegistryImage
	}
	if p.PostgresImage == "" {
		p.PostgresImage = defaultPostgresImage
	}
	if p.RegistryStorage == "" {
		p.RegistryStorage = registryStorageSize
	}
	if p.UploadsStorage == "" {
		p.UploadsStorage = uploadsStorageSize
	}
	if p.BackupStorage == "" {
		p.BackupStorage = backupStorageSize
	}
	return p
}

// controlPlanePodLabels is the recommended-label set stamped on a control-plane
// workload of the given component. The NetworkPolicy RCON peer selects on the
// PartOf + Component subset, so the labels here and the selector there are one
// source of truth.
func controlPlanePodLabels(component string) map[string]string {
	return map[string]string{
		LabelName:      appName,
		LabelComponent: component,
		LabelPartOf:    controlPlanePartOf,
	}
}
