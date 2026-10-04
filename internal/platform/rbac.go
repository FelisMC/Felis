package platform

import (
	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// API groups used by the rules. The felis group is sourced from v1alpha1 so the
// CRD's identity and its RBAC can never drift apart.
const (
	groupCore  = "" // core/v1: secrets, services, persistentvolumeclaims
	groupApps  = "apps"
	groupBatch = "batch"
)

var groupFelis = v1alpha1.GroupName // "felis.lolicon.best"

// RBAC is the control-plane authorization bundle: one SA per identity and the
// namespaced Roles + RoleBindings that grant each exactly the verbs its code path
// exercises. DistributedRBAC adds read-only node/PV grants when distributed mode is enabled.
type RBAC struct {
	ServiceAccounts []*corev1.ServiceAccount
	Roles           []*rbacv1.Role
	RoleBindings    []*rbacv1.RoleBinding
}

// ControlPlaneRBAC assembles the full RBAC bundle for p.
//
// The reaper identity (felis-reaper SA + Role + RoleBinding) is rendered ONLY when
// the retention reaper CronJob is — both gate on reaperEnabled(p), the same storage
// trio (workloads.go). This coupling is deliberate least-privilege: the reaper's
// Role is the one and only place persistentvolumeclaims:delete appears in the whole
// bundle (world reclamation) — neither felis-api nor felis-operator can delete a
// PVC. Leaving that destructive grant standing in a deployment that never runs the
// reaper would widen the blast radius of a control-plane compromise for no benefit
// (a control-namespace foothold could mount felis-reaper and destroy world PVCs),
// since nothing would consume it. So the destructive identity exists exactly as
// long as its consumer does, and the manifests command's fail-loud trio check
// guarantees the CronJob and this RBAC are always rendered together or not at all.
func ControlPlaneRBAC(p Params) RBAC {
	p = p.withDefaults()
	rbac := RBAC{
		ServiceAccounts: []*corev1.ServiceAccount{
			controlPlaneServiceAccount(p.ControlNamespace, SAAPI, ComponentAPI),
			controlPlaneServiceAccount(p.ControlNamespace, SAOperator, ComponentOperator),
		},
		Roles: []*rbacv1.Role{
			APIMinecraftRole(p),
			APIBuildRole(p),
			OperatorRole(p),
		},
		// Each binding lives in the Role's namespace and names the subject SA in the
		// control namespace (a RoleBinding may reference an SA from another namespace;
		// its roleRef must be a Role in the binding's own namespace). The reaper
		// binding below is the one exception: its CronJob runs in the Minecraft
		// namespace, so both the SA and the subject live there.
		RoleBindings: []*rbacv1.RoleBinding{
			bindRole(p.MinecraftNamespace, "felis-api", p.ControlNamespace, SAAPI, ComponentAPI),
			bindRole(p.BuildNamespace, "felis-api-builds", p.ControlNamespace, SAAPI, ComponentAPI),
			bindRole(p.MinecraftNamespace, "felis-operator", p.ControlNamespace, SAOperator, ComponentOperator),
		},
	}
	// The destructive fourth power is conditional on its consumer (see the doc above).
	if reaperEnabled(p) {
		// SAReaper lives in — and its binding subject resolves in — the MINECRAFT
		// namespace, because the reaper CronJob runs there (its backup PVC is there;
		// a Pod can only mount a PVC and use a ServiceAccount from its own namespace).
		rbac.ServiceAccounts = append(rbac.ServiceAccounts,
			controlPlaneServiceAccount(p.MinecraftNamespace, SAReaper, ComponentReaper))
		rbac.Roles = append(rbac.Roles, ReaperRole(p))
		rbac.RoleBindings = append(rbac.RoleBindings,
			bindRole(p.MinecraftNamespace, "felis-reaper", p.MinecraftNamespace, SAReaper, ComponentReaper))
	}
	return rbac
}

// APIMinecraftRole grants felis-api exactly what it does in the minecraft
// namespace: drive MinecraftServer specs (internal/api.k8scluster — get/list/
// create/patch, never status), read RCON passwords for console writes
// (internal/api.console — secrets:get), manage the restore Job under its
// deterministic name (internal/restore — jobs:create, plus get/delete so a
// FINISHED Job whose name still blocks a retry can be replaced), and stream the
// live console for the read side (internal/api.logstream — pods:list to find
// the server's running pod, then pods/log:get to follow it; spec §8 读=pods/log
// follow). It also Gets the world PVC before backup/restore
// (internal/api.k8scluster.WorldVolumeExists) so a never-started or reaped
// world is refused up front instead of leaving a Job Pending on a missing
// claim. felis-api reads the fleet (velocity's pull, the fleet page, the wake
// cap) from an informer cache of minecraftservers, hence watch on that one
// resource; everything else goes through a DIRECT client, so it needs no
// list/watch beyond the explicit List calls. PVC list finds retained world
// volumes before creating a server of the same name.
//
// The read-side grant is deliberately minimal: pods:list + pods/log:get, NOT
// pods:get — the streamer lists pods by the server label then reads the chosen
// pod's log subresource, never Gets a pod object. Keeping pods:get out is the
// least-privilege line the rbac test asserts (a pod's full object can carry more
// than its logs).
func APIMinecraftRole(p Params) *rbacv1.Role {
	p = p.withDefaults()
	rules := []rbacv1.PolicyRule{
		rule([]string{groupFelis}, []string{"minecraftservers"}, []string{"get", "list", "watch", "create", "patch"}),
		rule([]string{groupCore}, []string{"secrets"}, []string{"get"}),
		rule([]string{groupCore}, []string{"persistentvolumeclaims"}, []string{"get", "list"}),
		// list backs GET /servers/{name}/jobs — the async status outlet reads the
		// backup/restore Jobs back by the server label — and finds the pending
		// restore chains; patch settles a chain by relabelling its safety-snapshot
		// Job (internal/api restorechain.go).
		rule([]string{groupBatch}, []string{"jobs"}, []string{"create", "get", "delete", "list", "patch"}),
		// Read-side console (spec §8 读=pods/log follow): list pods to find the
		// server's running pod, then read its log subresource. Two separate rules so
		// the verbs stay tight — list on pods, get on pods/log, and nothing else.
		rule([]string{groupCore}, []string{"pods"}, []string{"list"}),
		rule([]string{groupCore}, []string{"pods/log"}, []string{"get"}),
	}
	if p.Distributed {
		rules = append(rules, rule([]string{groupCore}, []string{"persistentvolumeclaims"}, []string{"create", "list"}), rule([]string{groupApps}, []string{"statefulsets"}, []string{"get", "delete"}))
	}
	return role(p.MinecraftNamespace, "felis-api", ComponentAPI, rules)
}

// APIBuildRole grants felis-api the build-Job lifecycle in the build namespace
// (internal/build.k8sjobs — Create/Get/Delete) plus the read-side build-log
// stream (spec §16, §416 日志流复用 §8): list build Pods to find the build Job's
// Pod by build-id label, then read its log subresource. This is a SEPARATE
// namespace from the api's minecraft powers, so it is a separate Role +
// RoleBinding; the api SA reaches across both from the control namespace. The log
// grant mirrors felis-api's minecraft-ns console read (pods:list + pods/log:get,
// no pods:get) — read-only and least-privilege; it does NOT touch the build SA
// token or any secret.
func APIBuildRole(p Params) *rbacv1.Role {
	p = p.withDefaults()
	return role(p.BuildNamespace, "felis-api-builds", ComponentAPI, []rbacv1.PolicyRule{
		rule([]string{groupBatch}, []string{"jobs"}, []string{"create", "get", "delete"}),
		// Read-side build logs (spec §16): list build Pods to find the build Job's
		// Pod, then read its log subresource — and nothing wider. No pods:get (the
		// streamer lists then reads pods/log, never Gets a Pod object, whose full
		// spec carries more than its logs).
		rule([]string{groupCore}, []string{"pods"}, []string{"list"}),
		rule([]string{groupCore}, []string{"pods/log"}, []string{"get"}),
	})
}

// OperatorRole grants felis-operator what the reconciler exercises through the
// manager's CACHED client (internal/operator.reconciler). Because reads go
// through informers, every watched type needs list+watch even for a single Get;
// the manager's cache is namespace-scoped (see cmd/felis/operator.go), so a
// namespaced Role is sufficient. The operator owns StatefulSets and Services
// (Get/Create/Update — never patch or delete), writes only minecraftservers
// status (Status().Update — `update` only) and patches spec.desiredState to
// Stopped for idle auto-stop (spec §8 — the one spec field it may write, using
// the same merge patch as the reaper's Stop: without the grant the auto-stop
// call fails closed with a 403), and reads RCON Secrets by name, uncached. Jobs are list-only,
// through the manager's uncached API reader: before scaling a server up from zero
// the operator checks that no restore/backup/file-write Job holds its world
// (internal/maintenance). Pods are get and delete, by name through the uncached
// reader: a start that timed out is retried by deleting its pod for the
// StatefulSet to recreate (bounded, three attempts;
// internal/operator.recoverFailedStart), and a pod that is not ready and was made
// from an older template is read and replaced the same way
// (internal/operator.replaceStalePod). Events are create/patch only, for the
// timeline it records on each server. It never touches PVCs or finalizers, so
// none appear here.
func OperatorRole(p Params) *rbacv1.Role {
	p = p.withDefaults()
	return role(p.MinecraftNamespace, "felis-operator", ComponentOperator, []rbacv1.PolicyRule{
		rule([]string{groupFelis}, []string{"minecraftservers"}, []string{"get", "list", "watch", "patch"}),
		rule([]string{groupFelis}, []string{"minecraftservers/status"}, []string{"update"}),
		rule([]string{groupApps}, []string{"statefulsets"}, []string{"get", "list", "watch", "create", "update"}),
		rule([]string{groupCore}, []string{"services"}, []string{"get", "list", "watch", "create", "update"}),
		// get by name, through the uncached API reader (Reconciler.Secrets), of the
		// RCON password Secret a server's spec names; create for the one the operator
		// provisions on first reconcile (internal/operator.ensureRconSecret). No
		// list/watch, so there is no Secret informer and nothing enumerates the
		// namespace's Secrets. No update/delete — the password is written once and
		// removed by garbage collection through its controller reference.
		rule([]string{groupCore}, []string{"secrets"}, []string{"get", "create"}),
		// list only: an uncached List (no informer, so no watch) of the world-volume
		// maintenance Jobs; the operator never creates or deletes a Job.
		rule([]string{groupBatch}, []string{"jobs"}, []string{"list"}),
		// get and delete by name of the one named <server>-0: get through the
		// uncached API reader (Reconciler.Pods), to see whether it is ready and which
		// template revision made it. No list/watch, so there is no pod informer.
		rule([]string{groupCore}, []string{"pods"}, []string{"get", "delete"}),
		// The Events the reconciler records on MinecraftServers (phase changes,
		// pod recreation, idle stop): the recorder creates one and patches its
		// count when the same Event repeats.
		rule([]string{groupCore}, []string{"events"}, []string{"create", "patch"}),
	})
}

// ReaperRole grants felis-reaper its destructive powers
// (internal/reaper.k8scluster): patch a MinecraftServer to Stop it, delete its
// world PVC, and delete the MinecraftServer of a server an admin deleted.
// Candidate servers come from the Postgres store, not a cluster List, so
// minecraftservers need no list/watch; the reaper uses a direct client. It can
// read, patch and delete minecraftservers but cannot create them, and holds no
// power over StatefulSets, Services, or Secrets — those belong to the operator
// and api (deleting a MinecraftServer lets garbage collection take the ones the
// operator made for it; the world PVC is retained by the StatefulSet and the
// reaper deletes it first, after archiving it).
//
// The same patch holds the world maintenance lock while a world is archived and
// reclaimed (internal/maintenance). Taking it needs the two reads felis-api makes
// before a restore: list pods (is the game pod gone) and list jobs (does a
// restore, backup or file write hold the world). Both are list-only.
//
// persistentvolumeclaims also carries get: resolving where a world lives
// (cmd/felis/reaper.resolveWorldDir) reads the PVC's volumeName to derive the
// stock local-path directory name. get is strictly weaker than the delete the
// same rule already grants, so it widens nothing.
//
// The reaper is the only identity with minecraftservers:delete. Reaping an idle
// world releases the server by flipping desiredState=Stopped and reclaiming the
// world PVC, leaving the CR in place so it can be claimed again; the CR goes only
// when an admin deletes the server (PUT /servers/{name}/retirement), and then only
// once the reaper has archived and deleted its world. felis-api records that
// request and never deletes a CR itself.
func ReaperRole(p Params) *rbacv1.Role {
	p = p.withDefaults()
	rules := []rbacv1.PolicyRule{
		rule([]string{groupFelis}, []string{"minecraftservers"}, []string{"get", "patch", "delete"}),
		rule([]string{groupCore}, []string{"persistentvolumeclaims"}, []string{"get", "delete"}),
		rule([]string{groupCore}, []string{"pods"}, []string{"list"}),
		rule([]string{groupBatch}, []string{"jobs"}, []string{"list"}),
	}
	if p.Distributed {
		rules = append(rules, rule([]string{groupBatch}, []string{"jobs"}, []string{"create", "get"}), rule([]string{groupCore}, []string{"persistentvolumeclaims"}, []string{"list"}))
	}
	return role(p.MinecraftNamespace, "felis-reaper", ComponentReaper, rules)
}

// controlPlaneServiceAccount renders a control-plane SA. Unlike the weak
// build/restore SAs, these identities legitimately call the K8s API, so the token
// mounts (via their Deployment) — AutomountServiceAccountToken is left nil
// (cluster default = mount) rather than false.
func controlPlaneServiceAccount(ns, name, component string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: controlPlanePodLabels(component)},
	}
}

func role(ns, name, component string, rules []rbacv1.PolicyRule) *rbacv1.Role {
	return &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: controlPlanePodLabels(component)},
		Rules:      rules,
	}
}

// bindRole binds the Role named roleName (in roleNS) to the ServiceAccount saName
// in saNS. The RoleBinding lives in roleNS; the subject SA may live elsewhere.
func bindRole(roleNS, roleName, saNS, saName, component string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: roleNS, Labels: controlPlanePodLabels(component)},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      saName,
			Namespace: saNS,
		}},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     roleName,
		},
	}
}

func rule(apiGroups, resources, verbs []string) rbacv1.PolicyRule {
	return rbacv1.PolicyRule{APIGroups: apiGroups, Resources: resources, Verbs: verbs}
}
