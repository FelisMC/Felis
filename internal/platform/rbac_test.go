package platform

import (
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

// testParams is a fully specified Params; it sets VelocityCIDRs so the game policy renders its allow path.
func testParams() Params {
	return Params{
		ControlNamespace:   "felis",
		MinecraftNamespace: "minecraft",
		BuildNamespace:     "felis-build",
		FelisImage:         "registry.felis.svc:5000/felis:test",
		VelocityCIDRs:      []string{"10.0.0.5/32"},
	}
}

func roleByName(t *testing.T, roles []*rbacv1.Role, name string) *rbacv1.Role {
	t.Helper()
	for _, r := range roles {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("role %q not found in bundle", name)
	return nil
}

// hasRule reports whether role grants verb on (apiGroup, resource).
func hasRule(role *rbacv1.Role, apiGroup, resource, verb string) bool {
	for _, r := range role.Rules {
		if !contains(r.APIGroups, apiGroup) || !contains(r.Resources, resource) {
			continue
		}
		if contains(r.Verbs, verb) {
			return true
		}
	}
	return false
}

// grantsResource reports whether role has any rule for (apiGroup, resource).
func grantsResource(role *rbacv1.Role, apiGroup, resource string) bool {
	for _, r := range role.Rules {
		if contains(r.APIGroups, apiGroup) && contains(r.Resources, resource) {
			return true
		}
	}
	return false
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func TestAPIRole_MetricsAreNamespacedAndReadOnly(t *testing.T) {
	r := APIMinecraftRole(testParams())
	if r.Namespace != "minecraft" || !hasRule(r, "metrics.k8s.io", "pods", "list") {
		t.Fatal("API cannot sample game pod usage")
	}
	if grantsResource(r, "metrics.k8s.io", "nodes") {
		t.Fatal("per-server metrics must not require node metrics")
	}
	for _, verb := range []string{"create", "update", "patch", "delete", "watch", "get"} {
		if hasRule(r, "metrics.k8s.io", "pods", verb) {
			t.Fatalf("unneeded metric permission: %s", verb)
		}
	}
}

func TestAPIRole_StdinAttachIsNamespaceScopedCreateOnly(t *testing.T) {
	for _, distributed := range []bool{false, true} {
		p := testParams()
		p.ControlNamespace, p.MinecraftNamespace, p.BuildNamespace = "control-custom", "games-custom", "build-custom"
		p.Distributed = distributed
		grants := 0
		for _, role := range ControlPlaneRBAC(p).Roles {
			if grantsResource(role, groupCore, "pods/exec") {
				t.Errorf("%s must not execute arbitrary container commands", role.Name)
			}
			for _, rule := range role.Rules {
				if !contains(rule.Resources, "pods/attach") {
					continue
				}
				grants++
				if role.Name != "felis-api" || role.Kind != "Role" || role.Namespace != p.MinecraftNamespace ||
					len(rule.APIGroups) != 1 || rule.APIGroups[0] != groupCore ||
					len(rule.Resources) != 1 || len(rule.Verbs) != 1 || rule.Verbs[0] != "create" {
					t.Errorf("distributed=%v: excessive attach grant in %s/%s: %+v", distributed, role.Namespace, role.Name, rule)
				}
			}
		}
		if grants != 1 {
			t.Errorf("distributed=%v: got %d attach grants, want only the game API role", distributed, grants)
		}
	}
}

func TestAPIRole_CreatesJobsInBothNamespaces(t *testing.T) {
	rbac := ControlPlaneRBAC(testParams())

	mc := roleByName(t, rbac.Roles, "felis-api")
	if mc.Namespace != "minecraft" {
		t.Errorf("felis-api minecraft Role namespace = %q, want minecraft", mc.Namespace)
	}
	for _, v := range []string{"create", "get", "delete", "list", "patch"} {
		if !hasRule(mc, "batch", "jobs", v) {
			t.Errorf("felis-api (minecraft) must have batch/jobs:%s for the restore-Job lifecycle", v)
		}
	}

	build := roleByName(t, rbac.Roles, "felis-api-builds")
	if build.Namespace != "felis-build" {
		t.Errorf("felis-api-builds Role namespace = %q, want felis-build", build.Namespace)
	}
	for _, v := range []string{"create", "get", "delete"} {
		if !hasRule(build, "batch", "jobs", v) {
			t.Errorf("felis-api-builds must have batch/jobs:%s for the build-Job lifecycle", v)
		}
	}
	// Build logs: pods:list to find the build pod, then pods/log; never pods:get, since a pod object can carry more than its logs.
	if !hasRule(build, groupCore, "pods", "list") {
		t.Error("felis-api-builds must list pods (pods:list) to find the build Job's pod for the §16 build-log read")
	}
	if !hasRule(build, groupCore, "pods/log", "get") {
		t.Error("felis-api-builds must read pod logs (pods/log:get) for the §16 build-log stream")
	}
	if hasRule(build, groupCore, "pods", "get") {
		t.Error("felis-api-builds must NOT have pods:get (the build-log streamer lists then reads pods/log, never Gets a pod)")
	}
}

func TestAPIRole_MinecraftPowersExact(t *testing.T) {
	mc := roleByName(t, ControlPlaneRBAC(testParams()).Roles, "felis-api")
	// watch backs the informer cache the fleet reads come from.
	for _, v := range []string{"get", "list", "watch", "create", "patch"} {
		if !hasRule(mc, groupFelis, "minecraftservers", v) {
			t.Errorf("felis-api must have minecraftservers:%s", v)
		}
	}
	if hasRule(mc, groupFelis, "minecraftservers", "delete") {
		t.Error("felis-api must NOT delete minecraftservers (lifecycle is the reaper/operator's)")
	}
	if grantsResource(mc, groupFelis, "minecraftservers/status") {
		t.Error("felis-api must NOT touch minecraftservers/status (status is the operator's alone)")
	}
	if !hasRule(mc, groupCore, "secrets", "get") {
		t.Error("felis-api must read RCON secrets (secrets:get) for console writes")
	}
	// The informer cache covers minecraftservers only; RCON secrets stay a direct
	// Get by name, so no watch or list ever mirrors every secret into felis-api.
	if hasRule(mc, groupCore, "secrets", "list") || hasRule(mc, groupCore, "secrets", "watch") {
		t.Error("felis-api must NOT list or watch secrets (RCON reads are a direct Get by name)")
	}
	if !hasRule(mc, groupApps, "statefulsets", "get") || !hasRule(mc, groupApps, "statefulsets/scale", "update") {
		t.Error("API needs read and scale-only emergency shutdown grants")
	}
	for _, verb := range []string{"patch", "update", "delete"} {
		if hasRule(mc, groupApps, "statefulsets", verb) {
			t.Errorf("API must not mutate StatefulSets directly: %s", verb)
		}
	}
	// WorldVolumeExists reads a claim and lists retained claims on server creation.
	if !hasRule(mc, groupCore, "persistentvolumeclaims", "get") {
		t.Error("felis-api must get the world PVC (persistentvolumeclaims:get) for the backup/restore world-volume gate")
	}
	if !hasRule(mc, groupCore, "persistentvolumeclaims", "list") {
		t.Error("felis-api must list retained PVCs before creating a server")
	}
	if hasRule(mc, groupCore, "persistentvolumeclaims", "watch") || hasRule(mc, groupCore, "persistentvolumeclaims", "delete") {
		t.Error("felis-api must NOT watch or delete PVCs")
	}
	// Console reads: pods:list to find the pod, then pods/log only.
	if !hasRule(mc, groupCore, "pods", "list") {
		t.Error("felis-api must list pods (pods:list) to find a server's running pod for the console read")
	}
	if !hasRule(mc, groupCore, "pods/log", "get") {
		t.Error("felis-api must read pod logs (pods/log:get) for the console read (spec §8 读=pods/log follow)")
	}
	// Least privilege: the streamer lists pods and reads pods/log, never a pod object, so pods:get stays absent.
	if hasRule(mc, groupCore, "pods", "get") {
		t.Error("felis-api must NOT have pods:get (the console streamer lists then reads pods/log, never Gets a pod)")
	}
}

func TestOperatorRole_ScopeExact(t *testing.T) {
	op := roleByName(t, ControlPlaneRBAC(testParams()).Roles, "felis-operator")

	// Watched types need list+watch because reads go through informers.
	for _, v := range []string{"get", "list", "watch"} {
		if !hasRule(op, groupFelis, "minecraftservers", v) {
			t.Errorf("operator must have minecraftservers:%s (cached client)", v)
		}
	}
	// Idle auto-stop patches spec.desiredState=Stopped; without this grant it fails with 403.
	if !hasRule(op, groupFelis, "minecraftservers", "patch") {
		t.Error("operator must have minecraftservers:patch (idle auto-stop writes spec.desiredState)")
	}
	for _, v := range []string{"get", "list", "watch", "create", "update"} {
		if !hasRule(op, "apps", "statefulsets", v) {
			t.Errorf("operator must have statefulsets:%s", v)
		}
	}
	// Owns workloads by create/update only — never patch or delete.
	for _, v := range []string{"patch", "delete"} {
		if hasRule(op, "apps", "statefulsets", v) {
			t.Errorf("operator must NOT have statefulsets:%s (create/update only)", v)
		}
	}
	// Status is update-only.
	if !hasRule(op, groupFelis, "minecraftservers/status", "update") {
		t.Error("operator must have minecraftservers/status:update")
	}
	for _, v := range []string{"get", "patch"} {
		if hasRule(op, groupFelis, "minecraftservers/status", v) {
			t.Errorf("operator status rule must be update-only, found %s", v)
		}
	}
	if !hasRule(op, groupCore, "persistentvolumeclaims", "get") {
		t.Error("operator must read the world PVC identity before applying a disk sample")
	}
	for _, verb := range []string{"list", "watch", "create", "update", "patch", "delete"} {
		if hasRule(op, groupCore, "persistentvolumeclaims", verb) {
			t.Errorf("operator volume identity access must be get-only, found %s", verb)
		}
	}
	// Events: the recorder creates one and patches its count on a repeat.
	for _, v := range []string{"create", "patch"} {
		if !hasRule(op, groupCore, "events", v) {
			t.Errorf("operator must have events:%s (the server timeline)", v)
		}
	}
	for _, v := range []string{"get", "list", "watch", "update", "delete", "deletecollection", "*"} {
		if hasRule(op, groupCore, "events", v) {
			t.Errorf("operator events rule must be create+patch only, found %s", v)
		}
	}
	// RCON Secrets are read by name through the uncached reader and created once;
	// no list/watch, so no informer mirrors the namespace's Secrets into it.
	for _, v := range []string{"get", "create"} {
		if !hasRule(op, groupCore, "secrets", v) {
			t.Errorf("operator must have secrets:%s", v)
		}
	}
	for _, v := range []string{"list", "watch", "update", "patch", "delete", "deletecollection", "*"} {
		if hasRule(op, groupCore, "secrets", v) {
			t.Errorf("operator secrets rule must be get+create only, found %s", v)
		}
	}
	// Pods are read and deleted by name: the bounded retry of a timed-out start,
	// and the replacement of a not-ready pod left on an old template.
	if !hasRule(op, groupCore, "pods", "delete") {
		t.Error("operator must have pods:delete (auto-restart of a timed-out start)")
	}
	if !hasRule(op, groupCore, "pods", "get") {
		t.Error("operator must have pods:get (replacing a not-ready pod left on an old template)")
	}
	for _, v := range []string{"list", "watch", "create", "update", "patch", "deletecollection", "*"} {
		if hasRule(op, groupCore, "pods", v) {
			t.Errorf("operator pods rule must be get+delete only, found %s", v)
		}
	}
	// The world-volume lock check lists Jobs uncached; it never writes one.
	if !hasRule(op, groupBatch, "jobs", "list") {
		t.Error("operator must have jobs:list (maintenance hold before scale-up)")
	}
	for _, v := range []string{"get", "watch", "create", "update", "patch", "delete"} {
		if hasRule(op, groupBatch, "jobs", v) {
			t.Errorf("operator must NOT have jobs:%s (list-only)", v)
		}
	}
}

func TestReaperRole_ScopeExact(t *testing.T) {
	rp := roleByName(t, ControlPlaneRBAC(reaperParams()).Roles, "felis-reaper")

	if !hasRule(rp, groupCore, "persistentvolumeclaims", "delete") || !hasRule(rp, groupCore, "persistentvolumeclaims", "get") {
		t.Error("reaper must get (resolve the volume) and delete (reclaim) PVCs")
	}
	if !hasRule(rp, groupFelis, "minecraftservers", "patch") || !hasRule(rp, groupFelis, "minecraftservers", "get") {
		t.Error("reaper must get+patch minecraftservers (to Stop them)")
	}
	if hasRule(rp, groupFelis, "minecraftservers", "create") {
		t.Error("reaper must NOT create minecraftservers")
	}
	for _, res := range []struct{ group, name string }{
		{"apps", "statefulsets"}, {groupCore, "services"}, {groupCore, "secrets"},
	} {
		if grantsResource(rp, res.group, res.name) {
			t.Errorf("reaper must NOT touch %s/%s (operator/api territory)", res.group, res.name)
		}
	}
	// The reaper never lists servers from the cluster (candidates come from Postgres).
	for _, v := range []string{"list", "watch"} {
		if hasRule(rp, groupFelis, "minecraftservers", v) {
			t.Errorf("reaper must NOT %s minecraftservers (candidates come from the store)", v)
		}
	}
	// Taking the world lock reads the game pod and the maintenance Jobs, list only.
	if !hasRule(rp, groupCore, "pods", "list") || !hasRule(rp, groupBatch, "jobs", "list") {
		t.Error("reaper must list pods and jobs to take the world maintenance lock")
	}
	for _, res := range []struct{ group, name string }{{groupCore, "pods"}, {groupBatch, "jobs"}} {
		for _, v := range []string{"get", "watch", "create", "update", "patch", "delete"} {
			if hasRule(rp, res.group, res.name, v) {
				t.Errorf("reaper must NOT %s %s (list-only)", v, res.name)
			}
		}
	}
}

func TestReaperRBAC_GatedOnRetention(t *testing.T) {
	hasSA := func(rbac RBAC, name string) bool {
		for _, sa := range rbac.ServiceAccounts {
			if sa.Name == name {
				return true
			}
		}
		return false
	}
	reaperRoleBindings := func(rbac RBAC) (roles, bindings int) {
		for _, r := range rbac.Roles {
			if r.Name == "felis-reaper" {
				roles++
			}
		}
		for _, rb := range rbac.RoleBindings {
			if rb.Name == "felis-reaper" {
				bindings++
			}
		}
		return
	}

	// No retention storage ⇒ no reaper identity anywhere in the bundle.
	off := ControlPlaneRBAC(testParams())
	if hasSA(off, SAReaper) {
		t.Error("felis-reaper SA must NOT render without the retention trio")
	}
	if roles, bindings := reaperRoleBindings(off); roles != 0 || bindings != 0 {
		t.Errorf("reaper Role/RoleBinding present without retention (roles=%d bindings=%d)", roles, bindings)
	}
	// The bundle holds no pvc:delete grant at all.
	for _, role := range off.Roles {
		if hasRule(role, groupCore, "persistentvolumeclaims", "delete") {
			t.Errorf("%s grants pvc:delete with no reaper deployed — the destructive grant must be gated", role.Name)
		}
	}

	// Full trio ⇒ the SA, its Role, and the binding all render together.
	on := ControlPlaneRBAC(reaperParams())
	if !hasSA(on, SAReaper) {
		t.Error("felis-reaper SA must render with the retention trio")
	}
	if roles, bindings := reaperRoleBindings(on); roles != 1 || bindings != 1 {
		t.Errorf("want exactly 1 reaper Role + 1 binding with retention, got roles=%d bindings=%d", roles, bindings)
	}
}

func TestOnlyReaperDeletesMinecraftServers(t *testing.T) {
	for _, role := range ControlPlaneRBAC(testParams()).Roles {
		if hasRule(role, groupFelis, "minecraftservers", "delete") {
			t.Errorf("%s grants minecraftservers:delete with no reaper deployed", role.Name)
		}
	}
	reaper := 0
	for _, role := range ControlPlaneRBAC(reaperParams()).Roles {
		if !hasRule(role, groupFelis, "minecraftservers", "delete") {
			continue
		}
		if role.Name != "felis-reaper" {
			t.Errorf("%s grants minecraftservers:delete — only the reaper removes a server, after archiving its world", role.Name)
			continue
		}
		reaper++
	}
	if reaper != 1 {
		t.Error("the reaper must delete minecraftservers (an admin's deletion of a server)")
	}
}

func TestNoClusterScopedRBAC(t *testing.T) {
	rbac := ControlPlaneRBAC(testParams())

	dangerousVerbs := map[string]bool{"escalate": true, "bind": true, "impersonate": true}
	rbacResources := map[string]bool{
		"roles": true, "rolebindings": true, "clusterroles": true, "clusterrolebindings": true,
	}

	for _, role := range rbac.Roles {
		if role.Kind != "Role" {
			t.Errorf("%s: Kind = %q, want Role (no ClusterRole)", role.Name, role.Kind)
		}
		if role.Namespace == "" {
			t.Errorf("%s: Role must be namespaced", role.Name)
		}
		for _, r := range role.Rules {
			for _, g := range r.APIGroups {
				if g == "*" {
					t.Errorf("%s: wildcard apiGroup", role.Name)
				}
			}
			for _, res := range r.Resources {
				if res == "*" {
					t.Errorf("%s: wildcard resource", role.Name)
				}
				if rbacResources[strings.ToLower(res)] {
					t.Errorf("%s: must not grant power over RBAC resource %q", role.Name, res)
				}
			}
			for _, v := range r.Verbs {
				if v == "*" {
					t.Errorf("%s: wildcard verb", role.Name)
				}
				if dangerousVerbs[strings.ToLower(v)] {
					t.Errorf("%s: dangerous verb %q", role.Name, v)
				}
			}
		}
	}
}

func TestRoleBindings_RefLocalRoleAndControlPlaneSA(t *testing.T) {
	p := testParams()
	rbac := ControlPlaneRBAC(p)
	roleNames := map[string]bool{}
	for _, r := range rbac.Roles {
		roleNames[r.Namespace+"/"+r.Name] = true
	}

	for _, rb := range rbac.RoleBindings {
		if rb.RoleRef.Kind != "Role" {
			t.Errorf("%s: RoleRef.Kind = %q, want Role", rb.Name, rb.RoleRef.Kind)
		}
		if rb.RoleRef.APIGroup != rbacv1.GroupName {
			t.Errorf("%s: RoleRef.APIGroup = %q, want %q", rb.Name, rb.RoleRef.APIGroup, rbacv1.GroupName)
		}
		// The referenced Role must exist in the binding's own namespace.
		if !roleNames[rb.Namespace+"/"+rb.RoleRef.Name] {
			t.Errorf("%s: references Role %q absent from namespace %q", rb.Name, rb.RoleRef.Name, rb.Namespace)
		}
		if len(rb.Subjects) == 0 {
			t.Fatalf("%s: no subjects", rb.Name)
		}
		for _, s := range rb.Subjects {
			if s.Kind != rbacv1.ServiceAccountKind {
				t.Errorf("%s: subject Kind = %q, want ServiceAccount", rb.Name, s.Kind)
			}
			if s.Namespace != p.ControlNamespace {
				t.Errorf("%s: subject SA namespace = %q, want control ns %q", rb.Name, s.Namespace, p.ControlNamespace)
			}
		}
	}
}

func TestLeaseRolesAreNarrow(t *testing.T) {
	p := testParams()
	rbac := ControlPlaneRBAC(p)
	for sa, lease := range map[string]string{SAAPI: LeaseAPILoops, SAOperator: LeaseOperator} {
		role := roleByName(t, rbac.Roles, sa+"-leases")
		if role.Namespace != p.ControlNamespace || len(role.Rules) != 3 {
			t.Fatalf("%s role = %+v", sa, role)
		}
		for _, r := range role.Rules {
			switch {
			case contains(r.Resources, "leases") && len(r.ResourceNames) == 0:
				if len(r.Verbs) != 1 || r.Verbs[0] != "create" {
					t.Errorf("%s unnamed lease verbs = %v", sa, r.Verbs)
				}
			case contains(r.Resources, "leases"):
				if len(r.ResourceNames) != 1 || r.ResourceNames[0] != lease || len(r.Verbs) != 2 || !contains(r.Verbs, "get") || !contains(r.Verbs, "update") {
					t.Errorf("%s named lease rule = %+v", sa, r)
				}
			case contains(r.Resources, "events"):
				if len(r.Verbs) != 2 || !contains(r.Verbs, "create") || !contains(r.Verbs, "patch") {
					t.Errorf("%s event verbs = %v", sa, r.Verbs)
				}
			default:
				t.Errorf("%s has an unexpected rule %+v", sa, r)
			}
		}
		bound := false
		for _, rb := range rbac.RoleBindings {
			if rb.RoleRef.Name == role.Name {
				bound = len(rb.Subjects) == 1 && rb.Subjects[0].Name == sa && rb.Namespace == p.ControlNamespace
			}
		}
		if !bound {
			t.Errorf("%s lease role is not bound to %s alone", sa, sa)
		}
	}
}
