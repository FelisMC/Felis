package platform

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// TestObjects_EveryDocHasTypeMeta enforces that every rendered object carries an
// apiVersion and a kind. The build/restore packages build their SAs/NetworkPolicy
// without TypeMeta, so this guards the stamping in bundle.go specifically.
func TestObjects_EveryDocHasTypeMeta(t *testing.T) {
	for _, obj := range Objects(testParams()) {
		gvk := obj.GetObjectKind().GroupVersionKind()
		if gvk.Kind == "" || gvk.Version == "" {
			t.Errorf("%T %s/%s has empty TypeMeta (kind=%q version=%q)",
				obj, obj.GetNamespace(), obj.GetName(), gvk.Kind, gvk.Version)
		}
	}
}

// TestObjects_NamespacesLabeled checks the three namespaces are rendered with the
// immutable name label the NetworkPolicy namespaceSelectors key on.
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

// TestWeakJobSAs_Isolated proves the build/restore SAs are present, disable token
// auto-mounting, and — the key isolation invariant — are referenced by NO
// RoleBinding anywhere. Their powerlessness is the absence of any binding.
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

// TestRenderYAML_ParsesAndIsFenced renders the full bundle and asserts: every
// document parses with an apiVersion+kind, the expected kinds are present, and the
// stream contains no cluster-scoped RBAC (the ClusterRole/ClusterRoleBinding red
// line, checked on the literal output the way CI would).
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

// TestRenderYAML_Deterministic guards that the render is stable (no map-ordering
// nondeterminism leaking into the manifest), so a regenerated bundle diffs cleanly.
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
