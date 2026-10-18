package platform

import (
	"fmt"
	"net"

	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	LabelName      = "app.kubernetes.io/name"
	LabelComponent = "app.kubernetes.io/component"
	LabelPartOf    = "app.kubernetes.io/part-of"

	appName            = "felis"
	controlPlanePartOf = "felis-control-plane"

	ComponentAPI      = "api"
	ComponentOperator = "operator"
	ComponentReaper   = "reaper"

	// ComponentRegistry must stay outside part-of=felis-control-plane so RCON peers cannot select it.
	ComponentRegistry = "registry"

	// ComponentPostgres must stay outside part-of=felis-control-plane so RCON and internal-API peers cannot select it.
	ComponentPostgres = "postgres"
)

// Weak build and restore ServiceAccounts must have no RoleBindings.
const (
	SAAPI      = "felis-api"
	SAOperator = "felis-operator"
	SAReaper   = "felis-reaper"
	SABuild    = "felis-build"
	SARestore  = "felis-restore"
)

const (
	DefaultControlNamespace   = "felis"
	DefaultMinecraftNamespace = "minecraft"
	DefaultBuildNamespace     = "felis-build"
	DefaultPanelNodePort      = int32(30443)

	defaultRegistryPort int32 = 5000

	// defaultRegistryImage must match bootstrap REGISTRY_IMAGE so import and garbage-collection pins use the same digest.
	defaultRegistryImage = "docker.io/library/registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"

	// defaultPostgresImage must match bootstrap POSTGRES_IMAGE; major-version changes require planned data migration.
	defaultPostgresImage = "docker.io/library/postgres:18.6-trixie@sha256:5a5a84b19854a9ffaa54082c166ff4ec27473a361e496e5ea167f298f2da9722"
)

type Params struct {
	NodeControlSocket string
	NodeControlNode   string
	Distributed       bool
	ControllerNode    string
	// ControllerHA requires Distributed and uses ReplicatedStorageClass for control-plane data; defaults are the replicated class and 10Gi database storage.
	ControllerHA           bool
	ReplicatedStorageClass string
	PostgresStorage        string
	EgressProbe            string
	RegistryNodeCIDRs      []string
	// Control accounts live in ControlNamespace; the reaper account lives with its Minecraft-namespace PVCs.
	ControlNamespace   string
	MinecraftNamespace string
	BuildNamespace     string
	// VelocityCIDRs has no default; it constrains non-node game traffic because resident-node traffic bypasses NetworkPolicy.
	VelocityCIDRs []string
	// RegistryNamespace defaults to ControlNamespace.
	RegistryNamespace string
	RegistryPort      int32
	PanelNodePort     int32
	// PackageSourceCIDRs empty disables build-Pod internet egress.
	PackageSourceCIDRs []string
	// ServerEgressDenyCIDRs must include node public addresses to prevent tenant plugins reaching host services through them.
	ServerEgressDenyCIDRs []string
	// ServerEgressAllowCIDRs explicitly permits private destinations otherwise excluded; empty allows none.
	ServerEgressAllowCIDRs []string
	// FelisImage has no default and must also be passed to API Job executors to keep their binary image consistent.
	FelisImage    string
	RegistryImage string
	PostgresImage string
	// BackupPVC empty disables archive-volume rendering and backup or restore wiring; the claim lives with its mounting Pods.
	BackupPVC string
	// WorldsHostPath enables world reaping only with BackupPVC and ArchiveLocalPath; missing local worlds are preserved on archive failure.
	WorldsHostPath string
	// ReaperNode pins the reaper to the node holding hostPath worlds; empty leaves scheduling unpinned.
	ReaperNode string
	// ArchiveLocalPath must equal felis.toml archive.local_path because stored archive references are absolute.
	ArchiveLocalPath string
	// PVC requests do not enforce local-path capacity; registry pruning and upload or archive budgets provide the actual bounds.
	RegistryStorage  string
	UploadsStorage   string
	BackupStorage    string
	ArtifactsStorage string
}

func (p Params) Validate() error {
	if p.ControllerHA && !p.Distributed {
		return fmt.Errorf("controller high availability requires distributed mode")
	}
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
		{"artifact cache storage", p.ArtifactsStorage},
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

// withDefaults preserves empty CIDR lists because they represent fail-closed game ingress and disabled build internet access.
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
	if p.ArtifactsStorage == "" {
		p.ArtifactsStorage = artifactsStorageSize
	}
	if p.ControllerHA && p.ReplicatedStorageClass == "" {
		p.ReplicatedStorageClass = DefaultReplicatedStorageClass
	}
	if p.PostgresStorage == "" {
		p.PostgresStorage = postgresStorageSize
	}
	return p
}

func controlPlanePodLabels(component string) map[string]string {
	return map[string]string{
		LabelName:      appName,
		LabelComponent: component,
		LabelPartOf:    controlPlanePartOf,
	}
}
