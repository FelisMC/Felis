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

type Object interface {
	runtime.Object
	metav1.Object
}

func Objects(p Params) []Object {
	p = p.withDefaults()
	var objs []Object

	for _, ns := range distinctNamespaces(p) {
		nsObj := namespaceObject(ns)
		if ns == p.MinecraftNamespace && minecraftNamespaceIsOwn(p) {
			maps.Copy(nsObj.Labels, minecraftPodSecurityLabels)
		}
		objs = append(objs, nsObj)
	}

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

	if p.Distributed {
		objs = append(objs, DistributedRBAC(p)...)
		objs = append(objs, ArchiveNetworkPolicies(p)...)
	}

	// Stamp TypeMeta on weak Job accounts so rendered YAML includes apiVersion and kind.
	objs = append(objs,
		withSATypeMeta(build.BuildServiceAccount(p.BuildNamespace, SABuild)),
		withSATypeMeta(restore.RestoreServiceAccount(p.MinecraftNamespace, SARestore)),
	)

	buildNP := build.BuildNetworkPolicy(build.NetPolParams{
		Namespace:          p.BuildNamespace,
		RegistryNamespace:  p.RegistryNamespace,
		RegistryPort:       p.RegistryPort,
		ControlNamespace:   p.ControlNamespace,
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

	for _, np := range MinecraftNetworkPolicies(p) {
		objs = append(objs, np)
	}
	for _, np := range ServerEgressPolicies(p) {
		objs = append(objs, np)
	}
	objs = append(objs, RegistryIngressPolicy(p))
	for _, np := range ArtifactsNetworkPolicies(p) {
		objs = append(objs, np)
	}

	objs = append(objs, postgresHBAConfig(p), PostgresIngressPolicy(p))

	objs = append(objs, Workloads(p)...)

	return objs
}

func RenderYAML(p Params) ([]byte, error) {
	return RenderObjects(Objects(p))
}

func RenderObjects(objs []Object) ([]byte, error) {
	var buf bytes.Buffer
	for i, obj := range objs {
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

// Minecraft uses PodSecurity baseline: root Jobs retain only allowed capabilities and world hostPaths are accessed through static PVs.
var minecraftPodSecurityLabels = map[string]string{
	"pod-security.kubernetes.io/enforce":         "baseline",
	"pod-security.kubernetes.io/enforce-version": "latest",
	"pod-security.kubernetes.io/warn":            "baseline",
	"pod-security.kubernetes.io/warn-version":    "latest",
	"pod-security.kubernetes.io/audit":           "restricted",
	"pod-security.kubernetes.io/audit-version":   "latest",
}

// Apply baseline only to a dedicated Minecraft namespace; registry hostPorts and build Pods need different admission constraints.
func minecraftNamespaceIsOwn(p Params) bool {
	return p.MinecraftNamespace != p.ControlNamespace &&
		p.MinecraftNamespace != p.BuildNamespace &&
		p.MinecraftNamespace != p.RegistryNamespace
}

func namespaceObject(name string) *corev1.Namespace {
	return &corev1.Namespace{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"kubernetes.io/metadata.name": name},
		},
	}
}

func withSATypeMeta(sa *corev1.ServiceAccount) *corev1.ServiceAccount {
	sa.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}
	return sa
}
