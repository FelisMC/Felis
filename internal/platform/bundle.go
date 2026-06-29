package platform

import (
	"bytes"
	"fmt"

	"felis.lolicon.best/internal/build"
	"felis.lolicon.best/internal/restore"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

// Object is the common interface of every rendered install object. runtime.Object
// supplies the GVK (so the YAML carries apiVersion/kind via the embedded
// TypeMeta) and metav1.Object supplies name/namespace. Every type Objects emits
// satisfies it, which lets the §22 tests iterate the bundle uniformly.
type Object interface {
	runtime.Object
	metav1.Object
}

// Objects assembles the complete security-fence install bundle for p, in a
// deterministic order: the namespaces, the control-plane identities (SAs +
// namespaced Roles + RoleBindings — felis-api and felis-operator always, plus the
// destructive felis-reaper identity only when retention is enabled, gated with its
// CronJob), the two weak Job SAs (build/restore, which have NO Role anywhere), the
// build-namespace egress NetworkPolicy, and the minecraft-namespace ingress
// NetworkPolicies.
//
// Scope: this is the authorization + network fence (spec §21, §22) plus the
// running control-plane workloads it fences — the felis-api / felis-operator
// Deployments and the in-cluster registry (Deployment + Service + PVC), which
// make the SAs and NetworkPolicy peers refer to something real (see workloads.go).
// The reaper CronJob is also part of Workloads, rendered only when the retention
// storage topology is supplied (WorldsHostPath + BackupPVC + ArchiveLocalPath —
// workloads.go documents the gate and the shape-asserted hostPath caveat). The
// per-server StatefulSet is never a static manifest — the operator renders it at
// reconcile time (internal/operator).
func Objects(p Params) []Object {
	p = p.withDefaults()
	var objs []Object

	// Namespaces first, each carrying the immutable name label the NetworkPolicy
	// namespaceSelectors match on. (K8s ≥1.21 adds this label automatically, but
	// rendering it makes the bundle self-contained and the selectors provable.)
	for _, ns := range distinctNamespaces(p) {
		objs = append(objs, namespaceObject(ns))
	}

	// Control-plane RBAC: SAs, then Roles, then RoleBindings.
	rbac := ControlPlaneRBAC(p)
	for _, sa := range rbac.ServiceAccounts {
		objs = append(objs, sa)
	}
	for _, r := range rbac.Roles {
		objs = append(objs, r)
	}
	for _, rb := range rbac.RoleBindings {
		objs = append(objs, rb)
	}

	// Weak Job SAs. They come from the build/restore packages (single source of
	// truth for AutomountServiceAccountToken=false), which set ObjectMeta but not
	// TypeMeta — stamp it so the YAML header is present. The restore Job runs in
	// the minecraft namespace (felis-api creates it there); the build Job in the
	// build namespace.
	objs = append(objs,
		withSATypeMeta(build.BuildServiceAccount(p.BuildNamespace, SABuild)),
		withSATypeMeta(restore.RestoreServiceAccount(p.MinecraftNamespace, SARestore)),
	)

	// Build-namespace egress lock (reused from internal/build; TypeMeta stamped).
	buildNP := build.BuildNetworkPolicy(build.NetPolParams{
		Namespace:          p.BuildNamespace,
		RegistryNamespace:  p.RegistryNamespace,
		RegistryPort:       p.RegistryPort,
		PackageSourceCIDRs: p.PackageSourceCIDRs,
	})
	buildNP.TypeMeta = metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"}
	objs = append(objs, buildNP)

	// Minecraft-namespace ingress fence (default-deny + RCON + game).
	for _, np := range MinecraftNetworkPolicies(p) {
		objs = append(objs, np)
	}

	// The running control-plane the fence protects: felis-api/operator Deployments
	// (which bind the SAs to workloads and stamp the RCON-peer labels) and the
	// in-cluster registry (Deployment + Service + PVC) the build egress policy
	// targets. Rendered last so the identities/policies they reference appear first.
	objs = append(objs, Workloads(p)...)

	return objs
}

// RenderYAML marshals Objects(p) into a single multi-document YAML stream (the
// `---`-separated form kubectl apply consumes). It is the verifiable source of
// truth a Helm chart would otherwise only re-encode.
func RenderYAML(p Params) ([]byte, error) {
	var buf bytes.Buffer
	for i, obj := range Objects(p) {
		if i > 0 {
			buf.WriteString("---\n")
		}
		b, err := yaml.Marshal(obj)
		if err != nil {
			return nil, fmt.Errorf("marshal %T %s/%s: %w", obj, obj.GetNamespace(), obj.GetName(), err)
		}
		buf.Write(b)
	}
	return buf.Bytes(), nil
}

// distinctNamespaces lists the namespaces the bundle installs into, de-duplicated
// and in a stable order (control, minecraft, build, then registry if it is a
// distinct namespace).
func distinctNamespaces(p Params) []string {
	order := []string{p.ControlNamespace, p.MinecraftNamespace, p.BuildNamespace, p.RegistryNamespace}
	seen := make(map[string]bool, len(order))
	out := make([]string, 0, len(order))
	for _, ns := range order {
		if ns == "" || seen[ns] {
			continue
		}
		seen[ns] = true
		out = append(out, ns)
	}
	return out
}

// namespaceObject renders a Namespace carrying the immutable name label the
// NetworkPolicy namespaceSelectors key on.
func namespaceObject(name string) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"kubernetes.io/metadata.name": name},
		},
	}
}

// withSATypeMeta stamps the apiVersion/kind on a ServiceAccount built by a package
// that only set its ObjectMeta.
func withSATypeMeta(sa *corev1.ServiceAccount) *corev1.ServiceAccount {
	sa.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}
	return sa
}
