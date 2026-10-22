package platform

import (
	"felis.lolicon.best/internal/apis/felis/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	groupCore  = ""
	groupApps  = "apps"
	groupBatch = "batch"
)

var groupFelis = v1alpha1.GroupName

type RBAC struct {
	ServiceAccounts []*corev1.ServiceAccount
	Roles           []*rbacv1.Role
	RoleBindings    []*rbacv1.RoleBinding
}

// ControlPlaneRBAC renders destructive reaper grants only with their consumer; the API and operator cannot delete world PVCs.
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
			LeaseRole(p, SAAPI, LeaseAPILoops),
			LeaseRole(p, SAOperator, LeaseOperator),
		},
		// Bindings live with their Role, but may reference a ServiceAccount in another namespace.
		RoleBindings: []*rbacv1.RoleBinding{
			bindRole(p.MinecraftNamespace, "felis-api", p.ControlNamespace, SAAPI, ComponentAPI),
			bindRole(p.BuildNamespace, "felis-api-builds", p.ControlNamespace, SAAPI, ComponentAPI),
			bindRole(p.MinecraftNamespace, "felis-operator", p.ControlNamespace, SAOperator, ComponentOperator),
			bindRole(p.ControlNamespace, SAAPI+"-leases", p.ControlNamespace, SAAPI, ComponentAPI),
			bindRole(p.ControlNamespace, SAOperator+"-leases", p.ControlNamespace, SAOperator, ComponentOperator),
		},
	}
	if reaperEnabled(p) {
		// SAReaper must share the Minecraft namespace with its CronJob and mounted backup PVC.
		rbac.ServiceAccounts = append(rbac.ServiceAccounts,
			controlPlaneServiceAccount(p.MinecraftNamespace, SAReaper, ComponentReaper))
		rbac.Roles = append(rbac.Roles, ReaperRole(p))
		rbac.RoleBindings = append(rbac.RoleBindings,
			bindRole(p.MinecraftNamespace, "felis-reaper", p.MinecraftNamespace, SAReaper, ComponentReaper))
	}
	return rbac
}

// APIMinecraftRole permits Pod lists and logs without pods:get; informer-backed MinecraftServer reads additionally need watch.
func APIMinecraftRole(p Params) *rbacv1.Role {
	p = p.withDefaults()
	rules := []rbacv1.PolicyRule{
		rule([]string{groupFelis}, []string{"minecraftservers"}, []string{"get", "list", "watch", "create", "patch"}),
		rule([]string{groupCore}, []string{"secrets"}, []string{"get"}),
		rule([]string{groupCore}, []string{"persistentvolumeclaims"}, []string{"get", "list"}),
		// Job list exposes async status and pending restore chains; patch records chain settlement on the snapshot Job.
		rule([]string{groupBatch}, []string{"jobs"}, []string{"create", "get", "delete", "list", "patch"}),
		rule([]string{groupApps}, []string{"statefulsets"}, []string{"get"}),
		rule([]string{groupApps}, []string{"statefulsets/scale"}, []string{"update"}),
		rule([]string{groupCore}, []string{"pods"}, []string{"list"}),
		rule([]string{groupCore}, []string{"pods/log"}, []string{"get"}),
		rule([]string{"metrics.k8s.io"}, []string{"pods"}, []string{"list"}),
	}
	if p.Distributed {
		rules = append(rules, rule([]string{groupCore}, []string{"persistentvolumeclaims"}, []string{"create", "list"}), rule([]string{groupApps}, []string{"statefulsets"}, []string{"get", "delete"}))
	}
	return role(p.MinecraftNamespace, "felis-api", ComponentAPI, rules)
}

// APIBuildRole lives in the build namespace; its binding references the API account in the control namespace.
func APIBuildRole(p Params) *rbacv1.Role {
	p = p.withDefaults()
	return role(p.BuildNamespace, "felis-api-builds", ComponentAPI, []rbacv1.PolicyRule{
		rule([]string{groupBatch}, []string{"jobs"}, []string{"create", "get", "delete"}),
		rule([]string{groupCore}, []string{"pods"}, []string{"list"}),
		rule([]string{groupCore}, []string{"pods/log"}, []string{"get"}),
	})
}

// OperatorRole follows cached workload reads but uncached Secret, Pod and maintenance-Job reads; it has no PVC or finalizer grants.
func OperatorRole(p Params) *rbacv1.Role {
	p = p.withDefaults()
	return role(p.MinecraftNamespace, "felis-operator", ComponentOperator, []rbacv1.PolicyRule{
		rule([]string{groupFelis}, []string{"minecraftservers"}, []string{"get", "list", "watch", "patch"}),
		rule([]string{groupFelis}, []string{"minecraftservers/status"}, []string{"update"}),
		rule([]string{groupApps}, []string{"statefulsets"}, []string{"get", "list", "watch", "create", "update"}),
		rule([]string{groupCore}, []string{"services"}, []string{"get", "list", "watch", "create", "update"}),
		// Secret get and create are uncached and name-scoped in use; no list, watch, update or delete is needed.
		rule([]string{groupCore}, []string{"secrets"}, []string{"get", "create"}),
		rule([]string{groupBatch}, []string{"jobs"}, []string{"list"}),
		// Pod get and delete use the uncached reader by name for bounded startup retries and stale-template replacement.
		rule([]string{groupCore}, []string{"pods"}, []string{"get", "delete"}),
		rule([]string{groupCore}, []string{"events"}, []string{"create", "patch"}),
	})
}

// ReaperRole alone may delete world PVCs and MinecraftServers after archival; candidate selection comes from Postgres, not a cluster watch.
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

// Control-plane accounts need API tokens; weak build and restore accounts explicitly disable automount.
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

// Leader leases prevent old and replacement controller Pods from acting concurrently after node loss.
const (
	LeaseAPILoops = "felis-api-loops"
	LeaseOperator = "felis-operator"
)

// Lease creation cannot be resource-name-restricted; subsequent reads and renewals target only the identity's lease.
func LeaseRole(p Params, sa, lease string) *rbacv1.Role {
	p = p.withDefaults()
	component := ComponentAPI
	if sa == SAOperator {
		component = ComponentOperator
	}
	return &rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: sa + "-leases", Namespace: p.ControlNamespace, Labels: controlPlanePodLabels(component)},
		Rules: []rbacv1.PolicyRule{
			rule([]string{"coordination.k8s.io"}, []string{"leases"}, []string{"create"}),
			{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, ResourceNames: []string{lease}, Verbs: []string{"get", "update"}},
			rule([]string{groupCore}, []string{"events"}, []string{"create", "patch"}),
		},
	}
}
