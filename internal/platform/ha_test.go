package platform

import (
	"testing"

	"felis.lolicon.best/internal/placement"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
)

func haParams() Params {
	p := testParams()
	p.Distributed = true
	p.ControllerNode = "a"
	p.EgressProbe = "felis-api.felis.svc:443"
	p.BackupPVC = "felis-backups"
	p.ArchiveLocalPath = "/backups"
	p.VelocityCIDRs = []string{"192.0.2.1/32"}
	p.ControllerHA = true
	return p
}

func haEnv(spec corev1.PodSpec, name string) (string, bool) {
	for _, c := range spec.Containers {
		for _, e := range c.Env {
			if e.Name == name {
				return e.Value, true
			}
		}
	}
	return "", false
}

func claimsOf(spec corev1.PodSpec) map[string]bool {
	out := map[string]bool{}
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			out[v.PersistentVolumeClaim.ClaimName] = true
		}
		if v.HostPath != nil {
			out["hostPath:"+v.HostPath.Path] = true
		}
	}
	return out
}

func quickEviction(spec corev1.PodSpec) bool {
	found := map[string]bool{}
	for _, t := range spec.Tolerations {
		if t.Effect == corev1.TaintEffectNoExecute && t.TolerationSeconds != nil && *t.TolerationSeconds == haEvictionSeconds {
			found[t.Key] = true
		}
	}
	return found[corev1.TaintNodeUnreachable] && found[corev1.TaintNodeNotReady]
}

func besidePostgres(spec corev1.PodSpec) bool {
	a := spec.Affinity
	if a == nil || a.PodAffinity == nil || len(a.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		return false
	}
	term := a.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0]
	return term.TopologyKey == "kubernetes.io/hostname" && term.LabelSelector.MatchLabels[LabelComponent] == ComponentPostgres
}

func TestControllerHARendersAMovableControlPlane(t *testing.T) {
	p := haParams()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	pods := map[string]corev1.PodSpec{}
	pvcs := map[string]*corev1.PersistentVolumeClaim{}
	var class *storagev1.StorageClass
	for _, obj := range Objects(p) {
		switch o := obj.(type) {
		case *appsv1.Deployment:
			pods[o.Name] = o.Spec.Template.Spec
		case *batchv1.CronJob:
			pods[o.Name] = o.Spec.JobTemplate.Spec.Template.Spec
		case *corev1.PersistentVolumeClaim:
			pvcs[o.Name] = o
		case *storagev1.StorageClass:
			class = o
		}
	}
	for _, name := range []string{SAAPI, SAOperator, registryName, ArchiveName, PostgresName, "felis-reaper"} {
		spec, ok := pods[name]
		if !ok {
			t.Fatalf("no %s in the bundle (have %v)", name, keys(pods))
		}
		if len(spec.NodeSelector) != 1 || spec.NodeSelector[placement.LabelRole] != placement.RoleController {
			t.Errorf("%s selector = %v, want any controller", name, spec.NodeSelector)
		}
		if !quickEviction(spec) {
			t.Errorf("%s tolerations = %+v, want a %ds eviction", name, spec.Tolerations, haEvictionSeconds)
		}
		if want := name != PostgresName; besidePostgres(spec) != want {
			t.Errorf("%s beside PostgreSQL = %v, want %v", name, !want, want)
		}
	}
	for name, claim := range map[string]string{SAAPI: "felis-uploads-ha", registryName: "registry-ha", ArchiveName: "felis-backups-ha", PostgresName: PostgresHAClaim} {
		if got := claimsOf(pods[name]); !got[claim] || got["hostPath:"+PostgresDataHostPath] {
			t.Errorf("%s volumes = %v, want %s", name, got, claim)
		}
	}
	if v, _ := haEnv(pods[SAAPI], "FELIS_BACKUP_PVC"); v != "felis-backups-ha" {
		t.Errorf("FELIS_BACKUP_PVC = %q", v)
	}
	for _, name := range []string{SAAPI, SAOperator} {
		if v, _ := haEnv(pods[name], "FELIS_CONTROLLER_HA"); v != "true" {
			t.Errorf("%s FELIS_CONTROLLER_HA = %q", name, v)
		}
	}
	for _, name := range []string{"felis-uploads-ha", "registry-ha", "felis-backups-ha", PostgresHAClaim} {
		pvc := pvcs[name]
		if pvc == nil || pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != DefaultReplicatedStorageClass {
			t.Errorf("claim %s = %+v", name, pvc)
		}
	}
	for _, old := range []string{uploadsPVCName, registryName, "felis-backups"} {
		if pvcs[old] != nil {
			t.Errorf("the HA bundle renders the original claim %s, which a full apply would then own", old)
		}
	}
	if class == nil || class.Provisioner != "driver.longhorn.io" || *class.ReclaimPolicy != corev1.PersistentVolumeReclaimRetain || class.Parameters["numberOfReplicas"] != "3" {
		t.Fatalf("storage class = %+v", class)
	}

	// The database rendered alone, as the installer applies it first, brings its class
	// and claim with it.
	alone := map[string]bool{}
	for _, obj := range PostgresObjects(p) {
		switch o := obj.(type) {
		case *storagev1.StorageClass:
			alone["class"] = true
		case *corev1.PersistentVolumeClaim:
			alone[o.Name] = true
		}
	}
	if !alone["class"] || !alone[PostgresHAClaim] {
		t.Errorf("postgres alone = %v", alone)
	}
}

func TestSingleControllerStaysPinned(t *testing.T) {
	p := haParams()
	p.ControllerHA = false
	for _, obj := range Objects(p) {
		switch o := obj.(type) {
		case *appsv1.Deployment:
			spec := o.Spec.Template.Spec
			if spec.NodeSelector[placement.LabelIdentity] != "a" || len(spec.Tolerations) != 0 || spec.Affinity != nil {
				t.Errorf("%s = selector %v, tolerations %v, affinity %v", o.Name, spec.NodeSelector, spec.Tolerations, spec.Affinity)
			}
			if _, ok := haEnv(spec, "FELIS_CONTROLLER_HA"); ok {
				t.Errorf("%s carries FELIS_CONTROLLER_HA", o.Name)
			}
			if o.Name == PostgresName && !claimsOf(spec)["hostPath:"+PostgresDataHostPath] {
				t.Errorf("postgres volumes = %v", claimsOf(spec))
			}
		case *corev1.PersistentVolumeClaim:
			if o.Spec.StorageClassName != nil || o.Name == PostgresHAClaim {
				t.Errorf("claim %s on class %v", o.Name, o.Spec.StorageClassName)
			}
		case *storagev1.StorageClass:
			t.Errorf("a single controller renders storage class %s", o.Name)
		}
	}
	p.ControllerHA, p.Distributed = true, false
	if err := p.Validate(); err == nil {
		t.Error("HA without distributed mode validated")
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
