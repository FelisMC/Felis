package platform

import (
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"
)

func TestObjects_EveryDocHasTypeMeta(t *testing.T) {
	for _, obj := range Objects(testParams()) {
		gvk := obj.GetObjectKind().GroupVersionKind()
		if gvk.Kind == "" || gvk.Version == "" {
			t.Errorf("%T %s/%s has empty TypeMeta (kind=%q version=%q)",
				obj, obj.GetNamespace(), obj.GetName(), gvk.Kind, gvk.Version)
		}
	}
}

func TestObjects_CarryTheFences(t *testing.T) {
	want := map[string]string{
		"felis-default-deny-ingress":  "minecraft",
		"felis-server-egress":         "minecraft",
		"felis-login-to-internal-api": "minecraft",
		"felis-registry-ingress":      "felis",
	}
	for _, obj := range Objects(testParams()) {
		np, ok := obj.(*networkingv1.NetworkPolicy)
		if !ok {
			continue
		}
		if ns, expected := want[np.Name]; expected {
			if np.Namespace != ns {
				t.Errorf("%s in namespace %q, want %q", np.Name, np.Namespace, ns)
			}
			delete(want, np.Name)
		}
	}
	for name := range want {
		t.Errorf("bundle is missing NetworkPolicy %s", name)
	}
}

func TestObjects_NamespacesLabeled(t *testing.T) {
	want := map[string]bool{"felis": false, "minecraft": false, "felis-build": false}
	for _, obj := range Objects(testParams()) {
		ns, ok := obj.(*corev1.Namespace)
		if !ok {
			continue
		}
		if _, expected := want[ns.Name]; expected {
			want[ns.Name] = true
		}
		if got := ns.Labels["kubernetes.io/metadata.name"]; got != ns.Name {
			t.Errorf("namespace %q metadata.name label = %q, want %q", ns.Name, got, ns.Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("namespace %q not rendered", name)
		}
	}
}

func TestObjects_MinecraftNamespaceEnforcesBaseline(t *testing.T) {
	p := reaperParams()
	for _, obj := range Objects(p) {
		if ns, ok := obj.(*corev1.Namespace); ok {
			enforce := ns.Labels["pod-security.kubernetes.io/enforce"]
			switch ns.Name {
			case "minecraft":
				if enforce != "baseline" || ns.Labels["pod-security.kubernetes.io/warn"] != "baseline" {
					t.Errorf("minecraft namespace labels = %v, want enforce+warn baseline", ns.Labels)
				}
			default:
				if enforce != "" {
					t.Errorf("namespace %s must not be labelled by the bundle, got enforce=%q", ns.Name, enforce)
				}
			}
		}
		if obj.GetNamespace() != "minecraft" {
			continue
		}
		var spec *corev1.PodSpec
		switch o := obj.(type) {
		case *batchv1.CronJob:
			spec = &o.Spec.JobTemplate.Spec.Template.Spec
		case *appsv1.Deployment:
			spec = &o.Spec.Template.Spec
		}
		if spec == nil {
			continue
		}
		for _, v := range spec.Volumes {
			if v.HostPath != nil {
				t.Errorf("%s/%s mounts inline hostPath %q, which baseline refuses", obj.GetNamespace(), obj.GetName(), v.HostPath.Path)
			}
		}
		for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
			for _, port := range c.Ports {
				if port.HostPort != 0 {
					t.Errorf("%s/%s container %s uses hostPort %d, which baseline refuses", obj.GetNamespace(), obj.GetName(), c.Name, port.HostPort)
				}
			}
		}
	}

	shared := testParams()
	shared.MinecraftNamespace = shared.withDefaults().ControlNamespace
	for _, obj := range Objects(shared) {
		if ns, ok := obj.(*corev1.Namespace); ok && ns.Labels["pod-security.kubernetes.io/enforce"] != "" {
			t.Errorf("a minecraft namespace shared with the control plane must stay unlabelled, got %v", ns.Labels)
		}
	}
}

func TestWeakJobSAs_Isolated(t *testing.T) {
	objs := Objects(testParams())

	var build, restore *corev1.ServiceAccount
	for _, obj := range objs {
		sa, ok := obj.(*corev1.ServiceAccount)
		if !ok {
			continue
		}
		switch sa.Name {
		case SABuild:
			build = sa
		case SARestore:
			restore = sa
		}
	}
	if build == nil {
		t.Fatal("build SA not rendered")
	}
	if restore == nil {
		t.Fatal("restore SA not rendered")
	}
	for _, sa := range []*corev1.ServiceAccount{build, restore} {
		if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
			t.Errorf("%s must set AutomountServiceAccountToken=false", sa.Name)
		}
	}

	// No RoleBinding may name the weak SAs as a subject.
	for _, rb := range ControlPlaneRBAC(testParams()).RoleBindings {
		for _, s := range rb.Subjects {
			if s.Name == SABuild || s.Name == SARestore {
				t.Errorf("binding %q must NOT grant any Role to weak SA %q", rb.Name, s.Name)
			}
		}
	}
}

func TestRenderYAML_ParsesAndIsFenced(t *testing.T) {
	out, err := RenderYAML(testParams())
	if err != nil {
		t.Fatalf("RenderYAML: %v", err)
	}
	text := string(out)

	if strings.Contains(text, "ClusterRole") {
		t.Error("rendered bundle must not contain ClusterRole or ClusterRoleBinding")
	}

	kinds := map[string]bool{}
	for _, doc := range strings.Split(text, "\n---\n") {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		var m map[string]interface{}
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("doc does not parse: %v\n---\n%s", err, doc)
		}
		kind, _ := m["kind"].(string)
		apiVersion, _ := m["apiVersion"].(string)
		if kind == "" || apiVersion == "" {
			t.Errorf("doc missing apiVersion/kind: %s", doc)
		}
		kinds[kind] = true
	}
	for _, want := range []string{"Namespace", "ServiceAccount", "Role", "RoleBinding", "NetworkPolicy"} {
		if !kinds[want] {
			t.Errorf("rendered bundle is missing a %s", want)
		}
	}
}

func TestRenderYAML_Deterministic(t *testing.T) {
	a, err := RenderYAML(testParams())
	if err != nil {
		t.Fatal(err)
	}
	b, err := RenderYAML(testParams())
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Error("RenderYAML must be deterministic across calls")
	}
}

func TestObjects_EveryPodRunsAsARenderedServiceAccount(t *testing.T) {
	retention := testParams()
	retention.BackupPVC, retention.ArchiveLocalPath = "felis-backups", "/backups"
	reaping := retention
	reaping.WorldsHostPath = "/var/lib/felis/worlds"
	for _, c := range []struct {
		name string
		p    Params
		pods int
	}{
		{"no archive store", testParams(), 5},
		{"retention only", retention, 6},
		{"reaping", reaping, 6},
	} {
		objs := Objects(c.p)
		sas := map[string]bool{}
		for _, o := range objs {
			if sa, ok := o.(*corev1.ServiceAccount); ok {
				sas[sa.Namespace+"/"+sa.Name] = true
			}
		}
		pods := 0
		for _, o := range objs {
			var spec *corev1.PodSpec
			switch w := o.(type) {
			case *appsv1.Deployment:
				spec = &w.Spec.Template.Spec
			case *appsv1.StatefulSet:
				spec = &w.Spec.Template.Spec
			case *appsv1.DaemonSet:
				spec = &w.Spec.Template.Spec
			case *batchv1.Job:
				spec = &w.Spec.Template.Spec
			case *batchv1.CronJob:
				spec = &w.Spec.JobTemplate.Spec.Template.Spec
			default:
				continue
			}
			pods++
			if sa := spec.ServiceAccountName; sa != "" && sa != "default" && !sas[o.GetNamespace()+"/"+sa] {
				t.Errorf("%s: %T %s/%s runs as ServiceAccount %q, which the bundle does not render there",
					c.name, o, o.GetNamespace(), o.GetName(), sa)
			}
		}
		if pods != c.pods {
			t.Errorf("%s: %d pod templates checked, want %d", c.name, pods, c.pods)
		}
	}
}
