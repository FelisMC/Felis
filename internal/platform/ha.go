package platform

import (
	"felis.lolicon.best/internal/placement"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Controller high availability (Params.ControllerHA). Several controllers run k3s with
// embedded etcd, so the cluster keeps its API while one of them is lost; the control
// plane itself stays a single active set of pods that moves between them:
//
//   - its data lives on replicated volumes (ReplicatedStorageClass, Longhorn's), each
//     copy on another controller, so a move loses nothing that was written;
//   - every control-plane pod may run on any controller (the role label, not one
//     identity), and leaves a node that stopped answering after haEvictionSeconds
//     instead of the default five minutes;
//   - the pods that hold volumes run together on the node running PostgreSQL. A
//     replicated volume is attached to one node at a time, and host tools on that node
//     (the host leader: `felis ha leader`) reach each of them there, as they reach the
//     database on its loopback port.
//
// The originals of the volumes stay as they were, beside the replicated copies, which
// carry haSuffix: a claim's class cannot change, and keeping them makes the way back
// possible.

const (
	// DefaultReplicatedStorageClass is the StorageClass deploy/bootstrap.sh creates on
	// Longhorn for the control plane.
	DefaultReplicatedStorageClass = "felis-replicated"
	// haSuffix names the replicated copy of a control-plane volume.
	haSuffix = "-ha"
	// haEvictionSeconds is how long a control-plane pod stays bound to a node that stopped
	// responding before it is moved.
	haEvictionSeconds int64 = 30
	// postgresStorageSize is the replicated database volume's default capacity.
	postgresStorageSize = "10Gi"
	// PostgresHAClaim is the database's replicated volume.
	PostgresHAClaim = PostgresName + haSuffix
)

// HAClaimName is the claim a control-plane volume uses: base itself, or its replicated
// copy in ControllerHA mode.
func HAClaimName(ha bool, base string) string {
	if ha && base != "" {
		return base + haSuffix
	}
	return base
}

func claimName(p Params, base string) string {
	return HAClaimName(p.ControllerHA, base)
}

// storageClassFor is the class of a control-plane claim: the cluster default, or the
// replicated class in ControllerHA mode.
func storageClassFor(p Params) *string {
	if !p.ControllerHA {
		return nil
	}
	class := p.ReplicatedStorageClass
	return &class
}

// haTolerations move a control-plane pod off a node that stopped answering after
// haEvictionSeconds; outside ControllerHA there is nowhere to move it.
func haTolerations(p Params) []corev1.Toleration {
	if !p.ControllerHA {
		return nil
	}
	seconds := haEvictionSeconds
	var out []corev1.Toleration
	for _, key := range []string{corev1.TaintNodeUnreachable, corev1.TaintNodeNotReady} {
		out = append(out, corev1.Toleration{Key: key, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute, TolerationSeconds: &seconds})
	}
	return out
}

// withPostgres keeps a control-plane pod on the node running PostgreSQL in ControllerHA
// mode.
func withPostgres(p Params) *corev1.Affinity {
	if !p.ControllerHA {
		return nil
	}
	return &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
			LabelSelector: &metav1.LabelSelector{MatchLabels: postgresLabels()},
			Namespaces:    []string{p.ControlNamespace},
			TopologyKey:   "kubernetes.io/hostname",
		}},
	}}
}

// haPlacement applies the ControllerHA placement to a control-plane pod: the role
// selector (controllerSelector), the quick eviction, and the place beside PostgreSQL.
func haPlacement(p Params, spec *corev1.PodSpec) {
	spec.Tolerations = append(spec.Tolerations, haTolerations(p)...)
	if a := withPostgres(p); a != nil {
		spec.Affinity = a
	}
}

// postgresPVC is the database's replicated volume in ControllerHA mode.
func postgresPVC(p Params) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaim"},
		ObjectMeta: metav1.ObjectMeta{Name: PostgresHAClaim, Namespace: p.ControlNamespace, Labels: postgresLabels()},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: storageClassFor(p),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(p.PostgresStorage)},
			},
		},
	}
}

// ReplicatedStorageClass is the Longhorn class of the control plane's volumes: three
// copies (one per controller, as Longhorn puts each on another node), kept when their
// claim is deleted, and preferably one beside the pod.
func ReplicatedStorageClass(p Params) *storagev1.StorageClass {
	p = p.withDefaults()
	retain := corev1.PersistentVolumeReclaimRetain
	immediate := storagev1.VolumeBindingImmediate
	return &storagev1.StorageClass{
		TypeMeta:             metav1.TypeMeta{APIVersion: "storage.k8s.io/v1", Kind: "StorageClass"},
		ObjectMeta:           metav1.ObjectMeta{Name: p.ReplicatedStorageClass, Labels: map[string]string{LabelName: appName}},
		Provisioner:          "driver.longhorn.io",
		ReclaimPolicy:        &retain,
		VolumeBindingMode:    &immediate,
		AllowVolumeExpansion: boolPtr(true),
		Parameters: map[string]string{
			"numberOfReplicas":    "3",
			"staleReplicaTimeout": "30",
			"dataLocality":        "best-effort",
		},
	}
}

// controllerRoleSelector picks any controller.
func controllerRoleSelector() map[string]string {
	return map[string]string{placement.LabelRole: placement.RoleController}
}
