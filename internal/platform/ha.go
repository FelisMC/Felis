package platform

import (
	"felis.lolicon.best/internal/placement"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HA volumes use suffixed replicated claims and retain originals because existing claim storage classes cannot change.

const (
	DefaultReplicatedStorageClass = "felis-replicated"

	haSuffix                  = "-ha"
	haEvictionSeconds   int64 = 30
	postgresStorageSize       = "10Gi"
	PostgresHAClaim           = PostgresName + haSuffix
)

func HAClaimName(ha bool, base string) string {
	if ha && base != "" {
		return base + haSuffix
	}
	return base
}

func claimName(p Params, base string) string {
	return HAClaimName(p.ControllerHA, base)
}

func storageClassFor(p Params) *string {
	if !p.ControllerHA {
		return nil
	}
	class := p.ReplicatedStorageClass
	return &class
}

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

func haPlacement(p Params, spec *corev1.PodSpec) {
	spec.Tolerations = append(spec.Tolerations, haTolerations(p)...)
	if a := withPostgres(p); a != nil {
		spec.Affinity = a
	}
}

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

// ReplicatedStorageClass keeps three Longhorn copies and retains data after claim deletion.
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

func controllerRoleSelector() map[string]string {
	return map[string]string{placement.LabelRole: placement.RoleController}
}
