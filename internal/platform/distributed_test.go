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

func TestNodeControlSocketIsAPIOnly(t *testing.T) {
	p := testParams()
	p.NodeControlSocket = "/run/felis-node-control/control.sock"
	p.NodeControlNode = "controller"
	for _, object := range Objects(p) {
		d, ok := object.(*appsv1.Deployment)
		if !ok {
			continue
		}
		for _, volume := range d.Spec.Template.Spec.Volumes {
			if volume.Name == "node-control" && d.Name != SAAPI {
				t.Fatal("host socket leaked to", d.Name)
			}
		}
		if d.Name != SAAPI {
			if len(d.Spec.Template.Spec.SecurityContext.SupplementalGroups) != 0 {
				t.Fatal("host socket group leaked to", d.Name)
			}
			continue
		}
		groups := d.Spec.Template.Spec.SecurityContext.SupplementalGroups
		if len(groups) != 1 || groups[0] != 65532 {
			t.Fatal("API lacks host socket group", groups)
		}
		if *d.Spec.Template.Spec.SecurityContext.RunAsUser != nonRootUID || *d.Spec.Template.Spec.SecurityContext.RunAsGroup != nonRootUID {
			t.Fatal("API identity changed")
		}
		if d.Spec.Template.Spec.NodeSelector["kubernetes.io/hostname"] != "controller" {
			t.Fatal("API can run away from socket")
		}
		found := false
		for _, container := range d.Spec.Template.Spec.Containers {
			for _, mount := range container.VolumeMounts {
				if mount.Name == "node-control" {
					found = true
					if !mount.ReadOnly {
						t.Fatal("host directory writable")
					}
				}
			}
			if d.Spec.Template.Spec.SecurityContext.RunAsUser == nil || *d.Spec.Template.Spec.SecurityContext.RunAsUser == 0 {
				t.Fatal("API elevated")
			}
		}
		if !found {
			t.Fatal("API has no node socket")
		}
	}
}
