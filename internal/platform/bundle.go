package platform

import (
	"bytes"
	"fmt"
	"maps"

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
// build-namespace egress NetworkPolicy, LimitRange and ResourceQuota, and the minecraft-namespace
// ingress NetworkPolicies.
//
// Scope: this is the authorization + network fence (spec §21, §22) plus the
// running control-plane workloads it fences — the felis-api / felis-operator
// Deployments and the in-cluster registry (Deployment + Service + PVC), which
// make the SAs and NetworkPolicy peers refer to something real, plus the
// world-archive PVC that backs backup/restore when a backup PVC is named (see
// workloads.go). Every object here is namespaced; the control plane's
// node-pressure eviction shield is the BUILT-IN system-cluster-critical
// PriorityClass the pod templates reference (workloads.go controlPlanePriorityName),
// not an object this bundle renders.
// The reaper CronJob is also part of Workloads: it reaps worlds when the full
// storage topology is supplied (WorldsHostPath + BackupPVC + ArchiveLocalPath)
// and only looks after the archive store when the worlds root is missing
// (workloads.go documents both gates and the shape-asserted hostPath caveat). The
// per-server StatefulSet is never a static manifest — the operator renders it at
// reconcile time (internal/operator).
func Objects(p Params) []Object {
	p = p.withDefaults()
	var objs []Object

	// Namespaces first, each carrying the immutable name label the NetworkPolicy
	// namespaceSelectors match on. (K8s ≥1.21 adds this label automatically, but
	// rendering it makes the bundle self-contained and the selectors provable.)
	for _, ns := range distinctNamespaces(p) {
		nsObj := namespaceObject(ns)
		if ns == p.MinecraftNamespace && minecraftNamespaceIsOwn(p) {
			maps.Copy(nsObj.Labels, minecraftPodSecurityLabels)
		}
		objs = append(objs, nsObj)
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
		Namespace:         p.BuildNamespace,
		RegistryNamespace: p.RegistryNamespace,
		RegistryPort:      p.RegistryPort,
		ControlNamespace:  p.ControlNamespace,
		// The internal face's port, single-sourced with the api Deployment below.
		APIPort:            apiInternalPort,
		PackageSourceCIDRs: p.PackageSourceCIDRs,
	})
	buildNP.TypeMeta = metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"}
	objs = append(objs, buildNP)
	buildLR := build.BuildLimitRange(p.BuildNamespace)
	buildLR.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "LimitRange"}
	objs = append(objs, buildLR)
	buildRQ := build.BuildResourceQuota(p.BuildNamespace)
	buildRQ.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ResourceQuota"}
	objs = append(objs, buildRQ)

	// Minecraft-namespace ingress fence (default-deny + RCON + game), the server
	// egress fence, and the registry's ingress fence.
	for _, np := range MinecraftNetworkPolicies(p) {
		objs = append(objs, np)
	}
	for _, np := range ServerEgressPolicies(p) {
		objs = append(objs, np)
	}
	objs = append(objs, RegistryIngressPolicy(p))

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

// minecraftPodSecurityLabels put the minecraft namespace under the PodSecurity
// admission baseline profile. Everything Felis runs there fits it: game servers run
// as naming.GameUID with every capability dropped, their prepare-data init and the
// file/backup/restore Jobs run as root holding at most CHOWN and DAC_OVERRIDE (both
// on baseline's allow-list), and the reaper reaches the node's worlds-root through a
// static PV rather than an inline hostPath. What baseline then refuses — privileged
// containers, host namespaces and ports, inline hostPath, extra capabilities — is
// exactly what a pod smuggled in through any other write path to this namespace
// would need to reach the node.
//
// warn repeats the enforced level so a StatefulSet or Job that would render a
// refused pod reports it at apply time, instead of the controller failing to create
// pods quietly. audit records restricted-profile violations for the path toward
// restricted (only the root prepare-data init and root Jobs stand in its way).
var minecraftPodSecurityLabels = map[string]string{
	"pod-security.kubernetes.io/enforce":         "baseline",
	"pod-security.kubernetes.io/enforce-version": "latest",
	"pod-security.kubernetes.io/warn":            "baseline",
	"pod-security.kubernetes.io/warn-version":    "latest",
	"pod-security.kubernetes.io/audit":           "restricted",
	"pod-security.kubernetes.io/audit-version":   "latest",
}

// minecraftNamespaceIsOwn reports whether the minecraft namespace is shared with
// no other component. The registry (hostPort) and the build Jobs do not fit the
// baseline profile, so the labels go on only when neither lives there, and the
// control plane's namespace is never labelled from here.
func minecraftNamespaceIsOwn(p Params) bool {
	return p.MinecraftNamespace != p.ControlNamespace &&
		p.MinecraftNamespace != p.BuildNamespace &&
		p.MinecraftNamespace != p.RegistryNamespace
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
