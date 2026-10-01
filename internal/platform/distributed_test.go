package platform

import (
	"testing"

	"felis.lolicon.best/internal/archivetransfer"
	"felis.lolicon.best/internal/placement"
	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
)

func TestDistributedControllerPinningAndArchivePowers(t *testing.T) {
	p := testParams()
	p.Distributed = true
	p.ControllerNode = "a"
	p.EgressProbe = "felis-api.felis.svc:443"
	p.BackupPVC = "felis-backups"
	p.ArchiveLocalPath = "/backups"
	p.VelocityCIDRs = []string{"192.0.2.1/32"}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, obj := range Objects(p) {
		if d, ok := obj.(*appsv1.Deployment); ok {
			if d.Spec.Template.Spec.NodeSelector[placement.LabelIdentity] != "a" {
				t.Fatalf("controller workload %s may run on worker", d.Name)
			}
			if d.Name == ArchiveName {
				found = true
				s := d.Spec.Template.Spec
				if s.AutomountServiceAccountToken == nil || *s.AutomountServiceAccountToken || s.ServiceAccountName != "" || len(s.Volumes) != 1 || s.Volumes[0].PersistentVolumeClaim.ClaimName != p.BackupPVC {
					t.Fatal("archive has extra powers")
				}
				for _, c := range s.Containers {
					for _, e := range c.Env {
						if e.Name != archivetransfer.KeyEnv {
							t.Fatal("archive received controller config")
						}
					}
				}
			}
		}
		if r, ok := obj.(*rbacv1.ClusterRole); ok {
			for _, rule := range r.Rules {
				for _, verb := range rule.Verbs {
					if verb != "get" && verb != "list" {
						t.Fatal("distributed cluster grant writes", r.Name, verb)
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("archive service missing")
	}
	p.VelocityCIDRs = []string{"10.0.0.0/8"}
	if p.Validate() == nil {
		t.Fatal("broad Velocity source accepted")
	}
}
